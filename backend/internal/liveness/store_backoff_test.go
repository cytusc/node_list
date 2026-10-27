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

// A dead address must stop returning every interval, otherwise the due queue
// grows past the workers' throughput and every availability verdict ages out.
func TestSaveBacksOffConsecutiveFailures(t *testing.T) {
	ctx := context.Background()
	a := testutil.Database(t)
	node := domain.Node{Name: "backoff", Type: "http", Server: "192.0.2.2", Port: "80", RawConfig: domain.ProxyConfig{"type": "http", "server": "192.0.2.2", "port": 80}}
	payload, _ := json.Marshal(node)
	if _, err := a.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,available) VALUES('backoff',$1,false)`, payload); err != nil {
		t.Fatal(err)
	}
	pc := a.Config()
	pc.ConnConfig.RuntimeParams["search_path"] = strings.TrimPrefix(os.Getenv("PGOPTIONS"), "-c search_path=")
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	d := &DB{Pool: pool, MaxBackoff: 4 * time.Hour}
	if err = d.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	due, err := d.Due(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %v %d", err, len(due))
	}
	n := due[0]
	interval := 30 * time.Minute
	read := func() (int, time.Duration) {
		t.Helper()
		var raw []byte
		var next time.Time
		if err := pool.QueryRow(ctx, `SELECT payload,next_at FROM node_explorer_liveness_attempts WHERE identity=$1`, n.Identity).Scan(&raw, &next); err != nil {
			t.Fatal(err)
		}
		var r Result
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r.FailureStreak, time.Until(next)
	}
	for _, want := range []struct {
		streak int
		delay  time.Duration
	}{
		{1, 30 * time.Minute},
		{2, 60 * time.Minute},
		{3, 120 * time.Minute},
		{4, 240 * time.Minute},
		{5, 240 * time.Minute}, // capped at MaxBackoff
	} {
		if err = d.Save(ctx, n, Result{Status: "unreachable", Engine: "mihomo", CheckedAt: time.Now().UTC()}, interval); err != nil {
			t.Fatal(err)
		}
		streak, delay := read()
		if streak != want.streak {
			t.Fatalf("streak %d want %d", streak, want.streak)
		}
		if delay < want.delay-time.Minute || delay > want.delay+time.Minute {
			t.Fatalf("streak %d delay %s want ~%s", streak, delay, want.delay)
		}
	}
	// A recovered address returns to the normal interval immediately.
	if err = d.Save(ctx, n, Result{Status: "reachable", Engine: "mihomo", Reachable: true, CheckedAt: time.Now().UTC()}, interval); err != nil {
		t.Fatal(err)
	}
	streak, delay := read()
	if streak != 0 || delay > interval+time.Minute {
		t.Fatalf("recovery kept streak=%d delay=%s", streak, delay)
	}
}
