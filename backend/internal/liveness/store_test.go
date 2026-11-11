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
	payload, _ := json.Marshal(node)
	id := "test-identity"
	if _, err = a.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,available) VALUES($1,$2,false)`, id, payload); err != nil {
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
	due, err = d.Due(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("replacement not scheduled from registry: %v %d", err, len(due))
	}
}

func TestDueReservesAvailableCapacityWithoutStarvation(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Database(t)
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,available) SELECT 'available-'||i,'{}',true FROM generate_series(1,12) i;
 INSERT INTO node_explorer_node_registry(identity,node,available) SELECT 'new-'||i,'{}',false FROM generate_series(1,12) i`); err != nil {
		t.Fatal(err)
	}
	d := DB{Pool: pool}
	nodes, err := d.Due(ctx, 8)
	if err != nil || len(nodes) != 8 {
		t.Fatalf("due: %d %v", len(nodes), err)
	}
	available := 0
	for _, n := range nodes {
		if strings.HasPrefix(n.Identity, "available-") {
			available++
		}
	}
	if available != 6 {
		t.Fatalf("available slots=%d want 6/8", available)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM node_explorer_node_registry WHERE NOT available`); err != nil {
		t.Fatal(err)
	}
	nodes, err = d.Due(ctx, 8)
	if err != nil || len(nodes) != 8 {
		t.Fatalf("unused quota not filled: %d %v", len(nodes), err)
	}
}
