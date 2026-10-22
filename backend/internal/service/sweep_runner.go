package service

import (
	"context"
	"log"
	"sync"
	"time"
)

// Calls to start/stop belong to the config loop. Only status is shared with
// the heartbeat. Closing done hands the entire round (including commit) back
// to the config loop before it can reload the kernel.
type sweepRunner struct {
	every  time.Duration
	next   time.Time
	cancel context.CancelFunc
	done   chan struct{}
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
}

func newSweepRunner(every time.Duration) *sweepRunner {
	return &sweepRunner{every: every, next: time.Now().Add(every)}
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
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			log.Printf("mihomo-manager: sweep round failed: %s", taskFailureReason(err))
		}
		log.Printf("mihomo-manager: sweep round %s duration=%s", outcome, finished.Sub(started).Round(time.Millisecond))
	}()
	return true
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
