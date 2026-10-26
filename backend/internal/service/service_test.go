package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/ratelimit"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5"
)

type fixture struct {
	repo   *Repository
	st     *store.Store
	id     string
	runner *Runner
}

func setup(t *testing.T) *fixture {
	t.Helper()
	// Fixtures hold one or two nodes, so the production survival floor (500)
	// would park every deletion. Tests that care about the floor set their own.
	t.Setenv("NODE_CLEANUP_MIN_POOL", "0")
	cfg := testutil.Postgres(t)
	ctx := context.Background()
	a, e := postgres.NewAdapter(ctx, postgres.Config{Host: cfg.Host, Port: int(cfg.Port), User: cfg.User, Database: cfg.Database, Password: cfg.Password, PoolMax: 6})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { a.Close() })
	node := domain.Node{Name: "residential", Type: "http", Server: "192.0.2.1", Port: "443", RawConfig: domain.ProxyConfig{"password": "private"}}
	raw, _ := json.Marshal(domain.Snapshot{SpeedTest: true, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Nodes: []domain.Node{node}})
	if e = a.Insert(ctx, "fixture", raw, "", 1); e != nil {
		t.Fatal(e)
	}
	st := store.New(a, nil)
	if e = st.Reload(ctx); e != nil {
		t.Fatal(e)
	}
	repo := &Repository{Pool: a.Pool()}
	if e = repo.SyncRegistry(ctx, st); e != nil {
		t.Fatal(e)
	}
	return &fixture{repo: repo, st: st, id: domain.AggregateIdentity(node), runner: &Runner{Repo: repo, Config: Config{ResidentialInterval: 5 * time.Minute, NormalInterval: 30 * time.Minute}}}
}
func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, e := f.repo.Pool.Exec(context.Background(), sql, args...); e != nil {
		t.Fatal(e)
	}
}
func (f *fixture) claim(t *testing.T, kind string) Job {
	t.Helper()
	var ref string
	e := f.repo.Pool.QueryRow(context.Background(), `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND state='queued' AND due_at<=now() ORDER BY id LIMIT 1`, kind).Scan(&ref)
	if e != nil {
		t.Fatal(e)
	}
	j, e := f.repo.Claim(context.Background(), kind, ref, true)
	if e != nil || j == nil {
		t.Fatalf("claim %s: %v", kind, e)
	}
	return *j
}
func isWaiting(e error) bool { var w waiting; return errors.As(e, &w) }

func TestSchedulerBackfillsMissingTrafficRatioWithCooldown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		status    string
		wantQueue bool
		queued    bool
	}{
		{name: "legacy result is checked once", age: time.Hour, wantQueue: true},
		{name: "recent no-data response", age: time.Hour, status: "no-data", wantQueue: false},
		{name: "recent query error", age: 20 * time.Minute, status: "query-failed", wantQueue: false},
		{name: "retry query error after cooldown", age: 31 * time.Minute, status: "query-failed", wantQueue: true, queued: true},
		{name: "retry no-data after six hours", age: 7 * time.Hour, status: "no-data", wantQueue: true, queued: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			f.evidence(t, "ratio-backfill")
			stamp := time.Now().Add(-tc.age).UTC().Format(time.RFC3339Nano)
			if tc.status == "" {
				f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=(jsonb_set(payload,'{status}','"success"'::jsonb)-'{trafficRatioStatus}'-'{trafficRatioCheckedAt}') WHERE ip='8.8.4.4'`)
			} else {
				f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=jsonb_set(jsonb_set(jsonb_set(payload,'{status}','"success"'::jsonb),'{trafficRatioStatus}',to_jsonb($1::text)),'{trafficRatioCheckedAt}',to_jsonb($2::text)) WHERE ip='8.8.4.4'`, tc.status, stamp)
			}
			if err := f.repo.Schedule(ctx, KindRisk, "8.8.4.4", Payload{IP: "8.8.4.4", Identity: f.id}, 0, time.Now()); err != nil {
				t.Fatal(err)
			}
			initialWorkState := "done"
			if tc.queued {
				initialWorkState = "queued"
			}
			f.exec(t, `UPDATE node_explorer_work SET state=$2,due_at=CASE WHEN $2='queued' THEN now()+interval '24 hours' ELSE due_at END WHERE kind=$1 AND key='8.8.4.4'`, KindRisk, initialWorkState)
			f.runner.Config.Role = "exit-provision"
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			var generation int64
			if err := f.repo.Pool.QueryRow(ctx, `SELECT state,generation FROM node_explorer_work WHERE kind=$1 AND key='8.8.4.4'`, KindRisk).Scan(&state, &generation); err != nil {
				t.Fatal(err)
			}
			if got := state == "queued"; got != tc.wantQueue {
				t.Fatalf("queued=%v, want %v (state=%s)", got, tc.wantQueue, state)
			}
			if tc.wantQueue {
				var due time.Time
				if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at FROM node_explorer_work WHERE kind=$1 AND key='8.8.4.4'`, KindRisk).Scan(&due); err != nil {
					t.Fatal(err)
				}
				if time.Until(due) > time.Minute {
					t.Fatalf("backfill due remains in the future: %v", due)
				}
				if err := f.runner.schedule(ctx); err != nil {
					t.Fatal(err)
				}
				var repeatedGeneration int64
				if err := f.repo.Pool.QueryRow(ctx, `SELECT generation FROM node_explorer_work WHERE kind=$1 AND key='8.8.4.4'`, KindRisk).Scan(&repeatedGeneration); err != nil {
					t.Fatal(err)
				}
				if repeatedGeneration != generation {
					t.Fatalf("queued backfill was regenerated: %d -> %d", generation, repeatedGeneration)
				}
			}
		})
	}
}

func TestClassificationRefreshPreservesRiskTimestamp(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	f.evidence(t, "initial")
	f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=payload || jsonb_build_object('status','success','score',10,'checkedAt',$1::text)`, old)
	if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		return mergeAttributes(ctx, tx, "8.8.4.4", nil, nil)
	}); err != nil {
		t.Fatal(err)
	}
	attr, err := f.repo.Attributes(ctx, "8.8.4.4")
	if err != nil || attr.CheckedAt != old || attr.Score != 10 || !domain.FreshResidential(attr, "8.8.4.4", time.Now()) {
		t.Fatalf("classification changed risk freshness: %+v %v", attr, err)
	}
	var n domain.Node
	domain.NodeState{Exit: &domain.ExitState{IP: "1.1.1.1"}, Attributes: attr}.Apply(&n)
	if n.IPPure.IP != "1.1.1.1" || n.IPPure.Classification != nil || n.IPPure.Score != 0 {
		t.Fatal("new exit inherited old IP properties")
	}
}

