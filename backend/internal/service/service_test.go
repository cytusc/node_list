package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/ratelimit"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/taskqueue"
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
func (f *fixture) evidence(t *testing.T, session string) {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	live := domain.ConnectivityState{CheckedAt: stamp, Reachable: true}
	exit := domain.ExitState{CheckedAt: stamp, IP: "8.8.4.4", AddressFamily: 4, Verified: true, Version: 1}
	e := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		lb, _ := json.Marshal(live)
		eb, _ := json.Marshal(exit)
		if _, e := tx.Exec(ctx, `INSERT INTO node_explorer_connectivity VALUES($1,$2,$3) ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, f.id, stamp, lb); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,$2,$3,$4) ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, f.id, exit.IP, stamp, eb); e != nil {
			return e
		}
		if e := writeProbe(ctx, tx, session, KindLive, f.id, live); e != nil {
			return e
		}
		if e := writeProbe(ctx, tx, session, KindExit, f.id, exit); e != nil {
			return e
		}
		return mergeAttributes(ctx, tx, exit.IP, []ipcheck.Evidence{{Provider: "ipinfo", IP: exit.IP, CheckedAt: stamp, UsageType: "isp", QueryStatus: "success", Outcome: "supports-residential"}}, nil)
	})
	if e != nil {
		t.Fatal(e)
	}
}
func (f *fixture) retirement(t *testing.T) Payload {
	t.Helper()
	p := Payload{Identity: f.id, RetiredAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), Reason: store.RetireRestoreReason}
	f.exec(t, `INSERT INTO node_explorer_retired_nodes VALUES($1,$2,$3)`, p.Identity, p.RetiredAt, p.Reason)
	f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=true WHERE identity=$1`, p.Identity)
	return p
}

func TestOneCompleteAssessmentRestoresImmediatelyWithoutRisk(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.retirement(t)
	if e := f.repo.Schedule(ctx, KindRecovery, "cycle", p, 1, time.Now()); e != nil {
		t.Fatal(e)
	}
	j := f.claim(t, KindRecovery)
	if j.Payload.StartedAt.IsZero() {
		t.Fatal("assessment start was not persisted")
	}
	if e := f.runner.recovery(ctx, j); !isWaiting(e) {
		t.Fatalf("want initial dependencies: %v", e)
	}
	session := "recovery:" + j.Ref()
	f.evidence(t, session)
	attr, e := f.repo.Attributes(ctx, "8.8.4.4")
	if e != nil || !domain.FreshResidential(attr, "8.8.4.4", time.Now()) {
		t.Fatalf("fixture classification: %+v %v", attr, e)
	}
	if attr.Status == "success" {
		t.Fatal("test must not depend on risk success")
	}
	if e = f.runner.recovery(ctx, j); !isWaiting(e) {
		t.Fatalf("want final exit dependency: %v", e)
	}
	f.evidence(t, session+":verify")
	if e = f.runner.recovery(ctx, j); e != nil {
		t.Fatal(e)
	}
	transition := f.claim(t, KindLifecycle)
	if e = f.runner.lifecycle(ctx, transition); e != nil {
		t.Fatal(e)
	}
	target, e := f.repo.Target(ctx, f.id)
	if e != nil || target.Retired != nil {
		t.Fatalf("not restored immediately: %+v %v", target, e)
	}
	// A lifecycle change is also visible to the legacy store without rewriting history.
	if e = f.st.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	aggregate := f.st.Aggregate(false)
	if len(aggregate.Nodes) != 1 || !domain.Preferred(aggregate.Nodes[0], time.Now()) {
		t.Fatalf("runtime facts missing: %+v", aggregate.Nodes)
	}
	historical, e := f.st.Get(nil)
	if e != nil || historical.Nodes[0].Connectivity != nil || historical.Nodes[0].IPPure != nil {
		t.Fatal("runtime facts rewrote immutable history")
	}
}

func isWaiting(e error) bool { var w waiting; return errors.As(e, &w) }

