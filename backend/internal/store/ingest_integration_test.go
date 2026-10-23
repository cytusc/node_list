package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/sqlite"
)

func TestImportFileChangesAndConcurrentImports(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	adapter, err := sqlite.New(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	st := store.New(adapter, nil)
	importer := store.NewIngestor(st)
	path := filepath.Join(dir, "results.json")
	write := func(name string) {
		t.Helper()
		// Some filesystems expose millisecond change-time resolution. Keep
		// each rewrite in a different tick, including the restored/latest pair.
		time.Sleep(10 * time.Millisecond)
		raw, _ := json.Marshal(domain.Snapshot{Nodes: []domain.Node{{Name: name, Type: "http", Server: "192.0.2.1", Port: "80"}}})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func() store.IngestResult {
		t.Helper()
		result, err := importer.ImportFile(ctx, path, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	write("before")
	if !run().Inserted {
		t.Fatal("first import missing")
	}
	if !run().Skipped {
		t.Fatal("unchanged file was reimported")
	}
	// Same size and POSIX mtime; the change time still invalidates the
	// signature. Windows stat exposes no change time, so the rewrite is only
	// picked up through a fresh mtime there.
	var previous time.Time
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		previous = info.ModTime()
	} else {
		time.Sleep(10 * time.Millisecond)
	}
	write("after!")
	if runtime.GOOS != "windows" {
		if err := os.Chtimes(path, previous, previous); err != nil {
			t.Fatal(err)
		}
	}
	if !run().Inserted {
		t.Fatal("same-size rewrite was skipped")
	}
	snap, _ := st.Get(nil)
	if snap.Nodes[0].Name != "after!" {
		t.Fatal("new data missing")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.ImportFile(ctx, path, "", ""); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
	write("return")
	if !run().Inserted {
		t.Fatal("restored file was skipped")
	}
	write("latest")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, err := importer.ImportFile(ctx, path, "", "")
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rows, err := adapter.All(ctx)
	if err != nil || len(rows) != 4 {
		t.Fatalf("concurrent import count=%d err=%v", len(rows), err)
	}
	// Replacing the inode while retaining size and timestamp must also import.
	old, _ := os.Stat(path)
	raw, _ := json.Marshal(domain.Snapshot{Nodes: []domain.Node{{Name: "atomic", Type: "http", Server: "192.0.2.1", Port: "80"}}})
	tmp := filepath.Join(dir, "replacement")
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Pin the mtime so only the POSIX change time distinguishes the
	// replacement. Windows has no observable change time, so its fresh mtime
	// carries the signal instead.
	if runtime.GOOS != "windows" {
		if err := os.Chtimes(tmp, time.Now(), old.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if !run().Inserted {
		t.Fatal("atomic replacement was skipped")
	}
}

func TestAdmissionFiltersBeforeInsertAndRetriesUnchangedFile(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached", true: "persistent"}[persistent], func(t *testing.T) {
			ctx := context.Background()
			adapter, err := sqlite.New(filepath.Join(t.TempDir(), "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			st := store.New(adapter, nil)
			deny := true
			lookupErr := errors.New("evidence lookup unavailable")
			filter := func(_ context.Context, nodes []domain.Node) (map[int]string, error) {
				if len(nodes) != 1 || nodes[0].Name != "valid" {
					t.Fatalf("static faults reached runtime admission: %d candidates", len(nodes))
				}
				if lookupErr != nil {
					return nil, lookupErr
				}
				if deny {
					return map[int]string{0: "confirmed-retired-connection"}, nil
				}
				return nil, nil
			}
			importer := store.NewIngestor(st, filter)
			if persistent {
				importer = store.NewPersistentIngestor(st, filter)
			}
			path := filepath.Join(t.TempDir(), "results.json")
			snap := domain.Snapshot{Nodes: []domain.Node{
				{Name: "bad Reality", RawLink: "vless://user@example.com:443?security=reality&pbk=key"},
				{Name: "bad link", RawLink: "vmess://private-secret"},
				{Name: "valid", RawLink: "http://example.com:80"},
			}}
			raw, _ := json.Marshal(snap)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := importer.ImportFile(ctx, path, "", ""); !errors.Is(err, lookupErr) {
				t.Fatalf("lookup failure bypassed admission: %v", err)
			}
			lookupErr = nil
			if _, err := importer.ImportFile(ctx, path, "", ""); !errors.Is(err, store.ErrNoAdmissibleNodes) {
				t.Fatalf("expected rejected batch: %v", err)
			}
			rows, err := adapter.All(ctx)
			if err != nil || len(rows) != 0 {
				t.Fatalf("rejected candidates were persisted: %d %v", len(rows), err)
			}
			deny = false // Recovery restores a connection without rewriting the file.
			result, err := importer.ImportFile(ctx, path, "", "")
			if err != nil || !result.Inserted {
				t.Fatalf("unchanged file could not retry: %+v %v", result, err)
			}
			rows, err = adapter.All(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("stored batches: %d %v", len(rows), err)
			}
			var stored domain.Snapshot
			if err := json.Unmarshal(rows[0].Payload, &stored); err != nil || len(stored.Nodes) != 1 || stored.Nodes[0].Name != "valid" {
				t.Fatal("faulty nodes leaked into payload")
			}
			snap.Nodes = snap.Nodes[:2]
			if err := importer.Import(ctx, snap, nil); !errors.Is(err, store.ErrNoAdmissibleNodes) {
				t.Fatalf("all-invalid batch: %v", err)
			}
			rows, err = adapter.All(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatal("all-invalid batch changed previous data")
			}
		})
	}
}

func TestImportFiltersMissingCredentialsAfterSourceEnrichment(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached", true: "persistent"}[persistent], func(t *testing.T) {
			ctx := context.Background()
			adapter, err := sqlite.New(filepath.Join(t.TempDir(), "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close()
			st := store.New(adapter, nil)
			importer := store.NewIngestor(st)
			if persistent {
				importer = store.NewPersistentIngestor(st)
			}
			node := func(name, protocol, link string) domain.Node {
				return domain.Node{Name: name, Type: protocol, Server: "example.com", Port: "443", RawLink: link}
			}
			rescued := node("rescued", "trojan", "trojan://example.com:443")
			key, err := domain.NodeIdentity(rescued)
			if err != nil {
				t.Fatal(err)
			}
			sources := &domain.Sources{Configs: map[string][]domain.ProxyConfig{
				key: {{"name": "rescued", "type": "trojan", "server": "example.com", "port": 443, "password": "secret"}},
			}}
			snap := domain.Snapshot{CheckedAt: "2026-10-22T01:00:00Z", Nodes: []domain.Node{
				node("missing UUID", "vless", "vless://example.com:443"),
				node("missing reality key", "vless", "vless://user@example.com:443?security=reality"),
				node("missing token", "tuic", "tuic://example.com:443"),
				// This shares a fallback identity with the missing UUID entry.
				// Dropping that invalid entry must not suppress this one.
				node("unsupported retained", "vless", "vless://user@example.com:443?type=future-transport"),
				node("HTTP retained", "http", "http://example.com:443"), rescued,
			}}
			if err := importer.Import(ctx, snap, sources); err != nil {
				t.Fatal(err)
			}
			rows, err := adapter.All(ctx)
			if err != nil || len(rows) != 1 {
				t.Fatalf("stored rounds: %d, %v", len(rows), err)
			}
			var stored domain.Snapshot
			if err := json.Unmarshal(rows[0].Payload, &stored); err != nil {
				t.Fatal(err)
			}
			if len(stored.Nodes) != 3 || stored.Nodes[0].Name != "unsupported retained" || stored.Nodes[1].Name != "HTTP retained" || stored.Nodes[2].Name != "rescued" || stored.Nodes[2].RawConfig["password"] != "secret" {
				t.Fatalf("incorrect filtering or enrichment: %+v", stored.Nodes)
			}
		})
	}
}
