package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func claimNormal(t *testing.T, f *fixture) Job {
	t.Helper()
	var ref string
	if err := f.repo.Pool.QueryRow(context.Background(), `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind='normal-recovery' AND state='queued' ORDER BY id LIMIT 1`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.repo.Claim(context.Background(), KindNormalRecovery, ref, false)
	if err != nil || j == nil {
		t.Fatalf("normal claim: %v", err)
	}
	return *j
}

func queueNormal(t *testing.T, f *fixture) Job {
	t.Helper()
	f.retirement(t)
	f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
	f.runner.Config.Role = "normal-recovery"
	if err := f.runner.schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE node_explorer_work SET due_at=now() WHERE kind='normal-recovery'`)
	return claimNormal(t, f)
}

func completeNormal(t *testing.T, f *fixture, j Job, classification string) {
	t.Helper()
	ctx := context.Background()
	if err := f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("initial dependencies: %v", err)
	}
	session := "recovery:" + j.Ref()
	f.evidence(t, session)
	f.evidence(t, session+":verify")
	if classification == "missing" {
		f.exec(t, `DELETE FROM node_explorer_ip_attributes`)
	} else {
		f.exec(t, `UPDATE node_explorer_ip_attributes SET payload='{"ip":"8.8.4.4","asn":"AS13335","classification":{"ip":"8.8.4.4","state":"datacenter"}}'`)
	}
	if err := f.runner.recovery(ctx, j); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->>'recoveryMode' FROM node_explorer_work WHERE kind='lifecycle'`).Scan(&mode); err != nil || mode != "normal" {
		t.Fatalf("mode: %s %v", mode, err)
	}
	var high int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind IN ('connectivity','exit') AND priority>0`).Scan(&high); err != nil || high != 0 {
		t.Fatalf("used high probe lane: %d %v", high, err)
	}
}

func TestNormalRecoveryRestoresWithoutResidentialClassification(t *testing.T) {
	for _, classification := range []string{"hosting", "missing"} {
		t.Run(classification, func(t *testing.T) {
			f := setup(t)
			j := queueNormal(t, f)
			if j.Payload.StartedAt.IsZero() {
				t.Fatal("start not persisted")
			}
			completeNormal(t, f, j, classification)
			transition := f.claim(t, KindLifecycle)
			if err := f.runner.lifecycle(context.Background(), transition); err != nil {
				t.Fatal(err)
			}
			target, err := f.repo.Target(context.Background(), f.id)
			if err != nil || target.Retired != nil {
				t.Fatalf("normal not restored: %+v %v", target, err)
			}
			var history int
			if err = f.repo.Pool.QueryRow(context.Background(), `SELECT jsonb_array_length(payload->'recoveryHistory') FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&history); err != nil || history != 1 {
				t.Fatal("missing history", err)
			}
		})
	}
}

