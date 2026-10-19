package connectivitybench

import (
	"context"
	"sync"
)

type controlFlight struct {
	done chan struct{}
	ok   bool
	err  error
}
type controlFlights struct {
	mu              sync.Mutex
	active          map[string]*controlFlight
	requests, joins int64
}

func newControlFlights() *controlFlights { return &controlFlights{active: map[string]*controlFlight{}} }

// Coalesce ONLY overlapping control checks. Completed results are never cached.
// Every follower waits for a response completed after it requested validation.
// The leader runs synchronously while it owns its immutable wave's lease; no
// request can outlive that lease, even when a follower cancels its own wait.
func (g *controlFlights) do(ctx context.Context, key string, probe func() (bool, error)) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	g.mu.Lock()
	if f, ok := g.active[key]; ok {
		g.joins++
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-f.done:
			return f.ok, f.err
		}
	}
	f := &controlFlight{done: make(chan struct{})}
	g.active[key] = f
	g.requests++
	g.mu.Unlock()
	f.ok, f.err = probe()
	g.mu.Lock()
	delete(g.active, key)
	close(f.done)
	g.mu.Unlock()
	return f.ok, f.err
}
func (g *controlFlights) stats() (int64, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests, g.joins
}
