package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

func TestSweepRecoveryCancelsAndJoinsBeforeReload(t *testing.T) {
	for _, phase := range []string{"probing", "committing"} {
		t.Run(phase, func(t *testing.T) {
			s := newSweepRunner(time.Hour)
			s.next = time.Time{}
			started, cancelled := make(chan struct{}), make(chan struct{})
			rollback := make(chan struct{})
			if !s.start(context.Background(), func(ctx context.Context) error {
				s.progress(func(state *sweepStatus) { state.Phase = phase })
				close(started)
				<-ctx.Done()
				close(cancelled)
				<-rollback // stand in for transaction rollback / probe cleanup
				return ctx.Err()
			}) {
				t.Fatal("round did not start")
			}
			awaitSweep(t, started)
			reloaded, recovered := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(recovered)
				attempted, err := s.withRecovery(func() error {
					close(reloaded)
					return nil
				})
				if !attempted || err != nil {
					t.Errorf("recovery not attempted: %v %v", attempted, err)
				}
			}()
			awaitSweep(t, cancelled)
			select {
			case <-reloaded:
				t.Error("reload overlapped probe cleanup or rollback")
			default:
			}
			close(rollback)
			awaitSweep(t, recovered)
			awaitSweep(t, reloaded)
			if status := s.status(); status.Running || status.Outcome != "cancelled" {
				t.Fatalf("faulted round reported success: %+v", status)
			}
			if !s.start(context.Background(), func(context.Context) error { return nil }) {
				t.Fatal("recovery imposed a full interval before replacement sweep")
			}
			awaitSweep(t, s.done)
			s.stop()
		})
	}
}

func TestSweepRecoveryRollsBackPartialEvidenceBeforeReload(t *testing.T) {
	f := setup(t)
	entries, err := f.repo.sweepEntries(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	var calls atomic.Int32
	committing := make(chan struct{})
	m := testSweepKernel{generation: func(ctx context.Context, _ string) error {
		if calls.Add(1) == 2 {
			close(committing)
			<-ctx.Done()
		}
		return ctx.Err()
	}}
	s := newSweepRunner(time.Hour)
	s.next = time.Time{}
	if !s.start(context.Background(), func(ctx context.Context) error {
		// The first observation writes evidence; cancellation during the second
		// must roll back the whole transaction before recovery sees the database.
		results := make([]sweepObservation, 2)
		for i := range results {
			results[i] = sweepObservation{entry: entries[0], delay: transport.DelayResult{
				Generation: "test-generation", CheckedAt: time.Now().Add(time.Duration(i) * time.Second),
			}}
		}
		return f.repo.sweepCommit(ctx, m, results)
	}) {
		t.Fatal("not started")
	}
	t.Cleanup(s.stop)
	awaitSweep(t, committing)
	attempted, err := s.withRecovery(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var count int
		if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_connectivity`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("cancelled round left %d observations", count)
		}
		// The rollback must release its row locks as well as hide the evidence.
		_, err := f.repo.Pool.Exec(ctx, `SELECT identity FROM node_explorer_node_registry WHERE identity=$1 FOR UPDATE NOWAIT`, f.id)
		return err
	})
	if !attempted || err != nil {
		t.Fatalf("recovery ran before rollback: %v %v", attempted, err)
	}
}

func TestSweepRecoveryRetainsIntervalAfterCompletedRound(t *testing.T) {
	s := newSweepRunner(time.Hour)
	s.next = time.Time{}
	if !s.start(context.Background(), func(context.Context) error { return nil }) {
		t.Fatal("not started")
	}
	awaitSweep(t, s.done)
	if attempted, err := s.withRecovery(func() error { return nil }); !attempted || err != nil {
		t.Fatalf("recovery blocked: %v %v", attempted, err)
	}
	if s.start(context.Background(), func(context.Context) error { return nil }) {
		t.Fatal("completed round lost its interval")
	}
}
