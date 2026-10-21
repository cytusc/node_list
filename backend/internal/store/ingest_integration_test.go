package store_test

import (
	"context"
	"encoding/json"
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
