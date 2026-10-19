package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/taskqueue"
)

type publicationQueue struct {
	recordingQueue
	waves     [][]taskqueue.Delivery
	err       error
	afterPush func()
}

func (q *publicationQueue) PushBatch(_ context.Context, deliveries []taskqueue.Delivery) error {
	q.waves = append(q.waves, append([]taskqueue.Delivery(nil), deliveries...))
	if q.afterPush != nil {
		q.afterPush()
	}
	return q.err
}

func TestPublicationSkipsUnchangedWorkAndImmediatelySendsChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	q := &publicationQueue{}
	if err := f.repo.Schedule(ctx, KindLive, "publish", Payload{Identity: f.id}, 0, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	publish := func(waves int) {
		t.Helper()
		if err := f.repo.Publish(ctx, q, []string{KindLive}); err != nil {
			t.Fatal(err)
		}
		if len(q.waves) != waves || len(q.refs) != 0 {
			t.Fatalf("publication waves=%d, want %d; individual pushes=%v", len(q.waves), waves, q.refs)
		}
	}
	publish(1)
	publish(1)
	f.exec(t, `UPDATE node_explorer_work SET published_at=now()-interval '31 seconds'`)
	publish(2)
	ref := q.waves[0][0].Ref
	j, err := f.repo.Claim(ctx, KindLive, ref, false)
	if err != nil || j == nil {
		t.Fatalf("claim: %+v %v", j, err)
	}
	if err = f.repo.Retry(ctx, *j, time.Second, "retry"); err != nil {
		t.Fatal(err)
	}
	publish(3)
	if q.waves[2][0].Ref != ref || q.waves[2][0].Due.Equal(q.waves[1][0].Due) {
		t.Fatal("same-generation retry did not publish its new due time")
	}
	f.exec(t, `UPDATE node_explorer_work SET priority=1`)
	publish(4)
	if !q.waves[3][0].High {
		t.Fatal("promotion was not delivered to the reserved lane")
	}
	if err = f.repo.Schedule(ctx, KindLive, "publish", Payload{Identity: f.id}, 1, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	publish(5)
	if q.waves[4][0].Ref == ref {
		t.Fatal("reschedule reused the previous generation")
	}
	publish(5)
	if stale, err := f.repo.Claim(ctx, KindLive, ref, true); err != nil || stale != nil {
		t.Fatalf("stale delivery claimed rescheduled work: %+v %v", stale, err)
	}
}

func TestFailedOrRacingPublicationDoesNotMarkNewWorkDelivered(t *testing.T) {
	for _, scenario := range []string{"transport-failure", "concurrent-reschedule"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			if err := f.repo.Schedule(ctx, KindLive, "publish", Payload{Identity: f.id}, 0, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			q := &publicationQueue{}
			if scenario == "transport-failure" {
				q.err = errors.New("Redis unavailable")
			} else {
				q.afterPush = func() {
					f.exec(t, `UPDATE node_explorer_work SET generation=generation+1,due_at=now()`)
				}
			}
			if err := f.repo.Publish(ctx, q, []string{KindLive}); !errors.Is(err, q.err) {
				t.Fatalf("publish error: %v, want %v", err, q.err)
			}
			var marked bool
			if err := f.repo.Pool.QueryRow(ctx, `SELECT published_at IS NOT NULL FROM node_explorer_work`).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if marked {
				t.Fatal("failed or superseded publication was marked delivered")
			}
			q.err, q.afterPush = nil, nil
			if err := f.repo.Publish(ctx, q, []string{KindLive}); err != nil {
				t.Fatal(err)
			}
			if len(q.waves) != 2 {
				t.Fatal("work was not immediately republished")
			}
		})
	}
}

func TestNormalPublicationReservesRunningSlotsAndPrioritizesWaiting(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.repo.NormalRecoveryConcurrency = 2
	for i := 0; i < 6; i++ {
		if err := f.repo.Schedule(ctx, KindNormalRecovery, fmt.Sprint(i), Payload{Identity: f.id}, 0, time.Now().Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	q := &publicationQueue{}
	if err := f.repo.Publish(ctx, q, []string{KindNormalRecovery}); err != nil {
		t.Fatal(err)
	}
	if len(q.waves) != 1 || len(q.waves[0]) != 2 {
		t.Fatalf("wrong initial wave: %+v", q.waves)
	}
	first, err := f.repo.Claim(ctx, KindNormalRecovery, q.waves[0][0].Ref, false)
	if err != nil || first == nil {
		t.Fatal("first claim", err)
	}
	second, err := f.repo.Claim(ctx, KindNormalRecovery, q.waves[0][1].Ref, false)
	if err != nil || second == nil {
		t.Fatal("second claim", err)
	}
	// Full running lanes must not pop and discard the next unstarted task.
	if err = f.repo.Publish(ctx, q, []string{KindNormalRecovery}); err != nil {
		t.Fatal(err)
	}
	if len(q.waves) != 1 {
		t.Fatal("published above full running admission")
	}
	if err = f.repo.Retry(ctx, *first, 0, "dependency"); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.Publish(ctx, q, []string{KindNormalRecovery}); err != nil {
		t.Fatal(err)
	}
	if len(q.waves) != 2 || len(q.waves[1]) != 1 || q.waves[1][0].Ref != first.Ref() {
		t.Fatal("waiting assessment lost its slot")
	}
	if err = f.repo.Finish(ctx, *second, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.Publish(ctx, q, []string{KindNormalRecovery}); err != nil {
		t.Fatal(err)
	}
	if len(q.waves) != 3 || len(q.waves[2]) != 1 {
		t.Fatal("new task not immediately published after completion")
	}
}
