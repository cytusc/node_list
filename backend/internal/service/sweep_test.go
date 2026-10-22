package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

type testSweepKernel struct {
	probe      func(context.Context, transport.LeaseRequest) (transport.DelayResult, error)
	generation func(context.Context, string) error
}

func (m testSweepKernel) Delay(ctx context.Context, req transport.LeaseRequest) (transport.DelayResult, error) {
	if m.probe != nil {
		return m.probe(ctx, req)
	}
	return transport.DelayResult{Generation: "test-generation", CheckedAt: time.Now()}, nil
}
func (m testSweepKernel) Generation(ctx context.Context, g string) error {
	if m.generation != nil {
		return m.generation(ctx, g)
	}
	if g != "test-generation" {
		return errors.New("stale generation")
	}
	return ctx.Err()
}
func awaitSweep(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sweep did not exit")
	}
}
func TestSweepRunnerShutdownCancelsAndJoinsWithoutOverlap(t *testing.T) {
	s := newSweepRunner(time.Hour)
	s.next = time.Time{}
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	run := func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		close(exited)
		return ctx.Err()
	}
	if !s.start(context.Background(), run) {
		t.Fatal("not started")
	}
	awaitSweep(t, started)
	// The config loop is free to observe a change while the probe blocks.
	if s.start(context.Background(), run) {
		t.Fatal("overlapping round")
	}
	stopped := make(chan struct{})
	go func() { s.stop(); close(stopped) }()
	awaitSweep(t, cancelled)
	select {
	case <-stopped:
		t.Fatal("shutdown returned before round joined")
	default:
	}
	close(release)
	awaitSweep(t, stopped)
	awaitSweep(t, exited)
	if s.status().Running || s.status().Outcome != "cancelled" {
		t.Fatalf("%+v", s.status())
	}
	if !s.start(context.Background(), func(context.Context) error { return nil }) {
		t.Fatal("cancelled round prevented restart")
	}
	awaitSweep(t, s.done)
	s.stop()
	if s.start(context.Background(), func(context.Context) error { return nil }) {
		t.Fatal("completion interval ignored")
	}
}

