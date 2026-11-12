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
	// One success lists the node immediately; no confirming probe is
	// needed before it appears.
	check(true)
	save("reachable", true)
	check(true)
	save("infrastructure", false)
	check(true)
	check(true)
	save("unreachable", false)
	check(true)
	// infrastructure between failures must not reset the streak.
	save("infrastructure", false)
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

func TestDuePrioritisesFreshNodes(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Database(t)
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		t.Fatal(err)
	}
	// Fresh nodes have no attempts row; available and dead nodes have one
	// with a matching incarnation and digest, due an hour ago. A pending
	// node has one success but is not yet listed, so it leads the fresh
	// class. Identities sort so the class, not the name, decides the order.
	if _, err := pool.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,available) SELECT 'fresh-'||i,'{}',false FROM generate_series(1,6) i;
 INSERT INTO node_explorer_node_registry(identity,node,available) SELECT 'avail-'||i,'{}',true FROM generate_series(1,6) i;
 INSERT INTO node_explorer_node_registry(identity,node,available) SELECT 'dead-'||i,'{}',false FROM generate_series(1,6) i;
 INSERT INTO node_explorer_node_registry(identity,node,available) VALUES('pending-1','{}',false);
 INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload)
  SELECT identity,incarnation,md5((node - 'rawLink' - 'generatedLink')::text),now(),now() - interval '1 hour','{}'::jsonb
  FROM node_explorer_node_registry WHERE identity LIKE 'avail-%';
 INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload)
  SELECT identity,incarnation,md5((node - 'rawLink' - 'generatedLink')::text),now(),now() - interval '1 hour','{"failureStreak":1}'::jsonb
  FROM node_explorer_node_registry WHERE identity LIKE 'dead-%';
 INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload)
  SELECT identity,incarnation,md5((node - 'rawLink' - 'generatedLink')::text),now(),now() - interval '1 hour','{"successStreak":1}'::jsonb
  FROM node_explorer_node_registry WHERE identity='pending-1'`); err != nil {
		t.Fatal(err)
	}
	d := DB{Pool: pool}
	nodes, err := d.Due(ctx, 12)
	if err != nil || len(nodes) != 12 {
		t.Fatalf("due: %d %v", len(nodes), err)
	}
	if nodes[0].Identity != "pending-1" {
		t.Fatalf("first node=%s want pending-1", nodes[0].Identity)
	}
	counts := map[string]int{}
	for _, n := range nodes {
		counts[strings.SplitN(n.Identity, "-", 2)[0]]++
	}
	// Fresh gets half the slots (3:2:1): the pending node plus five fresh,
	// four available, two dead.
	if counts["fresh"] != 5 || counts["pending"] != 1 || counts["avail"] != 4 || counts["dead"] != 2 {
		t.Fatalf("slots=%v want fresh=5 pending=1 avail=4 dead=2", counts)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM node_explorer_node_registry WHERE identity LIKE 'dead-%'`); err != nil {
		t.Fatal(err)
	}
	nodes, err = d.Due(ctx, 12)
	if err != nil || len(nodes) != 12 {
		t.Fatalf("unused quota not filled: %d %v", len(nodes), err)
	}
	for _, n := range nodes {
		if strings.HasPrefix(n.Identity, "dead-") {
			t.Fatalf("dead node returned after delete: %s", n.Identity)
		}
	}
}