func TestNormalRecoveryLifecycleRechecksFences(t *testing.T) {
	for _, change := range []string{"manual", "new-retirement", "source-deleted", "unavailable", "unreachable", "exit-changed", "stale-proof"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			j := queueNormal(t, f)
			completeNormal(t, f, j, "hosting")
			switch change {
			case "manual":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual'`)
			case "new-retirement":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET retired_at='new-retirement'`)
			case "source-deleted":
				f.exec(t, `DELETE FROM node_explorer_snapshots`)
			case "unavailable":
				f.exec(t, `UPDATE node_explorer_node_registry SET available=false`)
			case "unreachable":
				f.exec(t, `UPDATE node_explorer_connectivity SET payload=jsonb_set(payload,'{reachable}','false')`)
			case "exit-changed":
				f.exec(t, `UPDATE node_explorer_exits SET version=version+1`)
			case "stale-proof":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{checkedAt}','"2000-01-01T00:00:00Z"')`)
			}
			transition := f.claim(t, KindLifecycle)
			if err := f.runner.lifecycle(context.Background(), transition); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_retired_nodes`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("fence %s failed: %d %v", change, count, err)
			}
		})
	}
}

func TestNormalRecoveryRoutingAndPersistentCadence(t *testing.T) {
	for _, scenario := range []string{"nonhistorical", "hosting", "nonresidential", "residential", "transient", "manual", "source-deleted", "unavailable", "fast-active"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			p := f.retirement(t)
			want := false
			switch scenario {
			case "nonhistorical":
				want = true
				f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
			case "hosting", "nonresidential", "transient", "fast-active":
				result := "probe-failed"
				if scenario == "hosting" {
					result = "skipped-datacenter-asn"
					want = true
				}
				if scenario == "nonresidential" {
					result = "not-residential"
					want = true
				}
				p.RecoveryResult = result
				if err := f.repo.Schedule(context.Background(), KindRecovery, f.id+":"+p.RetiredAt, p, 1, time.Now()); err != nil {
					t.Fatal(err)
				}
				if scenario != "fast-active" {
					f.exec(t, `UPDATE node_explorer_work SET state='done'`)
				} else {
					f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
				}
			case "manual":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual'`)
			case "source-deleted":
				f.exec(t, `DELETE FROM node_explorer_snapshots`)
			case "unavailable":
				f.exec(t, `UPDATE node_explorer_node_registry SET available=false`)
			}
			f.runner.Config.Role = "normal-recovery"
			if err := f.runner.schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != want {
				t.Fatalf("routing: count %d want %v", count, want)
			}
			if !want {
				return
			}
			var due, after time.Time
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT due_at FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&due); err != nil {
				t.Fatal(err)
			}
			if due.Before(time.Now().Add(-time.Second)) || due.After(time.Now().Add(5*time.Minute)) {
				t.Fatalf("first attempt too early/late: %v", due)
			}
			if err := f.runner.schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT due_at FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&after); err != nil || !due.Equal(after) {
				t.Fatal("restart moved deadline", err)
			}
		})
	}
}

func TestNormalRecoveryRetriesWithFreshProofsAndNoFastPromotion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j := queueNormal(t, f)
	if err := f.repo.Finish(ctx, j, func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, j, "probe-failed", "") }); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var generation int64
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&due, &generation); err != nil {
		t.Fatal(err)
	}
	if due.Before(time.Now().Add(19*time.Minute)) || generation != j.Generation+1 {
		t.Fatalf("wrong retry deadline/generation: %v %d", due, generation)
	}
	next, err := f.repo.Claim(ctx, KindNormalRecovery, fmt.Sprintf("%d:%d", j.ID, generation), false)
	if err != nil || next != nil {
		t.Fatal("claimed before interval", err)
	}
	f.runner.Config.Role = "residential"
	if err = f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var high int
	if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='recovery'`).Scan(&high); err != nil || high != 0 {
		t.Fatalf("nonresidential in fast service: %d %v", high, err)
	}
	f.evidence(t, "recovery:"+j.Ref())
	f.exec(t, `UPDATE node_explorer_work SET due_at=now() WHERE id=$1`, j.ID)
	nextJob := claimNormal(t, f)
	if err = f.runner.recovery(ctx, nextJob); !isWaiting(err) {
		t.Fatalf("reused old proof: %v", err)
	}
}

func TestNormalAdmissionIsIndependentAndBoundedAcrossReplicas(t *testing.T) {
	for _, limit := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			f.repo.NormalRecoveryConcurrency = limit
			replica := &Repository{Pool: f.repo.Pool, NormalRecoveryConcurrency: limit}
			p := f.retirement(t)
			var refs []string
			for i := 0; i < 8; i++ {
				if err := f.repo.Schedule(ctx, KindNormalRecovery, fmt.Sprint(i), p, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := f.repo.Pool.Query(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var ref string
				if err = rows.Scan(&ref); err != nil {
					t.Fatal(err)
				}
				refs = append(refs, ref)
			}
			rows.Close()
			var wg sync.WaitGroup
			claims := make(chan *Job, len(refs))
			for i, ref := range refs {
				wg.Add(1)
				go func(i int, ref string) {
					defer wg.Done()
					repo := f.repo
					if i%2 == 0 {
						repo = replica
					}
					j, e := repo.Claim(ctx, KindNormalRecovery, ref, false)
					if e != nil {
						t.Error(e)
					}
					claims <- j
				}(i, ref)
			}
			wg.Wait()
			close(claims)
			var admitted []*Job
			for j := range claims {
				if j != nil {
					admitted = append(admitted, j)
				}
			}
			if len(admitted) != limit {
				t.Fatalf("admitted %d want %d", len(admitted), limit)
			}
			// Waiting for dependencies still owns a full-assessment slot.
			if err = f.repo.Retry(ctx, *admitted[0], 0, "dependency"); err != nil {
				t.Fatal(err)
			}
			var extra string
			if err = f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE NOT(payload ? 'startedAt') ORDER BY id LIMIT 1`).Scan(&extra); err != nil {
				t.Fatal(err)
			}
			if j, e := replica.Claim(ctx, KindNormalRecovery, extra, false); e != nil || j != nil {
				t.Fatalf("exceeded waiting admission: %v %v", j, e)
			}
			resumed, e := replica.Claim(ctx, KindNormalRecovery, admitted[0].Ref(), false)
			if e != nil || resumed == nil {
				t.Fatalf("cannot resume full lane: %v", e)
			}
			if e = replica.Finish(ctx, *resumed, nil); e != nil {
				t.Fatal(e)
			}
			if j, e := replica.Claim(ctx, KindNormalRecovery, extra, false); e != nil || j == nil {
				t.Fatalf("slot not released: %v", e)
			}
			if err = f.repo.Schedule(ctx, KindRecovery, "fast", p, 1, time.Now()); err != nil {
				t.Fatal(err)
			}
			if f.claim(t, KindRecovery).Payload.StartedAt.IsZero() {
				t.Fatal("normal blocked fast")
			}
		})
	}
}