func TestSweepRunnerDefersReloadThroughProbeAndCommit(t *testing.T) {
	s := newSweepRunner(30 * time.Minute)
	s.next = time.Time{}
	probed := make(chan struct{})
	commit := make(chan struct{})
	committing := make(chan struct{})
	finish := make(chan struct{})
	var persisted atomic.Bool
	if !s.start(context.Background(), func(ctx context.Context) error {
		close(probed)
		<-commit
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.progress(func(status *sweepStatus) { status.Phase = "committing" })
		close(committing)
		<-finish
		persisted.Store(true)
		return ctx.Err()
	}) {
		t.Fatal("not started")
	}
	// Ensure a failed assertion cannot leave the worker blocked.
	t.Cleanup(func() { s.stop() })
	t.Cleanup(func() { close(finish) })
	t.Cleanup(func() { close(commit) })
	awaitSweep(t, probed)
	reload := func() error {
		if !persisted.Load() {
			t.Error("reload ran before results persisted")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if attempted, err := s.withReload(reload); attempted || err != nil {
			t.Fatalf("reload interrupted probing: attempted=%v err=%v", attempted, err)
		}
	}
	commit <- struct{}{}
	awaitSweep(t, committing)
	if attempted, err := s.withReload(reload); attempted || err != nil {
		t.Fatalf("reload interrupted commit: attempted=%v err=%v", attempted, err)
	}
	finish <- struct{}{}
	awaitSweep(t, s.done)
	if attempted, err := s.withReload(reload); !attempted || err != nil {
		t.Fatalf("completed round blocked reload: attempted=%v err=%v", attempted, err)
	}
	status := s.status()
	if status.Outcome != "completed" || status.NextRunAt.Sub(status.FinishedAt) != 30*time.Minute {
		t.Fatalf("round did not retain 30-minute idle interval: %+v", status)
	}
	if s.start(context.Background(), func(context.Context) error { return nil }) {
		t.Fatal("reload bypassed idle interval")
	}
}

func TestSweepRunnerReloadFailureReleasesLock(t *testing.T) {
	s := newSweepRunner(time.Hour)
	s.next = time.Time{}
	failure := errors.New("reload failed")
	if attempted, err := s.withReload(func() error { return failure }); !attempted || !errors.Is(err, failure) {
		t.Fatalf("attempted=%v err=%v", attempted, err)
	}
	if !s.start(context.Background(), func(context.Context) error { return nil }) {
		t.Fatal("failed reload retained process lock")
	}
	awaitSweep(t, s.done)
	s.stop()
}

func TestSweepReloadObservesCommittedResults(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	entries, err := f.repo.sweepEntries(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	s := newSweepRunner(30 * time.Minute)
	s.next = time.Time{}
	committing := make(chan struct{})
	finish := make(chan struct{})
	m := testSweepKernel{generation: func(ctx context.Context, _ string) error {
		close(committing)
		select {
		case <-finish:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	if !s.start(ctx, func(round context.Context) error {
		return f.repo.sweepCommit(round, m, []sweepObservation{{entry: entries[0], delay: transport.DelayResult{
			Generation: "test-generation", CheckedAt: time.Now(),
		}}})
	}) {
		t.Fatal("not started")
	}
	t.Cleanup(func() { s.stop() })
	t.Cleanup(func() { close(finish) })
	awaitSweep(t, committing)
	reload := func() error {
		var count int
		err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_connectivity WHERE identity=$1`, f.id).Scan(&count)
		if err == nil && count != 1 {
			return fmt.Errorf("reload observed %d committed results, want 1", count)
		}
		return err
	}
	if attempted, err := s.withReload(reload); attempted || err != nil {
		t.Fatalf("reload ran during transaction: attempted=%v err=%v", attempted, err)
	}
	finish <- struct{}{}
	awaitSweep(t, s.done)
	if s.status().Outcome != "completed" {
		t.Fatalf("commit failed: %+v", s.status())
	}
	if attempted, err := s.withReload(reload); !attempted || err != nil {
		t.Fatalf("reload did not observe committed evidence: attempted=%v err=%v", attempted, err)
	}
}
func TestSweepRunnerShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newSweepRunner(time.Hour)
	s.next = time.Time{}
	s.start(ctx, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	cancel()
	awaitSweep(t, s.done)
	s.stop()
	if s.start(ctx, func(context.Context) error { return nil }) {
		t.Fatal("started after shutdown")
	}
}
func TestSweepWorkersCancelAndSkipInfrastructureErrors(t *testing.T) {
	entries := make([]transport.RegistryEntry, 1000)
	for i := range entries {
		entries[i].Identity = fmt.Sprint(i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var running, peak atomic.Int32
	blocked := make(chan struct{}, 4)
	m := testSweepKernel{probe: func(ctx context.Context, _ transport.LeaseRequest) (transport.DelayResult, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		blocked <- struct{}{}
		<-ctx.Done()
		return transport.DelayResult{}, ctx.Err()
	}}
	done := make(chan struct{})
	go func() {
		results := (&Repository{}).sweepAll(ctx, m, entries, 4)
		for _, res := range results {
			if res.entry.Identity != "" {
				t.Error("cancelled result retained")
			}
		}
		close(done)
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-blocked:
		case <-time.After(time.Second):
			t.Fatal("workers not running")
		}
	}
	cancel()
	awaitSweep(t, done)
	if peak.Load() != 4 || running.Load() != 0 {
		t.Fatalf("peak=%d running=%d", peak.Load(), running.Load())
	}
	m.probe = func(_ context.Context, req transport.LeaseRequest) (transport.DelayResult, error) {
		switch req.Targets[0].Identity {
		case "0":
			return transport.DelayResult{}, transport.ErrManagerBusy
		case "1":
			return transport.DelayResult{}, errors.New("controller unavailable")
		default:
			return transport.DelayResult{Generation: "test-generation", CheckedAt: time.Now(), Success: false}, nil
		}
	}
	results := (&Repository{}).sweepAll(context.Background(), m, entries[:3], 2)
	if results[0].entry.Identity != "" || results[1].entry.Identity != "" || results[2].entry.Identity != "2" {
		t.Fatal("infra error confused with explicit node failure")
	}
}

func TestSweepCommitRejectsStaleEvidenceAndRollsBack(t *testing.T) {
	for _, scenario := range []string{"incarnation", "digest", "generation", "cancelled", "older", "rollback", "cancel-during-commit"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			entries, err := f.repo.sweepEntries(ctx)
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries: %v %v", entries, err)
			}
			observation := sweepObservation{entry: entries[0], delay: transport.DelayResult{Generation: "test-generation", CheckedAt: time.Now()}}
			m := testSweepKernel{}
			expectedError := false
			switch scenario {
			case "incarnation":
				f.exec(t, `UPDATE node_explorer_node_registry SET incarnation='replacement'`)
			case "digest":
				observation.entry.Digest = "old-config"
			case "generation":
				observation.delay.Generation = "old-generation"
				expectedError = true
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
				expectedError = true
			case "older":
				f.evidence(t, "newer")
				observation.delay.CheckedAt = time.Now().Add(-time.Hour)
			case "cancel-during-commit":
				cancelled, cancel := context.WithCancel(ctx)
				defer cancel()
				ctx = cancelled
				calls := 0
				m.generation = func(ctx context.Context, _ string) error {
					calls++
					if calls == 2 {
						cancel()
					}
					return ctx.Err()
				}
				expectedError = true
			case "rollback":
				// First observation writes failure evidence. A subsequent generation
				// rejection must roll back those writes and any failure accumulation.
				expectedError = true
			}
			results := []sweepObservation{observation}
			if scenario == "rollback" || scenario == "cancel-during-commit" {
				stale := observation
				stale.delay.Generation = "old-generation"
				results = append(results, stale)
			}
			err = f.repo.sweepCommit(ctx, m, results)
			if (err != nil) != expectedError {
				t.Fatalf("error=%v expected=%v", err, expectedError)
			}
			var count int
			if err = f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_fast_results`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale fast evidence: %d %v", count, err)
			}
			if err = f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_connectivity WHERE coalesce((payload->>'failedRounds')::int,0)>0`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failure contamination: %d %v", count, err)
			}
			if err = f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_retired_nodes`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("retired: %d %v", count, err)
			}
		})
	}
}

func TestSweepExplicitFailuresStillRetireAfterThreeRounds(t *testing.T) {
	f := setup(t)
	t.Setenv("LIFECYCLE_DRY_RUN", "false")
	ctx := context.Background()
	entries, err := f.repo.sweepEntries(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries: %v %v", entries, err)
	}
	for i := 0; i < 3; i++ {
		observation := sweepObservation{entry: entries[0], delay: transport.DelayResult{Generation: "test-generation", CheckedAt: time.Now().Add(time.Duration(-9+3*i) * time.Minute)}}
		if err = f.repo.sweepCommit(ctx, testSweepKernel{}, []sweepObservation{observation}); err != nil {
			t.Fatal(err)
		}
		var count int
		if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_retired_nodes`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if i == 2 {
			want = 1
		}
		if count != want {
			t.Fatalf("round %d retired=%d want=%d", i+1, count, want)
		}
	}
}
