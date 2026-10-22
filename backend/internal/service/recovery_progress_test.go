package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryRepairsCompletedDependencyWithoutProof(t *testing.T) {
	f := managedFixture(t, 200)
	ctx := context.Background()
	j := queueNormal(t, f)
	if err := f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("start: %v", err)
	}
	session := "recovery:" + j.Ref()
	f.exec(t, `UPDATE node_explorer_work SET state='done' WHERE kind=$1 AND key=$2`, KindFast, session)
	if err := f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("repair: %v", err)
	}
	var state, ref string
	var generation int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT state,generation,id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND key=$2`, KindFast, session).Scan(&state, &generation, &ref); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || generation != 2 {
		t.Fatalf("stalled dependency not repaired: %s %d", state, generation)
	}
	child, err := f.repo.Claim(ctx, KindFast, ref, false)
	if err != nil || child == nil {
		t.Fatal("claim repaired dependency", err)
	}
	if err = f.runner.fast(ctx, *child, false); err != nil {
		t.Fatal(err)
	}
	var live domain.ConnectivityState
	if ready, err := f.repo.Probe(ctx, session, KindLive, &live); err != nil || !ready || !live.Reachable {
		t.Fatalf("no fresh proof after replay: %v %v", ready, err)
	}
	if err = f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("must still require real exit: %v", err)
	}
	var exits int
	_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='exit' AND key=$1`, session).Scan(&exits)
	if exits != 1 {
		t.Fatal("recovery did not advance to exit verification")
	}
}

func TestRecoveryQueueTimeoutReleasesSlotWithoutNodeFailurePenalty(t *testing.T) {
	f := managedFixture(t, 200)
	ctx := context.Background()
	j := queueNormal(t, f)
	j.Payload.StartedAt = time.Now().Add(-7 * time.Hour)
	if err := f.runner.recovery(ctx, j); err != nil {
		t.Fatal(err)
	}
	var result, state string
	var attempts int
	var started bool
	if err := f.repo.Pool.QueryRow(ctx, `SELECT state,payload->>'recoveryResult',(payload->>'recoveryAttempts')::int,payload ? 'startedAt' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state, &result, &attempts, &started); err != nil {
		t.Fatal(err)
	}
	if state != "done" || result != "capacity-timeout" || attempts != 0 || started {
		t.Fatalf("bad timeout outcome: %s %s %d %v", state, result, attempts, started)
	}
	if err := f.runner.scheduleNormalRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	if err := f.repo.Pool.QueryRow(ctx, `SELECT state,due_at FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state, &due); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || time.Until(due) < 50*time.Second || time.Until(due) > time.Minute {
		t.Fatalf("capacity wait escalated node backoff: %s %s", state, due)
	}
}

func TestRecoveryRepairPreservesProofLeaseAndCooldown(t *testing.T) {
	for _, state := range []string{"running", "queued", "done-with-proof"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			p := Payload{Identity: f.id, Session: "recovery:1:1"}
			if err := f.repo.Schedule(ctx, KindExit, p.Session, p, 0, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if state == "running" {
				f.exec(t, `UPDATE node_explorer_work SET state='running',lease_token='active',lease_until=now()+interval '1 minute'`)
			}
			if state == "done-with-proof" {
				f.exec(t, `UPDATE node_explorer_work SET state='done'`)
				if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
					return writeProbe(ctx, tx, p.Session, KindExit, f.id, domain.ExitState{Error: "failed"})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error { return ensureRecoveryProbe(ctx, tx, KindExit, p.Session, p, 0) }); err != nil {
				t.Fatal(err)
			}
			var generation int
			var due time.Time
			if err := f.repo.Pool.QueryRow(ctx, `SELECT generation,due_at FROM node_explorer_work WHERE kind='exit'`).Scan(&generation, &due); err != nil {
				t.Fatal(err)
			}
			if generation != 1 || time.Until(due) < 59*time.Minute {
				t.Fatal("repair changed live, delayed or complete proof work")
			}
		})
	}
}

func TestRecoveryExitBypassesBacklogButLeavesRegularOpportunities(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at)
 SELECT 'exit','background-'||i,jsonb_build_object('identity',$1::text),0,now()-interval '1 hour' FROM generate_series(1,1100) i`, f.id)
	for i := 0; i < 4; i++ {
		session := fmt.Sprintf("recovery:%d:1", i)
		if err := f.repo.Schedule(ctx, KindExit, session, Payload{Identity: f.id, Session: session}, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var regularRef string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE key='background-1'`).Scan(&regularRef); err != nil {
		t.Fatal(err)
	}
	q := &admissionQueue{pop: func(context.Context) (string, error) { return regularRef, nil }}
	r := Runner{Repo: f.repo, Queue: q, Config: Config{Role: "exit-provision", Manager: &transport.ManagerClient{}, ExitConcurrency: 8}}
	r.configureAdmission()
	for i := 0; i < 4; i++ {
		j, release, err := r.nextJob(ctx, KindExit, false)
		if err != nil || j == nil {
			t.Fatal("claim", err)
		}
		if (i < 3) != (j.Payload.Session != "") {
			t.Fatalf("turn %d got session %q", i, j.Payload.Session)
		}
		if err = f.repo.Finish(ctx, *j, nil); err != nil {
			t.Fatal(err)
		}
		release()
	}
	if q.calls.Load() != 1 {
		t.Fatal("recovery depended on backlog delivery or starved ordinary work")
	}
}
