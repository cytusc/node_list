package service

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

// Calls to start/stop/withReload belong to the config loop. The process mutex
// protects the entire round, including its database commit, against reloads.
// A busy reload is deferred so the config loop can keep observing changes.
type sweepRunner struct {
	every  time.Duration
	next   time.Time
	cancel context.CancelFunc
	done   chan struct{}
	kernel sync.Mutex
	mu     sync.Mutex
	state  sweepStatus
	// Protected by mu. Retry batches never postpone the next full sweep.
	fullNext   time.Time
	pending    map[string]struct{}
	retryDelay time.Duration
}

const sweepRetryMin = 30 * time.Second
const sweepRetryMax = 5 * time.Minute

type sweepStatus struct {
	Kind           string         `json:"kind,omitempty"`
	Phase          string         `json:"phase,omitempty"`
	Candidates     int            `json:"candidates"`
	Processed      int            `json:"processed"`
	Observations   int            `json:"observations"`
	Skipped        int            `json:"skipped"`
	Rejected       int            `json:"rejected"`
	Pending        int            `json:"pending"`
	SkipReasons    map[string]int `json:"skip_reasons,omitempty"`
	ProbeMillis    int64          `json:"probe_ms"`
	CommitMillis   int64          `json:"commit_ms"`
	Running        bool           `json:"running"`
	StartedAt      time.Time      `json:"started_at,omitempty"`
	FinishedAt     time.Time      `json:"finished_at,omitempty"`
	DurationMillis int64          `json:"duration_ms"`
	Outcome        string         `json:"outcome,omitempty"`
	NextRunAt      time.Time      `json:"next_run_at,omitempty"`
}

// sweepFirstRoundDelay bounds how long the first round waits after start. A
// fresh process has no availability view at all, so waiting a whole interval
// leaves every consumer reading stale reachability for that long (a restart
// during the day did exactly that for 30 minutes).
const sweepFirstRoundDelay = 30 * time.Second

func newSweepRunner(every time.Duration) *sweepRunner {
	next := time.Now().Add(min(every, sweepFirstRoundDelay))
	return &sweepRunner{every: every, next: next, state: sweepStatus{NextRunAt: next}}
}
func (s *sweepRunner) status() sweepStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
func (s *sweepRunner) start(parent context.Context, run func(context.Context) error) bool {
	if s.done != nil {
		select {
		case <-s.done:
			s.cancel()
			s.done = nil
			state := s.status()
			s.next = state.NextRunAt
		default:
			return false
		}
	}
	if parent.Err() != nil || time.Now().Before(s.next) {
		return false
	}
	if !s.kernel.TryLock() {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Hour)
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	started := time.Now()
	s.mu.Lock()
	s.state = sweepStatus{Running: true, StartedAt: started, Phase: "loading", Pending: len(s.pending)}
	s.mu.Unlock()
	go func() {
		defer close(done)
		defer s.kernel.Unlock()
		defer cancel()
		err := run(ctx)
		outcome := "completed"
		if ctx.Err() != nil {
			outcome = "cancelled"
		} else if err != nil {
			outcome = "failed"
		}
		finished := time.Now()
		s.mu.Lock()
		if outcome == "completed" {
			if s.state.Kind != "retry" {
				s.fullNext = finished.Add(s.every)
			}
			if s.state.Skipped > 0 {
				outcome = "partial"
			}
		}
		next := s.fullNext
		if err != nil || ctx.Err() != nil || len(s.pending) > 0 {
			if s.retryDelay == 0 || (err == nil && s.state.Observations > 0) {
				s.retryDelay = sweepRetryMin
			} else {
				s.retryDelay = min(s.retryDelay*2, sweepRetryMax)
			}
			next = finished.Add(s.retryDelay)
			if !s.fullNext.IsZero() && s.fullNext.Before(next) && (s.fullNext.After(finished) || s.state.Kind == "retry") {
				next = s.fullNext
			}
		} else {
			s.retryDelay = 0
		}
		s.state.Running = false
		s.state.Phase = "finished"
		s.state.FinishedAt = finished
		s.state.DurationMillis = finished.Sub(started).Milliseconds()
		s.state.Outcome = outcome
		s.state.NextRunAt = next
		s.state.Pending = len(s.pending)
		state := s.state
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			log.Printf("mihomo-manager: sweep round failed: %s", taskFailureReason(err))
		}
		log.Printf("mihomo-manager: sweep round %s kind=%s candidates=%d observations=%d skipped=%d rejected=%d pending=%d duration=%s", outcome, state.Kind, state.Candidates, state.Observations, state.Skipped, state.Rejected, state.Pending, finished.Sub(started).Round(time.Millisecond))
	}()
	return true
}

// withReload never cancels a round. Only a finished probe and commit release
// the kernel for configuration changes; shutdown still cancels via stop.
func (s *sweepRunner) withReload(reload func() error) (bool, error) {
	if !s.kernel.TryLock() {
		return false, nil
	}
	defer s.kernel.Unlock()
	return true, reload()
}

// Recovery may cancel an unusable generation. Join the round, including any
// transaction rollback, before touching the kernel; normal reloads still defer.
func (s *sweepRunner) withRecovery(reload func() error) (bool, error) {
	s.stop()
	return s.withReload(reload)
}

func (s *sweepRunner) stop() {
	if s.done == nil {
		return
	}
	select {
	case <-s.done:
		// A completed round retains its completion-based interval.
		s.next = s.status().NextRunAt
	default:
		s.cancel()
		<-s.done
		// A cancelled round may restart after the new configuration is stable.
		s.next = time.Now()
	}
	s.cancel()
	s.done = nil
}

func (s *sweepRunner) progress(update func(*sweepStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update(&s.state)
}

// Always select from the current registry: departed nodes disappear, while a
// changed incarnation/configuration is retried using its fresh kernel entry.
func (s *sweepRunner) selectEntries(live []transport.RegistryEntry) []transport.RegistryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Kind = "full"
	if len(s.pending) == 0 || !time.Now().Before(s.fullNext) {
		return live
	}
	s.state.Kind = "retry"
	entries := make([]transport.RegistryEntry, 0, len(s.pending))
	for _, entry := range live {
		if _, ok := s.pending[entry.Identity]; ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// Only commitPending may advance coverage. A failed/cancelled transaction
// leaves the previous retry set intact, so uncommitted evidence is reprobed.
func (s *sweepRunner) observations(entries []transport.RegistryEntry, results []sweepObservation) map[string]struct{} {
	pending := map[string]struct{}{}
	reasons := map[string]int{}
	observed, rejected := 0, 0
	for i, entry := range entries {
		if results[i].entry.Identity != "" {
			observed++
			continue
		}
		reason := results[i].skip
		if reason == "" {
			reason = "cancelled"
		}
		reasons[reason]++
		if reason == "config_rejected" {
			rejected++ // Permanent configuration errors wait for a config change/full sweep.
		} else {
			pending[entry.Identity] = struct{}{}
		}
	}
	s.progress(func(state *sweepStatus) {
		state.Observations = observed
		state.Skipped = len(entries) - observed
		state.Rejected = rejected
		state.SkipReasons = reasons
	})
	return pending
}

func (s *sweepRunner) commitPending(pending map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = pending
}
