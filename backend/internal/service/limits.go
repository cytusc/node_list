package service

import (
	"context"
	"time"

	"github.com/cytusc/node_list/backend/internal/ratelimit"
)

// providerInterval is the minimum spacing between two calls to the same
// upstream. The free tiers are expressed as requests per minute, and pacing
// them one at a time keeps sibling providers running in parallel.
const providerInterval = 1500 * time.Millisecond

// providerWaitLimit is the longest wait a single assessment will sit through
// before it treats the provider as unavailable for this round.
const providerWaitLimit = 2 * time.Second

// BeforeProvider reserves the next slot for one upstream. The clock lives in
// Redis (GCRA via redis_rate) so every replica shares it; a single-replica
// deployment or a test without Redis falls back to an in-process token bucket.
//
// This replaces the row-locked node_explorer_provider_limits transaction that
// used to serialise every call: one Eval round trip instead of a transaction,
// and no lock contention between replicas.
func (r *Repository) BeforeProvider(ctx context.Context, provider string) error {
	wait, err := r.limiter().Allow(ctx, ratelimit.Key("provider", provider), providerInterval)
	if err != nil {
		return err
	}
	if wait > providerWaitLimit {
		return waiting{wait}
	}
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// AfterProvider publishes an upstream cooldown (Retry-After, an exhausted
// daily quota, or a 429) to every replica.
func (r *Repository) AfterProvider(ctx context.Context, provider string, next time.Time) error {
	wait := time.Until(next)
	if wait <= 0 {
		return nil
	}
	return r.limiter().Block(ctx, ratelimit.Key("provider", provider), wait)
}

// limiter returns the shared pacer. A nil field is a valid single-process
// deployment, and ratelimit treats a nil Limiter as an in-process clock.
func (r *Repository) limiter() *ratelimit.Limiter {
	return r.Limiter
}
