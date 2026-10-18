package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestRegistryConsumesOnlySourceChangesAcrossRestarts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// A runtime-only change must not deserialize the archive, even after restart.
	f.exec(t, `UPDATE node_explorer_connectivity SET checked_at=now()`)
	f.exec(t, `ALTER TABLE node_explorer_snapshots RENAME COLUMN payload TO temporarily_unreadable`)
	f.repo.registryReady = false
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatalf("unchanged source read payload: %v", err)
	}
	f.exec(t, `ALTER TABLE node_explorer_snapshots RENAME COLUMN temporarily_unreadable TO payload`)
	var raw []byte
	if err := f.repo.Pool.QueryRow(ctx, `SELECT node FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var node domain.Node
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	node.Name = "updated live node"
	snap := domain.Snapshot{CheckedAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano), Nodes: []domain.Node{node}}
	payload, _ := json.Marshal(snap)
	if err := f.st.Adapter().Insert(ctx, "new", payload, snap.CheckedAt, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	var batch int
	var name string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT snapshot_id,node->>'name' FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&batch, &name); err != nil {
		t.Fatal(err)
	}
	if batch != 2 || name != node.Name {
		t.Fatalf("new source not registered: %d %s", batch, name)
	}

	// Source compaction removes obsolete full copies, so deleting the winning
	// source cannot silently restore older configuration/measurements.
	var copies int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT sum(node_count) FROM node_explorer_snapshots`).Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("duplicate source records: %d %v", copies, err)
	}
	f.exec(t, `DELETE FROM node_explorer_snapshots WHERE id=2`)
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	var current bool
	if err := f.repo.Pool.QueryRow(ctx, `SELECT available FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&current); err != nil || current {
		t.Fatalf("obsolete source revived: %v %v", current, err)
	}
	f.exec(t, `DELETE FROM node_explorer_snapshots`)
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	var available bool
	if err := f.repo.Pool.QueryRow(ctx, `SELECT available FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&available); err != nil || available {
		t.Fatalf("removed source stayed available: %v %v", available, err)
	}
}

func TestSourceRetentionPreservesFactsAndHonorsDisable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "")
	f.exec(t, `UPDATE node_explorer_snapshots SET imported_at=$1`, time.Now().AddDate(0, 0, -8).UTC().Format(time.RFC3339Nano))
	if err := f.repo.pruneSources(ctx, -1); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_snapshots`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("disabled retention deleted source: %d %v", count, err)
	}
	if err := f.repo.pruneSources(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE available`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("current source removed by import age: %d %v", count, err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_exits`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retention removed runtime facts: %d %v", count, err)
	}
}