// A cooldown published by one replica must stop the others. In production the
// clock lives in Redis; here two repositories share one limiter, which is the
// same contract without a Redis dependency in the test.
func TestProviderCooldownIsSharedAcrossRepositories(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	limiter := ratelimit.New(nil)
	f.repo.Limiter = limiter
	until := time.Now().Add(time.Minute)
	if err := f.repo.AfterProvider(ctx, "ipinfo", until); err != nil {
		t.Fatal(err)
	}
	other := &Repository{Pool: f.repo.Pool, Limiter: limiter}
	if err := other.BeforeProvider(ctx, "ipinfo"); !isWaiting(err) {
		t.Fatal("another replica ignored the shared cooldown", err)
	}
	// A shorter cooldown must not re-open the provider early.
	if err := other.AfterProvider(ctx, "ipinfo", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	wait, err := limiter.Allow(ctx, ratelimit.Key("provider", "ipinfo"), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if wait < 30*time.Second {
		t.Fatalf("short cooldown overwrote Retry-After: %s", wait)
	}
}

func TestHeartbeatMakesServiceHealthy(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.repo.Heartbeat(ctx, "connectivity", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Healthy(ctx, "connectivity"); err != nil {
		t.Fatal(err)
	}
}

func (fn roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }
func echoClient(values ...string) *http.Client {
	var mu sync.Mutex
	i := 0
	return &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(values) {
			return nil, fmt.Errorf("unexpected echo")
		}
		v := values[i]
		i++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(v)), Header: make(http.Header)}, nil
	})}
}
func (q *recordingQueue) Push(_ context.Context, _ string, _ bool, ref string, _ time.Time) error {
	q.refs = append(q.refs, ref)
	return nil
}
func (q *recordingQueue) Pop(context.Context, string, bool) (string, error) { return "", nil }
func (q *recordingQueue) Close() error                                      { return nil }

type roundTrip func(*http.Request) (*http.Response, error)

type recordingQueue struct{ refs []string }

func (f *fixture) evidence(t *testing.T, session string) {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	live := domain.ConnectivityState{CheckedAt: stamp, Reachable: true}
	exit := domain.ExitState{CheckedAt: stamp, IP: "8.8.4.4", AddressFamily: 4, Verified: true, Version: 1}
	e := func() error {
		tx, e := f.repo.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, `UPDATE node_explorer_node_registry SET available=true WHERE identity=$1`, f.id); e != nil {
			return e
		}
		lb, _ := json.Marshal(live)
		eb, _ := json.Marshal(exit)
		if _, e := tx.Exec(ctx, `INSERT INTO node_explorer_connectivity VALUES($1,$2,$3) ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, f.id, stamp, lb); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,$2,$3,$4) ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, f.id, exit.IP, stamp, eb); e != nil {
			return e
		}
		if e := mergeAttributes(ctx, tx, exit.IP, []ipcheck.Evidence{{Provider: "ipinfo", IP: exit.IP, CheckedAt: stamp, UsageType: "isp", QueryStatus: "success", Outcome: "supports-residential"}}, nil); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}()
	if e != nil {
		t.Fatal(e)
	}
}
