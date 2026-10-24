package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func queueCleanup(t *testing.T, f *fixture) Job {
	t.Helper()
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	if e := f.runner.scheduleCleanup(ctx); e != nil {
		t.Fatal(e)
	}
	var ref string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1`, KindCleanup).Scan(&ref); e != nil {
		t.Fatal(e)
	}
	j, e := f.repo.Claim(ctx, KindCleanup, ref, false)
	if e != nil || j == nil {
		t.Fatalf("claim cleanup: %v", e)
	}
	return *j
}
func cleanupProof(t *testing.T, f *fixture, j Job, proof domain.ConnectivityState) {
	t.Helper()
	if e := f.repo.FinishDependencies(context.Background(), func(tx pgx.Tx) error {
		return writeProbe(context.Background(), tx, "cleanup:"+j.Ref(), KindLive, f.id, proof)
	}); e != nil {
		t.Fatal(e)
	}
}
func tryDelete(t *testing.T, f *fixture, j Job, proof domain.ConnectivityState) bool {
	t.Helper()
	deleted := false
	e := f.repo.Finish(context.Background(), j, func(tx pgx.Tx) error {
		var e error
		deleted, _, e = deleteUnavailable(context.Background(), tx, j, proof, "cleanup:"+j.Ref(), true)
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	return deleted
}
func TestCleanupRequiresSevenDaysAndDeduplicates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, age := range []string{"0 seconds", "6 days 23 hours", "7 days"} {
		f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-$1::interval+interval '1 second'`, age)
		if e := f.runner.scheduleCleanup(ctx); e != nil {
			t.Fatal(e)
		}
		var n int
		_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind=$1`, KindCleanup).Scan(&n)
		if n != 0 {
			t.Fatalf("premature candidate at %s", age)
		}
	}
	j := queueCleanup(t, f)
	if e := f.runner.scheduleCleanup(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind=$1`, KindCleanup).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate cleanup")
	}
	if j.Priority != 0 || j.Payload.StartedAt.IsZero() {
		t.Fatal("cleanup must be admitted in ordinary lane")
	}
}
func TestCleanupWindowIsConfigurable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// A registry whose rows all postdate a column migration cannot satisfy a
	// week; the operator shortens the window instead of waiting it out.
	t.Setenv("NODE_CLEANUP_MIN_AGE_SECONDS", "86400")
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '2 days'`)
	if e := f.runner.scheduleCleanup(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind=$1`, KindCleanup).Scan(&n)
	if n != 1 {
		t.Fatalf("configured window not honoured: %d candidates", n)
	}
}
func TestCleanupDefaultsKeepAWeekOfGrace(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '2 days'`)
	if e := f.runner.scheduleCleanup(ctx); e != nil {
		t.Fatal(e)
	}
	var n int
	_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind=$1`, KindCleanup).Scan(&n)
	if n != 0 {
		t.Fatalf("default window shortened: %d candidates", n)
	}
}
func TestCleanupBatchReader(t *testing.T) {
	if cleanupBatch() != 256 {
		t.Fatalf("default batch: %d", cleanupBatch())
	}
	t.Setenv("NODE_CLEANUP_BATCH", "2048")
	if cleanupBatch() != 2048 {
		t.Fatalf("configured batch: %d", cleanupBatch())
	}
	for _, bad := range []string{"0", "-5", "abc", "999999"} {
		t.Setenv("NODE_CLEANUP_BATCH", bad)
		if v := cleanupBatch(); v < 1 || v > 20000 {
			t.Fatalf("batch %q clamped to %d", bad, v)
		}
	}
	if cleanupInflight() != 32 {
		t.Fatalf("default inflight: %d", cleanupInflight())
	}
	t.Setenv("NODE_CLEANUP_INFLIGHT", "128")
	if cleanupInflight() != 128 {
		t.Fatalf("configured inflight: %d", cleanupInflight())
	}
}
func TestCleanupPublicationHonoursInflight(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	t.Setenv("NODE_CLEANUP_INFLIGHT", "2")
	for i := 0; i < 5; i++ {
		f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at) VALUES($1,$2,'{}',0,now()-interval '1 minute')`, KindCleanup, fmt.Sprintf("job-%d", i))
	}
	q := &publicationQueue{}
	if e := f.repo.Publish(ctx, q, []string{KindCleanup}); e != nil {
		t.Fatal(e)
	}
	if got := len(q.waves); got != 1 || len(q.waves[0]) != 2 {
		t.Fatalf("published %d waves, first %v", got, q.waves)
	}
	// A running job consumes one slot; the wave may only top it up to the limit.
	// Simulate the 30-second republication window instead of sleeping through it.
	f.exec(t, `UPDATE node_explorer_work SET state='running',lease_until=now()+interval '2 minutes',lease_token='x' WHERE key='job-0'`)
	f.exec(t, `UPDATE node_explorer_work SET published_at=NULL WHERE state='queued'`)
	q2 := &publicationQueue{}
	if e := f.repo.Publish(ctx, q2, []string{KindCleanup}); e != nil {
		t.Fatal(e)
	}
	if len(q2.waves) != 1 || len(q2.waves[0]) != 1 {
		t.Fatalf("occupied slot not accounted: %v", q2.waves)
	}
}
func TestCleanupClaimsAreNotSingleFlight(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	t.Setenv("NODE_CLEANUP_INFLIGHT", "3")
	// Two assessments already hold the lane. A third must still be claimable:
	// one stuck probe waiting on capacity used to block every other node.
	for _, key := range []string{"a", "b"} {
		f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at) VALUES($1,$2,'{"startedAt":"2026-10-24T00:00:00Z"}',0,now()-interval '1 minute')`, KindCleanup, key)
	}
	f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at) VALUES($1,'c','{}',0,now()-interval '1 minute')`, KindCleanup)
	var ref string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND key='c'`, KindCleanup).Scan(&ref); e != nil {
		t.Fatal(e)
	}
	j, e := f.repo.Claim(ctx, KindCleanup, ref, false)
	if e != nil || j == nil {
		t.Fatalf("third verification blocked by single-flight admission: %v", e)
	}
}
func TestTCPVerifiableProtocols(t *testing.T) {
	for proto, want := range map[string]bool{
		"http": true, "https": true, "socks5": true, "vless": true, "trojan": true,
		"ss": true, "vmess": true, "anytls": true,
		"hysteria2": false, "tuic": false, "hysteria": false,
	} {
		if got := tcpVerifiable(domain.Node{Type: proto, Server: "192.0.2.1", Port: "443"}); got != want {
			t.Fatalf("%s: %v, want %v", proto, got, want)
		}
	}
	if tcpVerifiable(domain.Node{Type: "http", Port: "443"}) || tcpVerifiable(domain.Node{Type: "http", Server: "192.0.2.1"}) {
		t.Fatal("endpoint without server or port was accepted")
	}
}
func TestCleanupDialVerificationDecidesByEndpoint(t *testing.T) {
	for _, alive := range []bool{true, false} {
		t.Run(fmt.Sprintf("alive=%v", alive), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			var snapshot int
			if e := f.repo.Pool.QueryRow(ctx, `SELECT id FROM node_explorer_snapshots LIMIT 1`).Scan(&snapshot); e != nil {
				t.Fatal(e)
			}
			// A live loopback endpoint stands in for a reachable egress control.
			ref, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ref.Close()
			f.exec(t, `INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index,first_seen_at) VALUES('ref',$1,$2,0,now())`,
				fmt.Sprintf(`{"type":"http","server":"127.0.0.1","port":"%d"}`, ref.Addr().(*net.TCPAddr).Port), snapshot)
			f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES('ref',now(),$1)`,
				fmt.Sprintf(`{"reachable":true,"checkedAt":%q}`, time.Now().UTC().Format(time.RFC3339Nano)))
			port := 1
			if alive {
				ln, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				defer ln.Close()
				port = ln.Addr().(*net.TCPAddr).Port
			} else {
				// Bind then close: the address is free, so a dial is refused.
				ln, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				port = ln.Addr().(*net.TCPAddr).Port
				ln.Close()
			}
			f.exec(t, `UPDATE node_explorer_node_registry SET node=$1, first_seen_at=now()-interval '8 days' WHERE identity=$2`,
				fmt.Sprintf(`{"type":"http","server":"127.0.0.1","port":"%d"}`, port), f.id)
			if e = f.runner.scheduleCleanup(ctx); e != nil {
				t.Fatal(e)
			}
			var ref2 string
			if e = f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND payload->>'identity'=$2`, KindCleanup, f.id).Scan(&ref2); e != nil {
				t.Fatal(e)
			}
			j, e := f.repo.Claim(ctx, KindCleanup, ref2, false)
			if e != nil || j == nil {
				t.Fatalf("claim: %v", e)
			}
			if e = f.runner.handle(ctx, *j, false); e != nil {
				t.Fatalf("handle: %v", e)
			}
			var n int
			_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&n)
			if alive && n != 1 {
				t.Fatal("live endpoint was deleted")
			}
			if !alive && n != 0 {
				t.Fatal("dead endpoint kept")
			}
		})
	}
}
func TestCleanupDeletionVetoes(t *testing.T) {
	for _, scenario := range []string{"reachable", "inconclusive", "stale", "missing-proof", "concurrent-success", "newer-observation", "changed-config", "recreated", "running-probe", "wrong-session", "admitting-probe"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			j := queueCleanup(t, f)
			proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
			switch scenario {
			case "reachable":
				proof.Reachable = true
			case "inconclusive":
				proof.ConfirmedDead = false
			case "stale":
				proof.CheckedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
			case "concurrent-success":
				f.exec(t, `UPDATE node_explorer_node_registry SET last_available_at=now()`)
			case "newer-observation":
				f.evidence(t, "")
				// Two adjacent Go wall-clock reads can land 2ns apart and even
				// backwards on Windows interrupt-time granularity, so the
				// fixture pins the observation strictly newer instead of
				// trusting call ordering.
				f.exec(t, `UPDATE node_explorer_connectivity SET checked_at=now()+interval '5 milliseconds' WHERE identity=$1`, f.id)
			case "changed-config":
				f.exec(t, `UPDATE node_explorer_node_registry SET node=node||'{"name":"changed"}'::jsonb`)
			case "recreated":
				f.exec(t, `UPDATE node_explorer_node_registry SET incarnation='new-instance'`)
			case "running-probe":
				if e := f.repo.Schedule(ctx, KindLive, "other", Payload{Identity: f.id}, 0, time.Now()); e != nil {
					t.Fatal(e)
				}
				f.exec(t, `UPDATE node_explorer_work SET state='running',lease_until=now()+interval '3 minutes' WHERE key='other'`)
			}
			if scenario != "missing-proof" {
				cleanupProof(t, f, j, proof)
			}
			if scenario == "wrong-session" {
				f.exec(t, `UPDATE node_explorer_probe_results SET identity='someone-else'`)
			}
			if scenario == "admitting-probe" {
				tx, e := f.repo.Pool.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext(current_schema()),hashtext($1))`, "node-probe:"+f.id); e != nil {
					t.Fatal(e)
				}
			}
			if tryDelete(t, f, j, proof) {
				t.Fatal("unsafe deletion")
			}
			var n int
			_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&n)
			if n != 1 {
				t.Fatal("node disappeared")
			}
		})
	}
}
func TestCleanupPhysicallyDeletesAndPreservesOtherNodes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var original []byte
	_ = f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_snapshots`).Scan(&original)
	var snap domain.Snapshot
	_ = json.Unmarshal(original, &snap)
	other := snap.Nodes[0]
	other.Server = "192.0.2.2"
	other.Name = "healthy"
	third := other
	third.Server = "192.0.2.3"
	snap.Nodes = append(snap.Nodes, other, third)
	raw, _ := json.Marshal(snap)
	f.exec(t, `UPDATE node_explorer_snapshots SET payload=$1,node_count=3`, string(raw))
	if e := f.repo.SyncRegistry(ctx, f.st); e != nil {
		t.Fatal(e)
	}
	f.evidence(t, "")
	f.exec(t, `INSERT INTO node_explorer_ippure_results SELECT id,i,'{"status":"success"}' FROM node_explorer_snapshots CROSS JOIN generate_series(0,2) i`)

	// An import can commit before registry/membership synchronization catches up.
	f.exec(t, `INSERT INTO node_explorer_snapshots(fingerprint,checked_at,imported_at,node_count,payload)
 SELECT 'pending',checked_at,imported_at,node_count,payload FROM node_explorer_snapshots LIMIT 1`)
	j := queueCleanup(t, f)
	f.exec(t, `UPDATE node_explorer_node_registry SET last_available_at=NULL WHERE identity=$1`, f.id)
	proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	cleanupProof(t, f, j, proof)
	if !tryDelete(t, f, j, proof) {
		t.Fatal("confirmed dead node not deleted")
	}
	var n int
	for _, table := range []string{"node_explorer_node_registry", "node_explorer_connectivity", "node_explorer_exits", "node_explorer_retired_nodes", "node_explorer_probe_results"} {
		if e := f.repo.Pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE identity=$1", table), f.id).Scan(&n); e != nil || n != 0 {
			t.Fatalf("residual %s %d %v", table, n, e)
		}
	}
	if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_ippure_results WHERE node_index IN (0,1)`).Scan(&n); e != nil || n != 2 {
		t.Fatalf("enrichments lost: %d %v", n, e)
	}
	if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_ip_attributes`).Scan(&n); e != nil || n != 1 {
		t.Fatal("shared IP attributes removed")
	}
	if e := f.repo.SyncRegistry(ctx, f.st); e != nil {
		t.Fatal(e)
	}
	if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&n); e != nil || n != 0 {
		t.Fatal("old snapshot resurrected node")
	}

	var members int
	if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_snapshot_members WHERE identity=$1`, f.id).Scan(&members); e != nil || members != 0 {
		t.Fatal("stale source membership survived")
	}
	rows, e := f.repo.Pool.Query(ctx, `SELECT payload FROM node_explorer_snapshots`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			t.Fatal(e)
		}
		var snap domain.Snapshot
		if e = json.Unmarshal(b, &snap); e != nil {
			t.Fatal(e)
		}
		for _, n := range snap.Nodes {
			if domain.AggregateIdentity(n) == f.id {
				t.Fatal("connection copy survived physical deletion")
			}
		}
	}

}

type cleanupEngine struct{ client *http.Client }

func (e cleanupEngine) Name() string         { return "test" }
func (e cleanupEngine) Supports(string) bool { return true }
func (e cleanupEngine) Reserve(ctx context.Context, nodes []domain.Node) (map[int]*transport.Outbound, func(), error) {
	m := map[int]*transport.Outbound{}
	for i := range nodes {
		m[i] = &transport.Outbound{Client: e.client}
	}
	return m, func() {}, nil
}
func TestSuccessfulLiveProbePersistsMonotonicAvailability(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	f.runner.Config.HighRouter = transport.NewRouter(cleanupEngine{client})
	f.runner.Config.Timeout = time.Second
	if e := f.repo.Schedule(ctx, KindLive, "success", Payload{Identity: f.id}, 1, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := f.runner.live(ctx, f.claim(t, KindLive), true); e != nil {
		t.Fatal(e)
	}
	var stamp time.Time
	var incarnation string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT last_available_at,incarnation FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&stamp, &incarnation); e != nil || time.Since(stamp) > time.Minute {
		t.Fatalf("success not persisted: %v", e)
	}
	if e := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		return recordAvailable(ctx, tx, f.id, incarnation, stamp.Add(-time.Hour).Format(time.RFC3339Nano))
	}); e != nil {
		t.Fatal(e)
	}
	var after time.Time
	_ = f.repo.Pool.QueryRow(ctx, `SELECT last_available_at FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&after)
	if !after.Equal(stamp) {
		t.Fatal("last success moved backwards")
	}
}

