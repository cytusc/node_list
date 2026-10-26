package liveness

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"

	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAvailabilityOwnershipAndStaleResults(t *testing.T) {
	ctx := context.Background()
	a := testutil.Database(t)
	var err error
	node := domain.Node{Name: "test", Type: "http", Server: "192.0.2.1", Port: "80", RawConfig: domain.ProxyConfig{"type": "http", "server": "192.0.2.1", "port": 80}}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: []domain.Node{node}})
	if _, err = a.Exec(ctx, `INSERT INTO node_explorer_snapshots(fingerprint,imported_at,node_count,payload) VALUES('test','now',1,$1)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(node)
	id := "test-identity"
	if _, err = a.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index,available) VALUES($1,$2,1,0,false)`, id, payload); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Exec(ctx, `INSERT INTO node_explorer_snapshot_members(snapshot_id,node_index,identity) VALUES(1,0,$1)`, id); err != nil {
		t.Fatal(err)
	}
	// Acquire copies the explicit schema setting as a dedicated ownership session.
	d := &DB{Pool: a}
	// Make the isolated schema explicit for every input and ownership connection.
	schema := strings.TrimPrefix(os.Getenv("PGOPTIONS"), "-c search_path=")
	pc := a.Config()
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	d.Pool = pool
	if err = d.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	other := &DB{Pool: pool}
	if err = other.Acquire(ctx); err == nil {
		t.Fatal("second owner accepted")
	}
	other.Close()
	due, err := d.Due(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("unavailable input: %v %d", err, len(due))
	}
	n := due[0]
	check := func(want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, "SELECT available FROM node_explorer_node_registry WHERE identity=$1", n.Identity).Scan(&got); err != nil || got != want {
			t.Fatalf("available=%v want=%v err=%v", got, want, err)
		}
	}
	check(false)
	save := func(status string, reachable bool) {
		t.Helper()
		if err := d.Save(ctx, n, Result{Status: status, Engine: "mihomo", Reachable: reachable, CheckedAt: time.Now()}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	save("reachable", true)
	check(true)
	save("infrastructure", false)
	check(true)
	check(true)
	save("unreachable", false)
	check(false)
	if _, err = pool.Exec(ctx, "UPDATE node_explorer_node_registry SET node=node || '{\"name\":\"changed\"}'::jsonb WHERE identity=$1", n.Identity); err != nil {
		t.Fatal(err)
	}
	save("reachable", true)
	check(false)
	due, err = d.Due(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("changed config not due: %v %d", err, len(due))
	}
	n = due[0]
	if _, err = pool.Exec(ctx, "UPDATE node_explorer_node_registry SET incarnation='replacement' WHERE identity=$1", n.Identity); err != nil {
		t.Fatal(err)
	}
	save("reachable", true)
	check(false)
	if _, err = pool.Exec(ctx, "DELETE FROM node_explorer_snapshot_members WHERE identity=$1", n.Identity); err != nil {
		t.Fatal(err)
	}
	due, err = d.Due(ctx, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("orphan input: %v %d", err, len(due))
	}
}
