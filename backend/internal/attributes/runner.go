package attributes

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Store interface {
	Due(context.Context, int) ([]Candidate, error)
	Current(context.Context, Candidate) (bool, error)
	Save(context.Context, Candidate, Result, time.Duration) error
	Heartbeat(context.Context, bool) error
}
type Runner struct {
	Store          Store
	Lookup         Lookup
	Workers        int
	Timeout, Retry time.Duration
}

func (r Runner) Run(parent context.Context) error {
	if r.Workers < 1 || r.Timeout <= 0 || r.Retry <= 0 || r.Store == nil || r.Lookup == nil {
		return errors.New("invalid attributes runner configuration")
	}
	ctx, cancel := context.WithCancel(parent)
	type completed struct {
		n    Candidate
		r    Result
		skip bool
		err  error
	}
	results := make(chan completed, r.Workers)
	active := map[string]bool{}
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = r.Store.Heartbeat(stopCtx, false)
	}()
	heartbeat := func() error {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return r.Store.Heartbeat(check, true)
	}
	fill := func() error {
		if len(active) >= r.Workers {
			return nil
		}
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		nodes, err := r.Store.Due(check, r.Workers*2)
		stop()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if active[n.IP] {
				continue
			}
			if len(active) >= r.Workers {
				break
			}
			active[n.IP] = true
			wg.Add(1)
			go func(n Candidate) {
				defer wg.Done()
				probeCtx, stop := context.WithTimeout(ctx, r.Timeout)
				defer stop()
				current, err := r.Store.Current(probeCtx, n)
				c := completed{n: n, skip: !current, err: err}
				if err == nil && current {
					c.r = r.Lookup.Lookup(probeCtx, n)
				}
				select {
				case results <- c:
				case <-ctx.Done():
				}
			}(n)
		}
		return nil
	}
	if err := heartbeat(); err != nil {
		return err
	}
	if err := fill(); err != nil {
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastHeartbeat := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case c := <-results:
			delete(active, c.n.IP)
			if ctx.Err() != nil {
				return nil
			}
			if c.err != nil {
				return c.err
			}
			// Retry stale inputs on the next poll, not a tight loop.
			if c.skip {
				continue
			}
			delay := r.Retry
			check, stop := context.WithTimeout(ctx, 5*time.Second)
			err := r.Store.Save(check, c.n, c.r, delay)
			stop()
			if err != nil {
				return err
			}
			if err := fill(); err != nil {
				return err
			}
		case <-tick.C:
			if time.Since(lastHeartbeat) >= 5*time.Second {
				if err := heartbeat(); err != nil {
					return err
				}
				lastHeartbeat = time.Now()
			}
			if err := fill(); err != nil {
				return err
			}
		}
	}
}