func TestCleanupCoordinatorUsesFreshNormalProbeAndDefersUncertainty(t *testing.T) {
	for _, outcome := range []string{"success", "inconclusive", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			// QUIC transports keep the kernel coordinator path this test covers;
			// TCP endpoints are now verified by a direct dial instead.
			f.exec(t, `UPDATE node_explorer_node_registry SET node=jsonb_set(node,'{type}','"hysteria2"') WHERE identity=$1`, f.id)
			j := queueCleanup(t, f)
			if e := f.runner.cleanup(ctx, j); !isWaiting(e) {
				t.Fatalf("initial probe: %v", e)
			}
			var count int
			if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='connectivity' AND priority=0`).Scan(&count); e != nil || count != 1 {
				t.Fatalf("ordinary fresh probe: %d %v", count, e)
			}
			if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind IN ('exit','attributes','risk')`).Scan(&count); e != nil || count != 0 {
				t.Fatal("cleanup queried IP providers")
			}
			proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: outcome == "failed", Reachable: outcome == "success"}
			cleanupProof(t, f, j, proof)
			f.exec(t, `UPDATE node_explorer_work SET state='done' WHERE kind='connectivity'`)
			if outcome == "success" {
				f.exec(t, `UPDATE node_explorer_node_registry SET last_available_at=$2 WHERE identity=$1`, f.id, proof.CheckedAt)
			}
			if e := f.runner.cleanup(ctx, j); e != nil {
				t.Fatal(e)
			}
			if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&count); e != nil {
				t.Fatal(e)
			}
			if (count == 0) != (outcome == "failed") {
				t.Fatalf("outcome=%s remaining=%d", outcome, count)
			}
			if outcome != "failed" {
				var due time.Time
				var generation int64
				if e := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&due, &generation); e != nil {
					t.Fatal(e)
				}
				if generation != j.Generation+1 || time.Until(due) < 59*time.Minute {
					t.Fatal("uncertainty not postponed as a fresh generation")
				}
			}
		})
	}
}

