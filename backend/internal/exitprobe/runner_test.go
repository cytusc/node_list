package exitprobe

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	nodes   []Candidate
	current bool
	saves   int
	stopped bool
}

func (s *memoryStore) Due(context.Context, int) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Candidate(nil), s.nodes...), nil
}
func (s *memoryStore) Current(context.Context, Candidate) (bool, error) { return s.current, nil }
func (s *memoryStore) Save(_ context.Context, n Candidate, _ Result, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	return nil
}
func (s *memoryStore) Heartbeat(_ context.Context, running bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = !running
	return nil
}

type probeFunc func(context.Context, json.RawMessage) Result

func (f probeFunc) Probe(c context.Context, n json.RawMessage) Result { return f(c, n) }
func TestRunnerBoundsConcurrencyAndJoinsOnCancellation(t *testing.T) {
	st := &memoryStore{current: true, nodes: []Candidate{{Identity: "a"}, {Identity: "b"}, {Identity: "c"}}}
	var count, peak atomic.Int64
	started := make(chan struct{}, 4)
	p := probeFunc(func(ctx context.Context, _ json.RawMessage) Result {
		n := count.Add(1)
		defer count.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return Result{Error: "cancelled", CheckedAt: time.Now()}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := Runner{Store: st, Prober: p, Workers: 2, Timeout: time.Minute, Interval: time.Hour, Retry: time.Minute}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("no probe")
		}
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
	if peak.Load() != 2 || count.Load() != 0 {
		t.Fatalf("concurrency or join %d %d", peak.Load(), count.Load())
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.saves != 0 || !st.stopped {
		t.Fatalf("cancelled result saved/stopped missing %+v", st)
	}
}
func TestRunnerRechecksUnavailableBeforeProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &memoryStore{nodes: []Candidate{{Identity: "down"}}}
	var calls atomic.Int64
	p := probeFunc(func(context.Context, json.RawMessage) Result { calls.Add(1); return Result{} })
	r := Runner{Store: st, Prober: p, Workers: 1, Timeout: time.Second, Interval: time.Hour, Retry: time.Minute}
	// Stop after at least one dispatch, without an unbounded busy-loop fixture.
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("unavailable node probed")
	}
}

type savedAttempt struct {
	id    string
	delay time.Duration
}
type completingStore struct {
	memoryStore
	saved chan savedAttempt
}

func (s *completingStore) Save(_ context.Context, n Candidate, _ Result, delay time.Duration) error {
	s.mu.Lock()
	for i, c := range s.nodes {
		if c.Identity == n.Identity {
			s.nodes = append(s.nodes[:i], s.nodes[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	s.saved <- savedAttempt{n.Identity, delay}
	return nil
}
func TestRunnerPersistsDistinctSuccessAndFailureIntervals(t *testing.T) {
	st := &completingStore{memoryStore: memoryStore{current: true, nodes: []Candidate{{Identity: "success", Node: json.RawMessage(`"success"`)}, {Identity: "failure", Node: json.RawMessage(`"failure"`)}}}, saved: make(chan savedAttempt, 2)}
	p := probeFunc(func(_ context.Context, n json.RawMessage) Result {
		r := Result{IP: "8.8.8.8", CheckedAt: time.Now()}
		if string(n) == `"failure"` {
			r.IP = ""
			r.Error = "echo_failed"
		}
		return r
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := Runner{Store: st, Prober: p, Workers: 2, Timeout: time.Second, Interval: time.Hour, Retry: time.Minute}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case saved := <-st.saved:
			want := time.Hour
			if saved.id == "failure" {
				want = time.Minute
			}
			if saved.delay != want {
				t.Fatalf("%s delay %s", saved.id, saved.delay)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("save did not finish")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked")
	}
}
