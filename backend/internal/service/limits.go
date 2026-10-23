package service

import (
	"context"
	"time"

	"github.com/cytusc/node_list/backend/internal/ratelimit"
)

// batchProvider is IP-API's batch endpoint. It is counted separately upstream
// and allows far fewer requests per minute than the single-IP path, so it needs
// its own spacing and its own patience.
const batchProvider = "ip-api-batch"

// providerInterval is the minimum spacing between two calls to the same
// upstream. The free tiers are expressed as requests per minute, and pacing
// them one at a time keeps sibling providers running in parallel.
//
// The interval is per endpoint because the ceilings differ: one shared 1.5 s
// spacing asked the batch endpoint for roughly twice its permitted rate, and
// the resulting 429s then blocked the single-IP path too.
func providerInterval(provider string) time.Duration {
	if provider == batchProvider {
		return 4 * time.Second
	}
	return 1500 * time.Millisecond
}

// providerWaitLimit is the longest wait a single assessment will sit through
// before it treats the provider as unavailable for this round. A batch wave
// covers 100 addresses at once, so it is worth waiting longer than one
// single-IP lookup.
func providerWaitLimit(provider string) time.Duration {
	if provider == batchProvider {
		return 6 * time.Second
	}
	return 2 * time.Second
}

// BeforeProvider reserves the next slot for one upstream. The clock lives in
// Redis (GCRA via redis_rate) so every replica shares it; a single-replica
// deployment or a test without Redis falls back to an in-process token bucket.
//
// This replaces the row-locked node_explorer_provider_limits transaction that
// used to serialise every call: one Eval round trip instead of a transaction,
// and no lock contention between replicas.
func (r *Repository) BeforeProvider(ctx context.Context, provider string) error {
	wait, err := r.limiter().Allow(ctx, ratelimit.Key("provider", provider), providerInterval(provider))
	if err != nil {
		return err
	}
	if limit := providerWaitLimit(provider); wait > limit {
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
