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
			if due.Before(time.Now().Add(4*time.Hour)) || due.After(time.Now().Add(6*time.Hour)) {
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
	if due.Before(time.Now().Add(5*time.Hour+59*time.Minute)) || generation != j.Generation+1 {
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
	f := setup(t)
	ctx := context.Background()
	p := f.retirement(t)
	var refs []string
	for i := 0; i < 8; i++ {
		if err := f.repo.Schedule(ctx, KindNormalRecovery, fmt.Sprint(i), p, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.repo.Pool.Query(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work`)
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
	for _, ref := range refs {
		wg.Add(1)
		go func(ref string) {
			defer wg.Done()
			j, e := f.repo.Claim(ctx, KindNormalRecovery, ref, false)
			if e != nil {
				t.Error(e)
			}
			claims <- j
		}(ref)
	}
	wg.Wait()
	close(claims)
	count := 0
	for j := range claims {
		if j != nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("admitted %d normal assessments", count)
	}
	if err = f.repo.Schedule(ctx, KindRecovery, "fast", p, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if f.claim(t, KindRecovery).Payload.StartedAt.IsZero() {
		t.Fatal("normal blocked fast")
	}
}
