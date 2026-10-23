package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/sqlite"
	"github.com/cytusc/node_list/backend/internal/transport"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type waveEngine struct {
	released atomic.Int32
	received atomic.Int32
}

func (*waveEngine) Name() string         { return "test" }
func (*waveEngine) Supports(string) bool { return true }
func (e *waveEngine) Reserve(ctx context.Context, nodes []domain.Node) (map[int]*transport.Outbound, func(), error) {
	e.received.Store(int32(len(nodes)))
	out := make(map[int]*transport.Outbound)
	for i := range nodes {
		ip := fmt.Sprintf("2001:db8::%d", i+1)
		out[i] = &transport.Outbound{Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(ip)), Header: make(http.Header)}, nil
		})}}
	}
	return out, func() { e.released.Add(1) }, nil
}

func TestUnsupportedShareLinkDoesNotRejectOtherNodes(t *testing.T) {
	ctx := context.Background()
	adapter, err := sqlite.New(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	nodes := []domain.Node{
		{Name: "unsupported", Type: "ss", RawLink: "ss://invalid"},
		{Name: "valid", Type: "ss", RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.1", "port": 443, "cipher": "aes-128-gcm", "password": "secret"}},
	}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: nodes})
	if err := adapter.Insert(ctx, "one", raw, "", len(nodes)); err != nil {
		t.Fatal(err)
	}
	st := store.New(adapter, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	engine := &waveEngine{}
	scheduler, err := NewScheduler(st, transport.NewRouter(engine), SchedulerConfig{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := scheduler.buildQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.runJobs(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if engine.received.Load() != 1 {
		t.Fatal("unsupported share link reached the engine")
	}
	rows, err := adapter.Enrichments(ctx, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("enrichments=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		var result domain.IPPureResult
		if err := json.Unmarshal(row.Payload, &result); err != nil {
			t.Fatal(err)
		}
		if row.NodeIndex == 0 && (result.FailureStage != "share-link" || result.ShareLinkVersion != domain.ShareLinkVersion) {
			t.Fatalf("unsupported result=%+v", result)
		}
		if row.NodeIndex == 1 && result.IP != "2001:db8::1" {
			t.Fatalf("valid node did not get its own probe result: %+v", result)
		}
	}
	jobs, err = scheduler.buildQueue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.index == 0 {
			t.Fatal("unchanged unsupported share link was queued again")
		}
	}
}

type failingSaveAdapter struct{ store.Adapter }

func (a failingSaveAdapter) SaveIPPure(context.Context, int, int, []byte, string) error {
	return errors.New("database write failed")
}

func TestWaveUsesLocalIndexAndPreservesPersistenceIndex(t *testing.T) {
	ctx := context.Background()
	adapter, err := sqlite.New(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	nodes := make([]domain.Node, 6)
	for i := range nodes {
		nodes[i] = domain.Node{Name: fmt.Sprint(i), Type: "ss", Server: fmt.Sprintf("192.0.2.%d", i+1), Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": fmt.Sprintf("192.0.2.%d", i+1), "port": 443}}
	}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: nodes})
	if err := adapter.Insert(ctx, "one", raw, "", len(nodes)); err != nil {
		t.Fatal(err)
	}
	st := store.New(adapter, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	engine := &waveEngine{}
	scheduler, err := NewScheduler(st, transport.NewRouter(engine), SchedulerConfig{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	jobs := []job{{snapshotID: 1, index: 5, node: nodes[5], identity: domain.AggregateIdentity(nodes[5])}, {snapshotID: 1, index: 2, node: nodes[2], identity: domain.AggregateIdentity(nodes[2]), knownDead: true}}
	if err := scheduler.runJobs(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	rows, err := adapter.Enrichments(ctx, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	expected := map[int]string{5: "2001:db8::1", 2: "2001:db8::2"}
	for _, row := range rows {
		var result domain.IPPureResult
		if err := json.Unmarshal(row.Payload, &result); err != nil {
			t.Fatal(err)
		}
		if result.IP != expected[row.NodeIndex] {
			t.Fatalf("index %d received IP %s", row.NodeIndex, result.IP)
		}
	}
	if scheduler.Status().Probed != 2 || engine.released.Load() != 1 {
		t.Fatal("fast and slow jobs were not both drained and released")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := scheduler.runWave(cancelled, engine, jobs, 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation hidden: %v", err)
	}
	broken := store.New(failingSaveAdapter{Adapter: adapter}, nil)
	failing, err := NewScheduler(broken, transport.NewRouter(engine), SchedulerConfig{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.runWave(ctx, engine, jobs, 1, 0); err == nil || !strings.Contains(err.Error(), "database write failed") {
		t.Fatalf("persistence failure hidden: %v", err)
	}
}

func TestQueueDeduplicatesConfigNodesAcrossHistory(t *testing.T) {
	ctx := context.Background()
	adapter, err := sqlite.New(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	st := store.New(adapter, nil)
	importer := store.NewIngestor(st)
	node := domain.Node{Name: "same", Type: "ss", Server: "192.0.2.1", Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.1", "port": 443, "cipher": "aes-128-gcm", "password": "secret"}}
	for _, checked := range []string{"2026-10-14T10:00:00Z", "2026-10-14T11:00:00Z"} {
		if err := importer.Import(ctx, domain.Snapshot{CheckedAt: checked, Nodes: []domain.Node{node}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	scheduler, err := NewScheduler(st, transport.NewRouter(&waveEngine{}), SchedulerConfig{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := scheduler.buildQueue(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].snapshotID != 2 {
		t.Fatalf("duplicate config probes: %+v %v", jobs, err)
	}
}

func TestQueueBackfillsMissingTrafficRatioAndThrottlesRetries(t *testing.T) {
	ctx := context.Background()
	adapter, err := sqlite.New(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	st := store.New(adapter, nil)
	importer := store.NewIngestor(st)
	node := domain.Node{Name: "ratio", Type: "ss", Server: "192.0.2.1", Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.1", "port": 443, "cipher": "aes-128-gcm", "password": "secret"}}
	if err := importer.Import(ctx, domain.Snapshot{CheckedAt: "2026-10-14T10:00:00Z", Nodes: []domain.Node{node}}, nil); err != nil {
		t.Fatal(err)
	}
	old := domain.IPPureResult{
		Status: "success", IP: "118.168.211.221", ExitIPSource: "IP.SB", ExitIPMode: "proxy-echo",
		ExitIPVersion: domain.ExitIPVersion, ExitIPProbeVersion: domain.ExitIPProbeVersion,
		QueryMode: "specified-exit", TransportMode: "http", SignatureVersion: ippureSignatureVersion,
		Score: 3, Label: "极度纯净", CheckedAt: "2026-10-14T10:00:00Z",
	}
	if err := st.SaveIPPure(1, 0, old, domain.AggregateIdentity(node)); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(st, transport.NewRouter(&waveEngine{}), SchedulerConfig{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := scheduler.buildQueue(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("missing ratio was not queued: jobs=%+v err=%v", jobs, err)
	}
	if !jobs[0].trafficRatioBackfill || !jobs[0].reuseExit || jobs[0].exitIP != old.IP {
		t.Fatalf("ratio backfill did not reuse the verified exit: %+v", jobs[0])
	}

	old.TrafficRatioCheckedAt = store.NowTimestamp()
	if err := st.SaveIPPure(1, 0, old, domain.AggregateIdentity(node)); err != nil {
		t.Fatal(err)
	}
	if saved := st.GetCollectionContext().AggregateIP[domain.AggregateIdentity(node)]; saved == nil || saved.TrafficRatioCheckedAt != old.TrafficRatioCheckedAt {
		t.Fatalf("ratio attempt timestamp did not survive persistence: saved=%+v want=%q", saved, old.TrafficRatioCheckedAt)
	}
	jobs, err = scheduler.buildQueue(ctx)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("recent ratio attempt was not throttled: jobs=%+v err=%v", jobs, err)
	}
}
