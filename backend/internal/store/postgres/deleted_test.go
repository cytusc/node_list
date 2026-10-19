package postgres

import (
	"context"
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/domain"
	"testing"
	"time"
)

func TestDeletionWatermarkRejectsOldImportsAndAllowsNewObservation(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	node := domain.Node{Name: "deleted", Type: "http", Server: "192.0.2.1", Port: "80"}
	healthy := node
	healthy.Server = "192.0.2.2"
	watermark := time.Now().Add(-time.Hour).UTC()
	if _, e := a.pool.Exec(ctx, `INSERT INTO node_explorer_deleted_nodes VALUES($1,$2)`, domain.AggregateIdentity(node), watermark); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name, stamp string
		want        int
	}{
		{"old", watermark.Add(-time.Minute).Format(time.RFC3339Nano), 1},
		{"equal", watermark.Format(time.RFC3339Nano), 1},
		{"unknown", "", 1},
		{"new", watermark.Add(time.Minute).Format(time.RFC3339Nano), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := domain.Snapshot{CheckedAt: tc.stamp, Nodes: []domain.Node{node, healthy}}
			raw, _ := json.Marshal(snap)
			if e := a.Insert(ctx, tc.name, raw, tc.stamp, 2); e != nil {
				t.Fatal(e)
			}
			var count int
			var stored []byte
			if e := a.pool.QueryRow(ctx, `SELECT node_count,payload FROM node_explorer_snapshots WHERE fingerprint=$1`, tc.name).Scan(&count, &stored); e != nil {
				t.Fatal(e)
			}
			if count != tc.want {
				t.Fatalf("node_count=%d want %d", count, tc.want)
			}
			if e := json.Unmarshal(stored, &snap); e != nil {
				t.Fatal(e)
			}
			if len(snap.Nodes) != tc.want {
				t.Fatal("raw nodes retained")
			}
		})
	}
}
