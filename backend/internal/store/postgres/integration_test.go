package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testAdapter(t *testing.T) *Adapter {
	t.Helper()
	cfg := testutil.Postgres(t)
	a, err := NewAdapter(context.Background(), Config{Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database, User: cfg.User, Password: cfg.Password, PoolMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestRuntimeSchemaDoesNotCreateLegacyProjection(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	// Every service calls initSchema at startup. A second call must use the
	// committed version without creating tables for the retired API projection.
	if err := a.initSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var current *string
	var applied bool
	if err := a.pool.QueryRow(ctx, `SELECT to_regclass('node_explorer_current_nodes')::text,
	 EXISTS(SELECT 1 FROM node_explorer_service_migrations WHERE version=9)`).Scan(&current, &applied); err != nil {
		t.Fatal(err)
	}
	if current != nil || !applied {
		t.Fatalf("unexpected runtime migration: legacy projection=%v applied=%v", current, applied)
	}
}

func TestProjectionGenerationsAndLockOwnership(t *testing.T) {
	a := testAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := store.New(a, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := NewProjectorForAdapter(ctx, a, st, ProjectorConfig{RetentionDays: -1})
	if err != nil {
		t.Fatal(err)
	}
	// A second process has its own connection and projection cache.
	poolCfg := a.pool.Config()
	other, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	observer := store.New(&Adapter{pool: other}, nil)
	if err := observer.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := NewProjector(ctx, other, observer, ProjectorConfig{RetentionDays: -1})
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{Name: "one", Type: "ss", Server: "192.0.2.1", Port: "443", RawConfig: domain.ProxyConfig{"password": "private"}}
	raw, _ := json.Marshal(domain.Snapshot{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Nodes: []domain.Node{node}})
	if err := a.Insert(ctx, "one", raw, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	changes, err := a.ReadChanges(ctx, 0)
	if err != nil || changes.Full || len(changes.Batches) != 1 {
		t.Fatalf("publication forced a full source reload: %+v %v", changes, err)
	}
	page := func(p *Projector, want int) {
		t.Helper()
		got, err := p.QueryPage(ctx, &Query{Page: 1, Limit: 50, Sort: "speed", Direction: "desc"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != want {
			t.Fatalf("page has %d nodes, want %d", got.Total, want)
		}
	}
	page(p, 1)
	// PoolMax=1 makes incorrect connection ownership and nested pool acquisition
	// deterministic failures instead of relying on which idle connection is chosen.
	var held int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid()`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("advisory locks leaked=%d err=%v", held, err)
	}
	if err := observer.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if observer.LatestSnapshotID() != 1 {
		t.Fatal("observer missed already published batch")
	}
	// After another process publishes, this projector must rebuild from a coherent
	// generation rather than its older cached batches.
	if err := a.SaveIPPure(ctx, 1, 0, []byte(`{"status":"success","country":"US"}`), domain.AggregateIdentity(node)); err != nil {
		t.Fatal(err)
	}
	if err := second.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	snap, _ := st.Get(nil)
	if snap.Nodes[0].Country != "US" {
		t.Fatal("published generation did not reach main cache")
	}
	// Empty enrichment and retirement sets must replace previously cached rows.
	if _, err := a.pool.Exec(ctx, `DELETE FROM `+tableEnrichments); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	snap, _ = st.Get(nil)
	if snap.Nodes[0].IPPure != nil {
		t.Fatal("deleted enrichment retained")
	}
	identity := domain.AggregateIdentity(node)
	if err := a.RetireNodes(ctx, []string{identity}, "now", "test"); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	page(p, 0)
	if _, err := a.RestoreNode(ctx, identity, "now", "test"); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	page(p, 1)
	if _, err := a.pool.Exec(ctx, `DELETE FROM `+tableSnapshots); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	page(p, 0)
	if err := observer.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if observer.Aggregate(false) != nil {
		t.Fatal("external deletion retained")
	}
}

func TestConnectionStringSpecialCharacters(t *testing.T) {
	want := Config{Host: "localhost", Port: 5432, Database: "node list", User: "a'b", Password: "spaces \\ path ' secret"}
	cfg, err := pgxpool.ParseConfig(want.connString())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != want.Database || cfg.ConnConfig.User != want.User || cfg.ConnConfig.Password != want.Password {
		t.Fatal("connection value quoting changed credentials")
	}
	if !strings.Contains(want.connString(), "host=") {
		t.Fatal("missing host")
	}
}
