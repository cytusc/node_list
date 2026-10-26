package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

type admissionQueue struct {
	recordingQueue
	calls atomic.Int32
	pop   func(context.Context) (string, error)
}

func (q *admissionQueue) Pop(ctx context.Context, _ string, _ bool) (string, error) {
	q.calls.Add(1)
	if q.pop != nil {
		return q.pop(ctx)
	}
	return "", nil
}

func TestExitAdmissionWaitsBeforePopAndReleasesOnEmptyOrError(t *testing.T) {
	q := &admissionQueue{}
	r := Runner{Repo: &Repository{}, Queue: q, Config: Config{Role: "exit-provision", Manager: &transport.ManagerClient{}, ExitConcurrency: 1}}
	r.configureAdmission()
	ctx := context.Background()
	if err := r.exitSlots.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := r.nextJob(bounded, KindExit, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("admission ignored cancellation: %v", err)
	}
	if q.calls.Load() != 0 {
		t.Fatal("capacity waiter popped durable work")
	}
	r.exitSlots.Release(1)
	for _, high := range []bool{true, false} {
		for _, popErr := range []error{nil, errors.New("redis unavailable")} {
			q.pop = func(context.Context) (string, error) { return "", popErr }
			if j, _, err := r.nextJob(ctx, KindExit, high); j != nil || !errors.Is(err, popErr) {
				t.Fatalf("pop result: %v %v", j, err)
			}
			if !r.exitSlots.TryAcquire(1) {
				t.Fatal("empty or failed pop leaked capacity")
			}
			r.exitSlots.Release(1)
		}
	}
}

func TestExitAdmissionSharesCapacityAcrossPriorityLanes(t *testing.T) {
	entered := make(chan struct{})
	unblock := make(chan struct{})
	q := &admissionQueue{pop: func(ctx context.Context) (string, error) {
		close(entered)
		select {
		case <-unblock:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	r := Runner{Repo: &Repository{}, Queue: q, Config: Config{Role: "exit-provision", Manager: &transport.ManagerClient{}, ExitConcurrency: 1}}
	r.configureAdmission()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _, _ = r.nextJob(ctx, KindExit, true) }()
	<-entered
	bounded, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, _, err := r.nextJob(bounded, KindExit, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("priority lanes exceeded shared capacity: %v", err)
	}
	if q.calls.Load() != 1 {
		t.Fatal("second lane popped while full")
	}
	close(unblock)
	<-done
}

func TestExitAdmissionHoldsSlotUntilDurableWorkFinishes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.repo.Schedule(ctx, KindExit, "admission", Payload{Identity: f.id}, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := f.repo.Pool.QueryRow(ctx, "SELECT id::text||':'||generation::text FROM node_explorer_work WHERE key='admission'").Scan(&ref); err != nil {
		t.Fatal(err)
	}
	q := &admissionQueue{pop: func(context.Context) (string, error) { return ref, nil }}
	r := Runner{Repo: f.repo, Queue: q, Config: Config{Role: "exit-provision", Manager: &transport.ManagerClient{}, ExitConcurrency: 1}}
	r.configureAdmission()
	j, release, err := r.nextJob(ctx, KindExit, false)
	if err != nil || j == nil {
		t.Fatalf("claim: %v", err)
	}
	if r.exitSlots.TryAcquire(1) {
		t.Fatal("durable claim released slot before execution")
	}
	if err = f.repo.Finish(ctx, *j, nil); err != nil {
		t.Fatal(err)
	}
	release()
	if !r.exitSlots.TryAcquire(1) {
		t.Fatal("finished claim leaked slot")
	}
	r.exitSlots.Release(1)
	j, _, err = r.nextJob(ctx, KindExit, false)
	if err != nil || j != nil {
		t.Fatalf("stale ref: %v %v", j, err)
	}
	if !r.exitSlots.TryAcquire(1) {
		t.Fatal("stale ref leaked slot")
	}
	r.exitSlots.Release(1)
}
