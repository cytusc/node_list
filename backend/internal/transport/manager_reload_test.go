package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManagerSmallRegistryUpdateSkipsFullValidationBatches(t *testing.T) {
	m, _, pushes := fakeManager(t)
	entries := make([]RegistryEntry, 600)
	for i := range entries {
		entries[i] = entry(fmt.Sprintf("node-%04d", i))
	}
	entries[100].Node.RawConfig["type"] = "invalid"
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	before := pushes.Load()
	entries = append(entries, entry("new-valid"))
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	if n := pushes.Load() - before; n != 1 {
		t.Fatalf("small update revalidated old registry: %d pushes", n)
	}
	if m.Stats().Quarantined != 1 || m.Stats().Loaded != 600 {
		t.Fatalf("quarantine lost: %+v", m.Stats())
	}
	// Fixing the connection digest must retry a previously quarantined node.
	for i := range entries {
		if entries[i].Identity == "node-0100" {
			entries[i].Node.RawConfig["type"] = "ss"
		}
	}
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	if m.Stats().Quarantined != 0 {
		t.Fatal("fixed config stayed quarantined")
	}
}

func TestRealManagerIncrementalReloadKeepsRoutingAndFences(t *testing.T) {
	m := realManager(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "isolated-route") }))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	size := 1024
	if v, e := strconv.Atoi(os.Getenv("MIHOMO_RELOAD_BENCH_SIZE")); e == nil && v > 0 {
		size = v
	}
	entries := make([]RegistryEntry, size)
	for i := range entries {
		entries[i] = entry(fmt.Sprintf("node-%04d", i))
	}
	entries[0].Node.RawConfig["port"] = atoi(port)
	ctx := context.Background()
	if err := m.Sync(ctx, entries); err != nil {
		t.Fatal(err)
	}
	before := m.Stats().Generation
	loads := m.Stats().ConfigLoads
	entries = append(entries, entry("new-node"))
	started := time.Now()
	if err := m.Sync(ctx, entries); err != nil {
		t.Fatal(err)
	}
	t.Logf("incremental %d->%d reload: %s; controller loads: %d", size, size+1, time.Since(started), m.Stats().ConfigLoads-loads)
	if m.Stats().ConfigLoads-loads != 1 {
		t.Fatal("incremental reload pushed validation batches")
	}
	if m.Stats().Generation == before {
		t.Fatal("reload did not fence old generation")
	}
	var target RegistryEntry
	for _, e := range entries {
		if e.Identity == "node-0000" {
			target = e
		}
	}
	lease, err := m.Acquire(LeaseRequest{Targets: []RegistryEntry{target}, Kind: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release(lease.ID)
	c := listenerClient(lease.URLs[0])
	defer c.CloseIdleConnections()
	resp, err := c.Get("http://route-isolation.invalid/check")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "isolated-route" {
		t.Fatalf("route changed: %q", body)
	}
	if err := m.Validate(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
}