func TestLifecycleRejectsInvalidatedProofsAndManualDisable(t *testing.T) {
	for _, scenario := range []string{"manual", "new-retirement", "changed-ip", "changed-version", "stale-exit", "ipv6", "failed-live", "old-voting", "dry-run", "deleted-source", "hosting-asn"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			p := f.retirement(t)
			p.Action = "restore"
			p.Session = "cycle"
			p.IP = "8.8.4.4"
			p.ExitVersion = 1
			f.evidence(t, p.Session+":verify")
			switch scenario {
			case "manual":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual disable'`)
			case "new-retirement":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET retired_at='new epoch'`)
			case "changed-ip":
				f.exec(t, `UPDATE node_explorer_exits SET ip='8.8.8.8',payload=jsonb_set(payload,'{ip}','"8.8.8.8"')`)
			case "changed-version":
				f.exec(t, `UPDATE node_explorer_exits SET version=2`)
			case "stale-exit":
				f.exec(t, `UPDATE node_explorer_exits SET payload=jsonb_set(payload,'{checkedAt}',to_jsonb($1::text))`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
			case "ipv6":
				f.exec(t, `UPDATE node_explorer_exits SET payload=jsonb_set(payload,'{addressFamily}','6')`)
			case "failed-live":
				f.exec(t, `UPDATE node_explorer_connectivity SET payload=jsonb_set(payload,'{reachable}','false')`)
			case "old-voting":
				f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=jsonb_set(payload,'{classification,version}','1')`)
			case "dry-run":
				f.runner.Config.DryRun = true
			case "hosting-asn":
				f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=payload || '{"asn":"AS12876"}'::jsonb`)
			case "deleted-source":
				f.exec(t, `DELETE FROM node_explorer_snapshots`)
			}
			if e := f.repo.Schedule(ctx, KindLifecycle, "restore", p, 1, time.Now()); e != nil {
				t.Fatal(e)
			}
			if e := f.runner.lifecycle(ctx, f.claim(t, KindLifecycle)); e != nil {
				t.Fatal(e)
			}
			var count int
			if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_retired_nodes`).Scan(&count); e != nil || count != 1 {
				t.Fatalf("unsafe restore: %d %v", count, e)
			}
		})
	}
}

func TestLateRetirementCannotOverrideNewConnectivity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "alive")
	p := Payload{Identity: f.id, Action: "retire", CheckedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}
	if e := f.repo.Schedule(ctx, KindLifecycle, "stale-failure", p, 1, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := f.runner.lifecycle(ctx, f.claim(t, KindLifecycle)); e != nil {
		t.Fatal(e)
	}
	target, e := f.repo.Target(ctx, f.id)
	if e != nil || target.Retired != nil {
		t.Fatal("old failed probe retired a newly reachable node")
	}
}

