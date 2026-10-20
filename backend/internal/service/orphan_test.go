package service

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMissingNodeProbeCompletesAndStopsPublication(t *testing.T) {
	for _, kind := range []string{KindFast, KindLive, KindExit} {
		for _, high := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/high=%v", kind, high), func(t *testing.T) {
				f := setup(t)
				ctx := context.Background()
				priority := 0
				if high {
					priority = 1
				}
				if err := f.repo.Schedule(ctx, kind, "orphan", Payload{Identity: "missing-node"}, priority, time.Now().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				var ref string
				if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work`).Scan(&ref); err != nil {
					t.Fatal(err)
				}
				job, err := f.repo.Claim(ctx, kind, ref, high)
				if err != nil || job != nil {
					t.Fatalf("orphan claim: %+v %v", job, err)
				}
				var done bool
				if err := f.repo.Pool.QueryRow(ctx, `SELECT state='done' AND lease_token IS NULL AND lease_until IS NULL AND error='' FROM node_explorer_work`).Scan(&done); err != nil || !done {
					t.Fatalf("orphan not completed: done=%v err=%v", done, err)
				}
				q := &publicationQueue{}
				if err := f.repo.Publish(ctx, q, []string{kind}); err != nil || len(q.waves) != 0 {
					t.Fatalf("orphan republished: %+v %v", q.waves, err)
				}
				// A later import can schedule the same key for a valid node.
				if err := f.repo.Schedule(ctx, kind, "orphan", Payload{Identity: f.id}, priority, time.Now().Add(-time.Second)); err != nil {
					t.Fatal(err)
				}
				if stale, err := f.repo.Claim(ctx, kind, ref, high); err != nil || stale != nil {
					t.Fatalf("stale generation claimed: %+v %v", stale, err)
				}
				if err := f.repo.Publish(ctx, q, []string{kind}); err != nil || len(q.waves) != 1 {
					t.Fatalf("reschedule publication: %+v %v", q.waves, err)
				}
				if job, err := f.repo.Claim(ctx, kind, q.waves[0][0].Ref, high); err != nil || job == nil {
					t.Fatalf("rescheduled claim: %+v %v", job, err)
				}
			})
		}
	}
}

func TestMissingNodeCompletionHonorsClaimFences(t *testing.T) {
	for _, scenario := range []string{"expired-lease", "active-lease", "future", "wrong-lane", "wrong-kind", "stale-generation"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			if err := f.repo.Schedule(ctx, KindFast, "orphan", Payload{Identity: "missing-node"}, 0, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			var ref string
			if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work`).Scan(&ref); err != nil {
				t.Fatal(err)
			}
			kind, high := KindFast, false
			switch scenario {
			case "expired-lease":
				f.exec(t, `UPDATE node_explorer_work SET state='running',lease_token='old',lease_until=now()-interval '1 second',error='old error'`)
			case "active-lease":
				f.exec(t, `UPDATE node_explorer_work SET state='running',lease_token='active',lease_until=now()+interval '1 hour'`)
			case "future":
				f.exec(t, `UPDATE node_explorer_work SET due_at=now()+interval '1 hour'`)
			case "wrong-lane":
				high = true
			case "wrong-kind":
				kind = KindLive
			case "stale-generation":
				f.exec(t, `UPDATE node_explorer_work SET generation=generation+1`)
			}
			var before string
			if err := f.repo.Pool.QueryRow(ctx, `SELECT row_to_json(w)::text FROM node_explorer_work w`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if job, err := f.repo.Claim(ctx, kind, ref, high); err != nil || job != nil {
				t.Fatalf("claim: %+v %v", job, err)
			}
			if scenario == "expired-lease" {
				var done bool
				if err := f.repo.Pool.QueryRow(ctx, `SELECT state='done' AND lease_token IS NULL AND lease_until IS NULL AND error='' FROM node_explorer_work`).Scan(&done); err != nil || !done {
					t.Fatalf("expired orphan not completed: %v %v", done, err)
				}
			} else {
				var after string
				if err := f.repo.Pool.QueryRow(ctx, `SELECT row_to_json(w)::text FROM node_explorer_work w`).Scan(&after); err != nil || before != after {
					t.Fatalf("unclaimable task changed: before=%s after=%s err=%v", before, after, err)
				}
			}
		})
	}
}
