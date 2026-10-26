package postgres

import (
	"context"
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/domain"
	"testing"
)

func TestDuplicateBatchBackfillsOriginalAndArchivesAllSpellings(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	p := domain.ProxyConfig{"name": "n", "type": "http", "server": "example.com", "port": 8080}
	n := domain.SummarizeProxy(p)
	n.RawConfig = p
	if err := domain.EnsureNodeLink(&n); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.Snapshot{CheckedAt: "2026-10-26T00:00:00Z", Nodes: []domain.Node{n}}
	insert := func() {
		t.Helper()
		raw, _ := json.Marshal(snapshot)
		fp, _ := domain.SnapshotFingerprint(snapshot)
		if err := a.Insert(ctx, fp, raw, snapshot.CheckedAt, 1); err != nil {
			t.Fatal(err)
		}
	}
	insert()
	original := "http://example.com:8080#first%20original"
	snapshot.Nodes[0].RawLink = original
	insert()
	snapshot.Nodes[0].RawLink = "http://example.com:8080#another%20original"
	insert()
	snapshot.Nodes[0].RawLink = ""
	insert()
	var count int
	var got string
	if err := a.pool.QueryRow(ctx, `SELECT count(*),min(payload::jsonb->'nodes'->0->>'rawLink') FROM node_explorer_snapshots`).Scan(&count, &got); err != nil || count != 1 || got != original {
		t.Fatalf("batch provenance lost: count=%d original_preserved=%v err=%v", count, got == original, err)
	}
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_links WHERE origin='original'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("original spellings discarded: %d %v", count, err)
	}
	if _, err := a.pool.Exec(ctx, `DELETE FROM node_explorer_snapshots`); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_links`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("history cascaded with snapshots: %d %v", count, err)
	}
}
