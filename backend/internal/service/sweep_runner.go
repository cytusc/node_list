package service

import (
	"context"
	"log"
	"sync"
	"time"
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
}
type sweepStatus struct {
	Phase          string    `json:"phase,omitempty"`
	Candidates     int       `json:"candidates"`
	Observations   int       `json:"observations"`
	ProbeMillis    int64     `json:"probe_ms"`
	CommitMillis   int64     `json:"commit_ms"`
	Running        bool      `json:"running"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	DurationMillis int64     `json:"duration_ms"`
	Outcome        string    `json:"outcome,omitempty"`
	NextRunAt      time.Time `json:"next_run_at,omitempty"`
}

func newSweepRunner(every time.Duration) *sweepRunner {
	next := time.Now().Add(every)
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
			s.next = state.FinishedAt.Add(s.every)
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
	s.state = sweepStatus{Running: true, StartedAt: started, Phase: "loading"}
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
		s.state.Running = false
		s.state.Phase = "finished"
		s.state.FinishedAt = finished
		s.state.DurationMillis = finished.Sub(started).Milliseconds()
		s.state.Outcome = outcome
		s.state.NextRunAt = finished.Add(s.every)
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			log.Printf("mihomo-manager: sweep round failed: %s", taskFailureReason(err))
		}
		log.Printf("mihomo-manager: sweep round %s duration=%s", outcome, finished.Sub(started).Round(time.Millisecond))
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

func (s *sweepRunner) stop() {
	if s.done == nil {
		return
	}
	select {
	case <-s.done:
		// A completed round retains its completion-based interval.
		s.next = s.status().FinishedAt.Add(s.every)
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