func TestNormalRecoveryGatesExitOnFreshSuccessfulConnectivity(t *testing.T) {
	for _, scenario := range []string{"pending", "unreachable", "stale", "error", "success"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			j := queueNormal(t, f)
			session := "recovery:" + j.Ref()
			if err := f.runner.recovery(ctx, j); !isWaiting(err) {
				t.Fatalf("initial: %v", err)
			}
			var exits int
			if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='exit'`).Scan(&exits); err != nil || exits != 0 {
				t.Fatalf("eager exit: %d %v", exits, err)
			}
			if scenario == "pending" {
				return
			}
			f.evidence(t, session)
			f.exec(t, `DELETE FROM node_explorer_probe_results WHERE kind='exit'`)
			switch scenario {
			case "unreachable":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{reachable}','false')`)
			case "stale":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{checkedAt}','"2000-01-01T00:00:00Z"')`)
			case "error":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=payload || '{"error":"inconclusive"}'`)
			}
			err := f.runner.recovery(ctx, j)
			if scenario != "success" {
				if err != nil {
					t.Fatal(err)
				}
				var result string
				if err = f.repo.Pool.QueryRow(ctx, `SELECT payload->>'recoveryResult' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&result); err != nil || result != "probe-failed" {
					t.Fatalf("accepted failed connectivity: %s %v", result, err)
				}
				if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='exit'`).Scan(&exits); err != nil || exits != 0 {
					t.Fatalf("failed node requested exit: %d %v", exits, err)
				}
				return
			}
			if !isWaiting(err) {
				t.Fatalf("first exit not awaited: %v", err)
			}
			f.evidence(t, session)
			if err = f.runner.recovery(ctx, j); !isWaiting(err) {
				t.Fatalf("final exit not awaited: %v", err)
			}
			f.evidence(t, session+":verify")
			if err = f.runner.recovery(ctx, j); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShorterRecoveryIntervalAdvancesOnlyUnstartedJobs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.retirement(t)
	f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
	f.runner.Config.Role = "normal-recovery"
	f.runner.Config.NormalRecoveryInterval = 6 * time.Hour
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	f.runner.Config.NormalRecoveryInterval = 20 * time.Minute
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var generation int64
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&due, &generation); err != nil || due.After(time.Now().Add(20*time.Minute)) || generation != 2 {
		t.Fatalf("old cadence retained: %v %d %v", due, generation, err)
	}
	f.exec(t, `UPDATE node_explorer_work SET due_at=now()+interval '6 hours',payload=payload||jsonb_build_object('startedAt',now()) WHERE kind='normal-recovery'`)
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&due, &generation); err != nil || due.Before(time.Now().Add(5*time.Hour)) || generation != 2 {
		t.Fatalf("running session replaced: %v %d %v", due, generation, err)
	}
}
