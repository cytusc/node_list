package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

func TestCrossProcessChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	reader, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	st := store.New(reader, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	n := domain.Node{Name: "one", Type: "http", Server: "192.0.2.1", Port: "80"}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: []domain.Node{n}})
	check := func() {
		t.Helper()
		if err := st.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Insert(ctx, "first", raw, "", 1); err != nil {
		t.Fatal(err)
	}
	check()
	if st.LatestSnapshotID() != 1 {
		t.Fatal("empty-start reader missed new node")
	}
	c, err := reader.ReadChanges(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := reader.ReadChanges(ctx, c.Revision)
	if err != nil || len(idle.Batches) != 0 || len(idle.Enrichments) != 0 || idle.Full {
		t.Fatalf("idle refresh loaded payloads: %+v, %v", idle, err)
	}
	identity := domain.AggregateIdentity(n)
	if err := writer.SaveIPPure(ctx, 1, 0, []byte(`{"status":"success","country":"US"}`), identity); err != nil {
		t.Fatal(err)
	}
	check()
	snap, _ := st.Get(nil)
	if snap.Nodes[0].IPPure == nil || snap.Nodes[0].Country != "US" {
		t.Fatal("external enrichment missing")
	}
	if _, err := writer.db.ExecContext(ctx, `DELETE FROM ippure_results`); err != nil {
		t.Fatal(err)
	}
	check()
	snap, _ = st.Get(nil)
	if snap.Nodes[0].IPPure != nil {
		t.Fatal("empty enrichment set retained stale result")
	}
	if err := writer.RetireNodes(ctx, []string{identity}, "now", "test"); err != nil {
		t.Fatal(err)
	}
	check()
	snap, _ = st.Get(nil)
	if len(snap.Nodes) != 0 {
		t.Fatal("external retirement missing")
	}
	if _, err := writer.RestoreNode(ctx, identity, "now", "test"); err != nil {
		t.Fatal(err)
	}
	check()
	snap, _ = st.Get(nil)
	if len(snap.Nodes) != 1 {
		t.Fatal("empty retirement set retained stale identity")
	}
	// A changed batch must not force the entire archive through the reader.
	if err := writer.Insert(ctx, "second", raw, "", 1); err != nil {
		t.Fatal(err)
	}
	delta, err := reader.ReadChanges(ctx, c.Revision)
	if err != nil || delta.Full || len(delta.Batches) != 2 {
		t.Fatalf("delta: %+v, %v", delta, err)
	}
	check()
	c, err = reader.ReadChanges(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.SaveIPPure(ctx, 2, 0, []byte(`{"status":"error"}`), identity); err != nil {
		t.Fatal(err)
	}
	delta, err = reader.ReadChanges(ctx, c.Revision)
	if err != nil || len(delta.Batches) != 1 || delta.Batches[0].ID != 2 {
		t.Fatalf("incremental scope: %+v %v", delta, err)
	}
	if _, err := writer.db.ExecContext(ctx, `DELETE FROM ippure_results; DELETE FROM snapshots`); err != nil {
		t.Fatal(err)
	}
	check()
	if st.Aggregate(false) != nil {
		t.Fatal("deleted batches remained in cache")
	}
	// Local saves and refresh reads must never race or lose the latest save.
	if err := writer.Insert(ctx, "third", raw, "", 1); err != nil {
		t.Fatal(err)
	}
	check()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if err := st.SaveIPPure(3, 0, domain.IPPureResult{Status: "success", Organization: fmt.Sprint(i)}, identity); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if err := st.Refresh(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	check()
	snap, _ = st.Get(nil)
	rows, err := writer.Enrichments(ctx, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v %v", rows, err)
	}
	var stored domain.IPPureResult
	if err := json.Unmarshal(rows[0].Payload, &stored); err != nil {
		t.Fatal(err)
	}
	if snap.Nodes[0].IPPure.Organization != stored.Organization {
		t.Fatal("refresh overwrote a local save")
	}
}