func TestDuplicateDeliveryAndExpiredLeasesFenceWrites(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if e := f.repo.Schedule(ctx, KindLive, "one", Payload{Identity: f.id}, 1, time.Now()); e != nil {
		t.Fatal(e)
	}
	first := f.claim(t, KindLive)
	duplicate, e := f.repo.Claim(ctx, KindLive, first.Ref(), true)
	if e != nil || duplicate != nil {
		t.Fatal("duplicate delivery acquired live lease")
	}
	f.exec(t, `UPDATE node_explorer_work SET lease_until=now()-interval '1 second'`)
	replacement, e := f.repo.Claim(ctx, KindLive, first.Ref(), true)
	if e != nil || replacement == nil {
		t.Fatal(e)
	}
	called := false
	e = f.repo.Finish(ctx, first, func(tx pgx.Tx) error { called = true; return nil })
	if !errors.Is(e, ErrLeaseLost) || called {
		t.Fatal("expired worker was allowed to commit")
	}
	if e = f.repo.Finish(ctx, *replacement, func(tx pgx.Tx) error {
		return schedule(ctx, tx, KindExit, "downstream", Payload{Identity: f.id}, 1, time.Now())
	}); e != nil {
		t.Fatal(e)
	}
	if e = f.repo.Finish(ctx, *replacement, nil); !errors.Is(e, ErrLeaseLost) {
		t.Fatal("completed task committed twice")
	}
	// A stale normal-lane reference cannot steal promoted residential work.
	if e = f.repo.Schedule(ctx, KindLive, "promoted", Payload{}, 1, time.Now()); e != nil {
		t.Fatal(e)
	}
	var ref string
	_ = f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE key='promoted'`).Scan(&ref)
	if stolen, e := f.repo.Claim(ctx, KindLive, ref, false); e != nil || stolen != nil {
		t.Fatal("normal worker stole residential work")
	}
}

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

func TestRedisLostDeliveryIsRepublishedFromPostgres(t *testing.T) {
	address := os.Getenv("NODE_EXPLORER_TEST_REDIS")
	if address == "" {
		t.Skip("set NODE_EXPLORER_TEST_REDIS to an isolated Redis")
	}
	f := setup(t)
	ctx := context.Background()
	q, err := taskqueue.New(ctx, address, fmt.Sprintf("replay-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err = f.repo.Schedule(ctx, KindLive, "lost", Payload{Identity: f.id}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.Publish(ctx, q, []string{KindLive}); err != nil {
		t.Fatal(err)
	}
	lost, err := q.Pop(ctx, KindLive, true)
	if err != nil || lost == "" {
		t.Fatalf("initial delivery: %q %v", lost, err)
	}
	// Simulate a consumer crash after Redis removes the message but before CAS.
	// Unchanged tasks are repaired after the bounded reconciliation interval.
	f.exec(t, `UPDATE node_explorer_work SET published_at=now()-interval '31 seconds' WHERE key='lost'`)
	if err = f.repo.Publish(ctx, q, []string{KindLive}); err != nil {
		t.Fatal(err)
	}
	replayed, err := q.Pop(ctx, KindLive, true)
	if err != nil || replayed != lost {
		t.Fatalf("lost task was not repaired: %q %v", replayed, err)
	}
	j, err := f.repo.Claim(ctx, KindLive, replayed, true)
	if err != nil || j == nil {
		t.Fatal("replayed task cannot be claimed", err)
	}
	if err = f.repo.Finish(ctx, *j, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.Publish(ctx, q, []string{KindLive}); err != nil {
		t.Fatal(err)
	}
	if ref, err := q.Pop(ctx, KindLive, true); err != nil || ref != "" {
		t.Fatal("finished task was republished")
	}
}

func TestSharedIPDedupAndAtomicRollback(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		if err := scheduleIP(ctx, tx, "8.8.4.4", f.id, 0); err != nil {
			return err
		}
		return scheduleIP(ctx, tx, "8.8.4.4", "another-node", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	var count, priority int
	if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*),min(priority) FROM node_explorer_work WHERE key='8.8.4.4'`).Scan(&count, &priority); err != nil || count != 2 || priority != 1 {
		t.Fatalf("IP work was not deduplicated/promoted: %d %d %v", count, priority, err)
	}
	j := f.claim(t, KindAttributes)
	sentinel := errors.New("rollback")
	err = f.repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if err := mergeAttributes(ctx, tx, "8.8.4.4", nil, nil); err != nil {
			return err
		}
		if err := once(ctx, tx, KindLifecycle, "must-rollback", Payload{}, 1); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err = f.repo.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM node_explorer_ip_attributes)+(SELECT count(*) FROM node_explorer_work WHERE key='must-rollback')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial facts/downstream work committed", err)
	}
	if err = f.repo.Finish(ctx, j, nil); err != nil {
		t.Fatal("rollback discarded the lease", err)
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

func TestNormalRecoveryDoesNotOccupyResidentialProbeLane(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.retirement(t)
	f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
	if err := f.repo.Schedule(ctx, KindRecovery, "normal-cycle", p, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.repo.Claim(ctx, KindRecovery, ref, false)
	if err != nil || j == nil {
		t.Fatal(err)
	}
	if err = f.runner.recovery(ctx, *j); !isWaiting(err) {
		t.Fatal(err)
	}
	var high int
	if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind=ANY($1) AND priority>0`, []string{KindLive, KindExit}).Scan(&high); err != nil || high != 0 {
		t.Fatal("normal recovery stole residential capacity", err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

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
func TestStrongExitRequiresAgreementAndDistinctHostEgress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		proxy, host []string
		ok          bool
	}{
		{"valid", []string{"8.8.8.8", "8.8.8.8"}, []string{"1.1.1.1", "1.1.1.1"}, true},
		{"rotating", []string{"8.8.8.8", "8.8.4.4"}, nil, false},
		{"host-leak", []string{"8.8.8.8", "8.8.8.8"}, []string{"8.8.8.8", "8.8.8.8"}, false},
		{"private", []string{"192.168.1.1"}, nil, false},
		{"ipv6", []string{"2606:4700:4700::1111"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := StrongExit(context.Background(), echoClient(tc.proxy...), echoClient(tc.host...))
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected exit validation: %v", err)
			}
		})
	}
}

// A startup backlog must not start clocks and create probes for every candidate.
func TestRecoveryAdmissionBoundsBacklogAndReservesResidentialLane(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := f.retirement(t)
	refs := make([]string, 40)
	for i := range refs {
		key := fmt.Sprintf("backlog-%d", i)
		if err := f.repo.Schedule(ctx, KindRecovery, key, p, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND key=$2`, KindRecovery, key).Scan(&refs[i]); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := []Job{}
	for _, ref := range refs {
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			j, err := f.repo.Claim(ctx, KindRecovery, ref, false)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
			}
			if j != nil {
				admitted = append(admitted, *j)
			}
		}(ref)
	}
	wg.Wait()
	if len(admitted) != 2 {
		t.Fatalf("admitted %d backlog assessments, want 2", len(admitted))
	}
	var clocks int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE payload ? 'startedAt'`).Scan(&clocks); err != nil || clocks != 2 {
		t.Fatalf("unadmitted clocks started: %d %v", clocks, err)
	}
	// A residential assessment remains admissible while the ordinary lane is full.
	if err := f.repo.Schedule(ctx, KindRecovery, "residential", p, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	var highRef string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE key='residential'`).Scan(&highRef); err != nil {
		t.Fatal(err)
	}
	high, err := f.repo.Claim(ctx, KindRecovery, highRef, true)
	if err != nil || high == nil {
		t.Fatal("ordinary backlog blocked residential admission", err)
	}
	if high.Payload.StartedAt.IsZero() {
		t.Fatal("residential admission did not start assessment")
	}
	// Retried admitted work must be published before untouched old backlog rows.
	retry := admitted[0]
	if err := f.repo.Retry(ctx, retry, time.Second, "dependency"); err != nil {
		t.Fatal(err)
	}
	q := &recordingQueue{}
	if err := f.repo.Publish(ctx, q, []string{KindRecovery}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	found := false
	for _, ref := range q.refs {
		if ref == retry.Ref() {
			found = true
		}
	}
	mu.Unlock()
	if !found || len(q.refs) > 4 {
		t.Fatalf("admitted retry starved or backlog flooded: %v", q.refs)
	}
	// Finishing a cycle releases exactly one admission slot.
	if err := f.repo.Finish(ctx, admitted[1], nil); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ref := range refs {
		j, err := f.repo.Claim(ctx, KindRecovery, ref, false)
		if err != nil {
			t.Fatal(err)
		}
		if j != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("released slot admitted %d new cycles", count)
	}
}

type recordingQueue struct{ refs []string }

func (q *recordingQueue) Push(_ context.Context, _ string, _ bool, ref string, _ time.Time) error {
	q.refs = append(q.refs, ref)
	return nil
}
func (q *recordingQueue) Pop(context.Context, string, bool) (string, error) { return "", nil }
func (q *recordingQueue) Close() error                                      { return nil }
