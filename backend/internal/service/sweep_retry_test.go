package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

func runRetryRound(t *testing.T, s *sweepRunner, live []transport.RegistryEntry, probe func(context.Context, transport.LeaseRequest) (transport.DelayResult, error), commitErr error) []transport.RegistryEntry {
	t.Helper()
	s.stop()
	s.next = time.Time{} // Advance only the scheduled batch, not the full-sweep deadline.
	var selected []transport.RegistryEntry
	if !s.start(context.Background(), func(ctx context.Context) error {
		selected = s.selectEntries(live)
		s.progress(func(st *sweepStatus) { st.Candidates = len(selected) })
		results := (&Repository{}).sweepAll(ctx, testSweepKernel{probe: probe}, selected, 1)
		pending := s.observations(selected, results)
		if commitErr != nil {
			return commitErr
		}
		s.commitPending(pending)
		return nil
	}) {
		t.Fatal("round did not start")
	}
	awaitSweep(t, s.done)
	return selected
}

func TestSweepRetriesOnlyMissingEvidenceWithFreshRegistry(t *testing.T) {
	s := newSweepRunner(30 * time.Minute)
	defer s.stop()
	live := []transport.RegistryEntry{{Identity: "passed"}, {Identity: "dead"}, {Identity: "busy"}, {Identity: "timeout"}, {Identity: "changed"}, {Identity: "rejected"}, {Identity: "departed"}}
	calls := map[string]int{}
	probe := func(_ context.Context, req transport.LeaseRequest) (transport.DelayResult, error) {
		id := req.Targets[0].Identity
		calls[id]++
		switch id {
		case "busy", "departed":
			return transport.DelayResult{}, transport.ErrManagerBusy
		case "timeout":
			return transport.DelayResult{}, context.DeadlineExceeded
		case "changed":
			return transport.DelayResult{}, transport.ErrConfigChanged
		case "rejected":
			return transport.DelayResult{}, transport.ErrConfigRejected
		}
		return transport.DelayResult{Generation: "g", CheckedAt: time.Now(), Success: id == "passed"}, nil
	}
	runRetryRound(t, s, live, probe, nil)
	status := s.status()
	if status.Kind != "full" || status.Outcome != "partial" || status.Observations != 2 || status.Skipped != 5 || status.Rejected != 1 || status.Pending != 4 || status.NextRunAt.Sub(status.FinishedAt) != 30*time.Second {
		t.Fatalf("bad partial coverage: %+v", status)
	}
	if status.SkipReasons["busy"] != 2 || status.SkipReasons["timeout"] != 1 || status.SkipReasons["config_changed"] != 1 {
		t.Fatalf("missing reasons: %+v", status)
	}
	fullNext := s.fullNext
	// Remove a departed node and replace a configuration before the retry.
	live = live[:6]
	live[4].Incarnation = "replacement"
	probe = func(_ context.Context, req transport.LeaseRequest) (transport.DelayResult, error) {
		e := req.Targets[0]
		calls[e.Identity]++
		if e.Identity == "changed" && e.Incarnation != "replacement" {
			t.Error("retried stale incarnation")
		}
		return transport.DelayResult{Generation: "new", CheckedAt: time.Now(), Success: true}, nil
	}
	selected := runRetryRound(t, s, live, probe, nil)
	status = s.status()
	if len(selected) != 3 || status.Kind != "retry" || status.Outcome != "completed" || status.Pending != 0 || !status.NextRunAt.Equal(fullNext) {
		t.Fatalf("retry coverage/schedule: selected=%v status=%+v", selected, status)
	}
	for _, id := range []string{"passed", "dead", "rejected", "departed"} {
		if calls[id] != 1 {
			t.Errorf("unnecessary repeat for %s: %d", id, calls[id])
		}
	}
}

func TestSweepRetryBackoffAndFullSweepDeadline(t *testing.T) {
	s := newSweepRunner(30 * time.Minute)
	defer s.stop()
	live := []transport.RegistryEntry{{Identity: "unavailable"}}
	failed := func(context.Context, transport.LeaseRequest) (transport.DelayResult, error) {
		return transport.DelayResult{}, errors.New("unavailable")
	}
	for _, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		runRetryRound(t, s, live, failed, nil)
		st := s.status()
		if st.Outcome != "partial" || st.Pending != 1 || st.NextRunAt.Sub(st.FinishedAt) != delay {
			t.Fatalf("backoff want %s: %+v", delay, st)
		}
	}
	// Ongoing retries must not starve nodes added since the last full sweep.
	s.stop()
	s.fullNext = time.Now().Add(-time.Second)
	live = append(live, transport.RegistryEntry{Identity: "new"})
	if selected := runRetryRound(t, s, live, failed, nil); len(selected) != 2 || s.status().Kind != "full" {
		t.Fatalf("full sweep starved: %+v", s.status())
	}
}

func TestSweepFailedCommitDoesNotAdvanceCoverage(t *testing.T) {
	s := newSweepRunner(30 * time.Minute)
	defer s.stop()
	live := []transport.RegistryEntry{{Identity: "a"}}
	failed := func(context.Context, transport.LeaseRequest) (transport.DelayResult, error) {
		return transport.DelayResult{}, context.DeadlineExceeded
	}
	runRetryRound(t, s, live, failed, nil)
	good := func(context.Context, transport.LeaseRequest) (transport.DelayResult, error) {
		return transport.DelayResult{Generation: "g", CheckedAt: time.Now()}, nil
	}
	runRetryRound(t, s, live, good, errors.New("commit failed"))
	if st := s.status(); st.Outcome != "failed" || st.Pending != 1 || st.NextRunAt.Sub(st.FinishedAt) > sweepRetryMax {
		t.Fatalf("uncommitted evidence counted: %+v", st)
	}
	if selected := runRetryRound(t, s, live, good, nil); len(selected) != 1 || s.status().Pending != 0 {
		t.Fatal("failed commit lost retry")
	}
}

func TestSweepRejectedConfigDoesNotHotLoop(t *testing.T) {
	s := newSweepRunner(30 * time.Minute)
	defer s.stop()
	runRetryRound(t, s, []transport.RegistryEntry{{Identity: "rejected"}}, func(context.Context, transport.LeaseRequest) (transport.DelayResult, error) {
		return transport.DelayResult{}, transport.ErrConfigRejected
	}, nil)
	if st := s.status(); st.Outcome != "partial" || st.Rejected != 1 || st.Pending != 0 || st.NextRunAt.Sub(st.FinishedAt) != 30*time.Minute {
		t.Fatalf("rejected config hot loop: %+v", st)
	}
}

func TestSweepFullDeadlineReachedDuringRetry(t *testing.T) {
	s := newSweepRunner(30 * time.Minute)
	defer s.stop()
	s.pending = map[string]struct{}{"a": {}}
	s.fullNext = time.Now().Add(time.Hour)
	due := time.Now().Add(-time.Second)
	runRetryRound(t, s, []transport.RegistryEntry{{Identity: "a"}}, func(context.Context, transport.LeaseRequest) (transport.DelayResult, error) {
		// Simulate the full deadline passing while the retry batch is in flight.
		s.mu.Lock()
		s.fullNext = due
		s.mu.Unlock()
		return transport.DelayResult{}, transport.ErrManagerBusy
	}, nil)
	if st := s.status(); st.Kind != "retry" || !st.NextRunAt.Equal(due) {
		t.Fatalf("overdue full sweep delayed by retry backoff: %+v", st)
	}
}
