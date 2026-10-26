package liveness

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAvailabilityOwnershipAndStaleResults(t *testing.T) {
	cfg := testutil.Postgres(t)
	ctx := context.Background()
	a, err := postgres.NewAdapter(ctx, postgres.Config{Host: cfg.Host, Port: int(cfg.Port), User: cfg.User, Password: cfg.Password, Database: cfg.Database, PoolMax: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	node := domain.Node{Name: "test", Type: "http", Server: "192.0.2.1", Port: "80", RawConfig: domain.ProxyConfig{"type": "http", "server": "192.0.2.1", "port": 80}}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: []domain.Node{node}})
	if err = a.Insert(ctx, "test", raw, "", 1); err != nil {
		t.Fatal(err)
	}
	st := store.New(a, nil)
	if err = st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	repo := &service.Repository{Pool: a.Pool()}
	if err = repo.SyncRegistry(ctx, st); err != nil {
		t.Fatal(err)
	}
	// Acquire copies the explicit schema setting as a dedicated ownership session.
	d := &DB{Pool: a.Pool()}
	// Make the isolated schema explicit for every input and ownership connection.
	schema := strings.TrimPrefix(os.Getenv("PGOPTIONS"), "-c search_path=")
	pc := a.Pool().Config()
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
	if err = repo.SyncRegistry(ctx, st); err != nil {
		t.Fatal(err)
	}
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
