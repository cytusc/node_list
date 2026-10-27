package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/testutil"
)

func fixture(t *testing.T) (*Repository, Converter) {
	t.Helper()
	r := &Repository{Pool: testutil.Database(t)}
	if err := r.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := DefaultConverter()
	c.Script = filepath.Join("..", "..", "..", "services", "node-source-converter", "converter.mjs")
	return r, c
}
func configuration(port int) domain.ProxyConfig {
	return domain.ProxyConfig{"name": "alias", "type": "http", "server": "192.0.2.1", "port": port}
}
func convert(t *testing.T, c Converter, configs ...domain.ProxyConfig) Conversion {
	t.Helper()
	items := []sourceItem{}
	for _, config := range configs {
		items = append(items, sourceItem{Name: "alias", RawConfig: config})
	}
	v, err := c.Convert(context.Background(), map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func count(t *testing.T, r *Repository, sql string) int {
	t.Helper()
	var n int
	if err := r.Pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func save(t *testing.T, r *Repository, source, digest string, v Conversion, want error) {
	t.Helper()
	if err := r.Save(context.Background(), source, digest, v); !errors.Is(err, want) {
		t.Fatalf("save: %v want %v", err, want)
	}
}
func TestConcurrentReplayAndSourceMembership(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	v := convert(t, c, configuration(80))
	other := &Repository{Pool: r.Pool}
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			repo := r
			if n%2 == 0 {
				repo = other
			}
			errs <- repo.Save(ctx, "one", "a", v)
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count(t, r, "SELECT count(*) FROM node_explorer_node_registry") != 1 || count(t, r, "SELECT count(*) FROM node_explorer_source_batches") != 1 {
		t.Fatal("concurrent duplicate")
	}
	v.Nodes[0].Name = "second alias"
	save(t, r, "two", "a", v, nil)
	if count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences") != 2 || count(t, r, "SELECT count(*) FROM node_explorer_source_links") != 2 {
		t.Fatal("lost source evidence")
	}
	partial := convert(t, c, configuration(81), domain.ProxyConfig{"type": "trojan", "server": "x", "port": 443})
	save(t, r, "one", "b", partial, ErrPartialBatch)
	save(t, r, "one", "b", partial, ErrPartialBatch)
	if count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences WHERE source='one' AND current") != 2 {
		t.Fatal("partial removed previous members")
	}
	save(t, r, "one", "c", convert(t, c, configuration(82)), nil)
	if count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences WHERE source='one' AND current") != 1 || count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences WHERE source='two' AND current") != 1 {
		t.Fatal("source replacement leaked")
	}
	save(t, r, "one", "a", v, nil)
	if count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences WHERE source='one' AND digest='a' AND current") != 1 {
		t.Fatal("replay not reactivated")
	}
	rejected := convert(t, c, domain.ProxyConfig{"type": "trojan"})
	save(t, r, "one", "d", rejected, ErrNoAdmissibleNodes)
	save(t, r, "one", "empty", Conversion{Version: converterVersion}, ErrNoAdmissibleNodes)
	if count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences WHERE source='one' AND current") != 1 {
		t.Fatal("rejection cleared source")
	}
	if count(t, r, "SELECT count(*) FROM node_explorer_node_registry WHERE available") != 0 {
		t.Fatal("source marked available")
	}
}
func TestLegacyAdoptionPreservesFacts(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	config := configuration(8080)
	raw, _ := json.Marshal(domain.Node{Name: "old", RawConfig: config})
	_, err := r.Pool.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,available) VALUES('legacy',$1,true)`, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO node_explorer_connectivity VALUES('legacy',now(),'{"reachable":true}')`,
		`INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES('legacy','192.0.2.9',now(),'{"test":true,"verified":true}')`,
		`INSERT INTO node_explorer_ip_attributes VALUES('192.0.2.9',now(),'{"test":true}')`,
	} {
		if _, err = r.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	facts := func() string {
		var got string
		err := r.Pool.QueryRow(ctx, `SELECT jsonb_build_object('available',r.available,'incarnation',r.incarnation,'last',r.last_available_at,'node',r.node,'connectivity',(SELECT to_jsonb(c) FROM node_explorer_connectivity c WHERE identity=r.identity),'exit',(SELECT to_jsonb(e) FROM node_explorer_exits e WHERE identity=r.identity),'attribute',(SELECT to_jsonb(a) FROM node_explorer_ip_attributes a WHERE ip='192.0.2.9'))::text FROM node_explorer_node_registry r WHERE identity='legacy'`).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := facts()
	v := convert(t, c, config)
	save(t, r, "source", "a", v, nil)
	if facts() != before {
		t.Fatal("source mutated other service facts")
	}
	if count(t, r, "SELECT count(*) FROM node_explorer_source_identity_aliases") != 1 || count(t, r, "SELECT count(*) FROM node_explorer_node_registry") != 1 {
		t.Fatal("legacy not adopted")
	}
	if count(t, r, "SELECT count(*) FROM node_explorer_live_nodes WHERE identity='legacy'") != 1 {
		t.Fatal("adopted node not visible directly from registry")
	}
	save(t, r, "source", "b", v, nil)
	if facts() != before {
		t.Fatal("repeat changed current facts")
	}
	// Two identical historical rows must not be silently merged or selected arbitrarily.
	raw, _ = json.Marshal(domain.Node{RawConfig: configuration(9090)})
	_, err = r.Pool.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node) VALUES('old-a',$1),('old-b',$1)`, raw)
	if err != nil {
		t.Fatal(err)
	}
	save(t, r, "source", "c", convert(t, c, configuration(9090)), nil)
	if count(t, r, "SELECT count(*) FROM node_explorer_node_registry") != 4 {
		t.Fatal("ambiguous legacy adopted")
	}
}
func TestCollisionRollsBackEntireBatch(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	v := convert(t, c, configuration(80))
	save(t, r, "source", "first", v, nil)
	if _, err := r.Pool.Exec(ctx, "UPDATE node_explorer_source_connections SET canonical='tampered'"); err != nil {
		t.Fatal(err)
	}
	before := count(t, r, "SELECT count(*) FROM node_explorer_source_batches")
	v = convert(t, c, configuration(81), configuration(80))
	if err := r.Save(ctx, "source", "collision", v); err == nil {
		t.Fatal("collision accepted")
	}
	if count(t, r, "SELECT count(*) FROM node_explorer_source_batches") != before || count(t, r, "SELECT count(*) FROM node_explorer_node_registry") != 1 || count(t, r, "SELECT count(*) FROM node_explorer_source_batches") != 1 || count(t, r, "SELECT count(*) FROM node_explorer_source_occurrences WHERE current") != 1 {
		t.Fatal("partial transaction persisted")
	}
}
func TestIndependentServiceAndConverterFailures(t *testing.T) {
	r, c := fixture(t)
	t.Setenv("SOURCE_PROVIDERS", "none")
	t.Setenv("SOURCE_SUBSCRIPTIONS", "")
	t.Setenv("SOURCE_CONVERTER", c.Script)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.RunSource(ctx) }()
	deadline := time.After(5 * time.Second)
	for r.Healthy(context.Background()) != nil {
		select {
		case err := <-done:
			t.Fatalf("service exited: %v", err)
		case <-deadline:
			t.Fatal("no heartbeat")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop")
	}
	if r.Healthy(context.Background()) == nil {
		t.Fatal("stopped service healthy")
	}
	script := filepath.Join(t.TempDir(), "failure.mjs")
	os.WriteFile(script, []byte("console.error('secret-password');process.exit(1)"), 0600)
	c.Script = script
	if _, err := c.Convert(context.Background(), map[string]any{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("converter error leaked")
	}
	os.WriteFile(script, []byte("setTimeout(()=>{},10000)"), 0600)
	ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, err := c.Convert(ctx, map[string]any{}); err == nil {
		t.Fatal("converter ignored cancellation")
	}
}

func TestImporterIgnoresUpstreamFactsAndRetainsOriginal(t *testing.T) {
	r, c := fixture(t)
	ctx := context.Background()
	in := &Importer{Repository: r, Converter: c, Source: "panel"}
	link := "http://user:pass@192.0.2.1:8080#original"
	snap := domain.SourceBatch{CheckedAt: "first", Nodes: []domain.Node{{Name: "original", RawLink: link, Country: "untrusted", IP: "192.0.2.9", Speed: 999}}}
	if err := in.Import(ctx, snap); err != nil {
		t.Fatal(err)
	}
	snap.CheckedAt = "second"
	if err := in.Import(ctx, snap); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	var available bool
	if err := r.Pool.QueryRow(ctx, "SELECT node,available FROM node_explorer_node_registry").Scan(&payload, &available); err != nil {
		t.Fatal(err)
	}
	var node domain.Node
	if err := json.Unmarshal(payload, &node); err != nil {
		t.Fatal(err)
	}
	if available || node.RawLink != link || node.IP != "" || node.Country != "" || node.Speed != 0 || count(t, r, "SELECT count(*) FROM node_explorer_source_batches") != 1 {
		t.Fatal("upstream facts imported or repeat not idempotent")
	}
	for _, table := range []string{"node_explorer_connectivity", "node_explorer_exits", "node_explorer_ip_attributes"} {
		if count(t, r, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("source wrote " + table)
		}
	}
}
