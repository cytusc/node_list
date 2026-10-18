package service

import (
	"context"
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/domain"
	"testing"
	"time"
)

func TestSourceCompactionKeepsOneOccurrenceAndPreservesFacts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "")
	success := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	f.exec(t, `UPDATE node_explorer_node_registry SET last_available_at=$1`, success)
	var raw []byte
	_ = f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_snapshots LIMIT 1`).Scan(&raw)
	var original domain.Snapshot
	_ = json.Unmarshal(raw, &original)
	a := original.Nodes[0]
	b := a
	b.Server = "192.0.2.2"
	b.Name = "kept with enrichment"
	now := time.Now().UTC()
	add := func(key, stamp string, nodes []domain.Node) {
		t.Helper()
		snap := domain.Snapshot{CheckedAt: stamp, Nodes: nodes}
		raw, _ := json.Marshal(snap)
		if e := f.st.Adapter().Insert(ctx, key, raw, stamp, len(nodes)); e != nil {
			t.Fatal(e)
		}
	}
	add("older", now.Add(time.Minute).Format(time.RFC3339Nano), []domain.Node{a, b, a})
	f.exec(t, `INSERT INTO node_explorer_ippure_results(snapshot_id,node_index,payload) SELECT id,1,'{"ip":"1.1.1.1"}' FROM node_explorer_snapshots WHERE fingerprint='older'`)
	a.Name = "latest"
	c := b
	c.Server = "192.0.2.3"
	add("latest", now.Add(2*time.Minute).Format(time.RFC3339Nano), []domain.Node{a, c})
	if e := f.repo.SyncRegistry(ctx, f.st); e != nil {
		t.Fatal(e)
	}
	var copies, identities int
	if e := f.repo.Pool.QueryRow(ctx, `SELECT sum(node_count),(SELECT count(*) FROM node_explorer_node_registry) FROM node_explorer_snapshots`).Scan(&copies, &identities); e != nil || copies != 3 || identities != 3 {
		t.Fatalf("copies=%d identities=%d err=%v", copies, identities, e)
	}
	var name string
	var after time.Time
	if e := f.repo.Pool.QueryRow(ctx, `SELECT node->>'name',last_available_at FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&name, &after); e != nil || name != "latest" || !after.Equal(success) {
		t.Fatalf("canonical facts reset: %s %s %v", name, after, e)
	}
	var index int
	if e := f.repo.Pool.QueryRow(ctx, `SELECT e.node_index FROM node_explorer_ippure_results e JOIN node_explorer_snapshots s ON s.id=e.snapshot_id WHERE s.fingerprint='older'`).Scan(&index); e != nil || index != 0 {
		t.Fatalf("enrichment remap=%d %v", index, e)
	}
	a.Name = "stale import"
	add("late-arriving-old-source", now.Add(-time.Minute).Format(time.RFC3339Nano), []domain.Node{a})
	if e := f.repo.SyncRegistry(ctx, f.st); e != nil {
		t.Fatal(e)
	}
	if e := f.repo.Pool.QueryRow(ctx, `SELECT node_count FROM node_explorer_snapshots WHERE fingerprint='late-arriving-old-source'`).Scan(&copies); e != nil || copies != 0 {
		t.Fatal("obsolete source retained full node")
	}
	if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_snapshot_members`).Scan(&copies); e != nil || copies != 3 {
		t.Fatalf("source index duplicate: %d %v", copies, e)
	}
}