func TestInconclusiveLiveFailureDoesNotChangeLastSuccess(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour).UTC()
	f.exec(t, `UPDATE node_explorer_node_registry SET last_available_at=$1`, old)
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	f.runner.Config.HighRouter = transport.NewRouter(cleanupEngine{client})
	f.runner.Config.Timeout = time.Second
	f.runner.Config.RetryDelay = time.Millisecond
	if e := f.repo.Schedule(ctx, KindLive, "failure", Payload{Identity: f.id}, 1, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := f.runner.live(ctx, f.claim(t, KindLive), true); e != nil {
		t.Fatal(e)
	}
	var after time.Time
	var raw []byte
	if e := f.repo.Pool.QueryRow(ctx, `SELECT n.last_available_at,c.payload FROM node_explorer_node_registry n JOIN node_explorer_connectivity c USING(identity) WHERE identity=$1`, f.id).Scan(&after, &raw); e != nil {
		t.Fatal(e)
	}
	if after.Sub(old) > time.Microsecond || old.Sub(after) > time.Microsecond {
		t.Fatal("failure overwrote success")
	}
	var live domain.ConnectivityState
	_ = json.Unmarshal(raw, &live)
	if live.Reachable || live.ConfirmedDead {
		t.Fatal("missing controls confirmed deletion")
	}
}
