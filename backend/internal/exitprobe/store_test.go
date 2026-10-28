package exitprobe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/servicelock"
	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func fixture(t *testing.T) *DB {
	t.Helper()
	cfg := testutil.Postgres(t)
	cfg.RuntimeParams["search_path"] = strings.TrimPrefix(os.Getenv("PGOPTIONS"), "-c search_path=")
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig = cfg
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Only public input/output contracts exist: no snapshots, Redis or task services.
	_, err = pool.Exec(context.Background(), `CREATE TABLE node_explorer_node_registry(identity text PRIMARY KEY,incarnation text NOT NULL,node jsonb NOT NULL,available boolean NOT NULL);
 CREATE TABLE node_explorer_exits(identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity),ip text NOT NULL,version bigint NOT NULL DEFAULT 1,checked_at timestamptz NOT NULL,payload jsonb NOT NULL,pinned_country text);
 CREATE TABLE node_explorer_ip_attributes(ip text PRIMARY KEY,checked_at timestamptz NOT NULL,payload jsonb NOT NULL);
 CREATE TABLE node_explorer_service_status(service text PRIMARY KEY,updated_at timestamptz NOT NULL,payload jsonb NOT NULL);
 INSERT INTO node_explorer_node_registry VALUES('up','v1','{"name":"one"}',true),('down','v1','{}',false);`)
	if err != nil {
		t.Fatal(err)
	}
	db := &DB{Pool: pool}
	t.Cleanup(db.Close)
	if err = db.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestAvailableOnlyAndStaleFences(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	due, err := d.Due(ctx, 10)
	if err != nil || len(due) != 1 || due[0].Identity != "up" {
		t.Fatalf("due=%v err=%v", due, err)
	}
	n := due[0]
	for _, change := range []string{`available=false`, `incarnation='v2'`, `node='{"name":"changed"}'`} {
		if _, err = d.Pool.Exec(ctx, `UPDATE node_explorer_node_registry SET `+change+` WHERE identity='up'`); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.Current(ctx, n); err != nil || ok {
			t.Fatalf("stale input accepted %v %v", ok, err)
		}
		if err = d.Save(ctx, n, Result{IP: "8.8.8.8", CheckedAt: time.Now()}, time.Hour); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM node_explorer_exits)+(SELECT count(*) FROM node_explorer_exit_attempts)`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("stale write %d %v", count, err)
		}
		if _, err = d.Pool.Exec(ctx, `UPDATE node_explorer_node_registry SET available=true,incarnation='v1',node='{"name":"one"}' WHERE identity='up'`); err != nil {
			t.Fatal(err)
		}
	}
}
func TestExitFailureAndRefreshHaveNoOtherResponsibilities(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	nodes, _ := d.Due(ctx, 1)
	n := nodes[0]
	save := func(r Result) {
		t.Helper()
		r.CheckedAt = time.Now()
		if err := d.Save(ctx, n, r, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	read := func() domain.ExitState {
		t.Helper()
		var b []byte
		if err := d.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_exits WHERE identity='up'`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		var v domain.ExitState
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	save(Result{IP: "8.8.8.8", Endpoint: "https://echo"})
	if v := read(); !v.Verified || v.Version != 1 || v.AddressFamily != 4 {
		t.Fatalf("initial %+v", v)
	}
	save(Result{Error: "echo_failed"})
	if v := read(); !v.Verified || v.IP != "8.8.8.8" || v.Version != 1 {
		t.Fatalf("failed %+v", v)
	}
	save(Result{IP: "1.1.1.1"})
	if v := read(); !v.Verified || v.Version != 2 || v.FailureStreak != 0 {
		t.Fatalf("refresh %+v", v)
	}
	due, err := d.Due(ctx, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("retry persisted: %v %v", due, err)
	}
	var available bool
	if err = d.Pool.QueryRow(ctx, `SELECT available FROM node_explorer_node_registry WHERE identity='up'`).Scan(&available); err != nil || !available {
		t.Fatalf("availability modified %v %v", available, err)
	}
	// New instance immediately becomes due, independent of any liveness task.
	if _, err = d.Pool.Exec(ctx, `UPDATE node_explorer_node_registry SET incarnation='v2' WHERE identity='up'`); err != nil {
		t.Fatal(err)
	}
	due, err = d.Due(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("new instance not due %v %v", due, err)
	}
}
func TestOwnershipLossFencesWritesAndAllowsRestart(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	if err := servicelock.Run(ctx, d.Pool, "exit-provision", func(context.Context) error { t.Error("legacy writer started"); return nil }); !errors.Is(err, servicelock.ErrServiceRunning) {
		t.Fatalf("legacy lock incompatible: %v", err)
	}
	other := &DB{Pool: d.Pool}
	defer other.Close()
	if err := other.Acquire(ctx); err == nil {
		t.Fatal("duplicate owner accepted")
	}
	other.Close()
	due, _ := d.Due(ctx, 1)
	var killed bool
	if err := d.Pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, d.owner.PgConn().PID()).Scan(&killed); err != nil || !killed {
		t.Fatalf("terminate %v", err)
	}
	successor := &DB{Pool: d.Pool}
	defer successor.Close()
	if err := successor.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Save(ctx, due[0], Result{IP: "8.8.8.8", CheckedAt: time.Now()}, time.Hour); err == nil {
		t.Fatal("lost owner committed")
	}
	if err := successor.Save(ctx, due[0], Result{IP: "1.1.1.1", CheckedAt: time.Now()}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := successor.Heartbeat(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := successor.Healthy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := successor.Heartbeat(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := successor.Healthy(ctx); err == nil {
		t.Fatal("stopped service healthy")
	}
}

func TestRestartPreservesDueTime(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	nodes, err := d.Due(ctx, 1)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("due %v %v", nodes, err)
	}
	if err = d.Save(ctx, nodes[0], Result{Error: "echo_failed", CheckedAt: time.Now()}, time.Hour); err != nil {
		t.Fatal(err)
	}
	d.Close()
	restarted := &DB{Pool: d.Pool}
	defer restarted.Close()
	if err = restarted.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	nodes, err = restarted.Due(ctx, 10)
	if err != nil || len(nodes) != 0 {
		t.Fatalf("restart ignored due: %v %v", nodes, err)
	}
}
