// Package ratelimit paces outbound provider calls.
//
// The pacing clock is shared across replicas through Redis using GCRA
// (github.com/go-redis/redis_rate). When no Redis client is configured the
// limiter degrades to an in-process token bucket, which keeps tests and
// single-replica deployments working without pretending to be shared.
package ratelimit

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// cooldownPrefix marks a key that must not be called until its TTL expires.
const cooldownPrefix = "cooldown:"

// Limiter paces calls per key. It is safe for concurrent use.
type Limiter struct {
	rdb *redis.Client
	// shared is used by every Limiter without Redis. A caller that wants two
	// repositories to share the in-process clock passes the same Limiter to
	// both, which mirrors what Redis does across processes.
	shared *local
}

type local struct {
	mu        sync.Mutex
	limiters  map[string]*rate.Limiter
	cooldowns map[string]time.Time
}

// New builds a limiter. A nil client keeps everything in-process.
func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, shared: newLocal()}
}

func newLocal() *local {
	return &local{limiters: map[string]*rate.Limiter{}, cooldowns: map[string]time.Time{}}
}

// Every describes one request per period, with no burst: two callers cannot
// start at the same instant, which is what the upstream quotas expect.
func Every(period time.Duration) redis_rate.Limit {
	return redis_rate.Limit{Rate: 1, Burst: 1, Period: period}
}

// Allow reports how long the caller must wait before calling key. A zero
// duration means the slot is free now. The wait is advisory: callers decide
// whether to sleep, skip the provider or retry later.
// fallback paces callers that have no limiter at all, so a nil field can never
// turn into an unpaced provider.
var fallback = newLocal()

// localClock returns the in-process clock to use when Redis is not available.
func (l *Limiter) localClock() *local {
	if l == nil || l.shared == nil {
		return fallback
	}
	return l.shared
}

func (l *Limiter) Allow(ctx context.Context, key string, every time.Duration) (time.Duration, error) {
	if every <= 0 {
		return 0, nil
	}
	if l == nil || l.rdb == nil {
		return l.localClock().allow(key, every), nil
	}
	if wait, blocked, err := l.cooldown(ctx, key); err != nil {
		// Redis trouble must not stop outbound traffic: fall back to the local
		// clock so the provider still gets paced, just not cluster-wide.
		return l.localClock().allow(key, every), nil
	} else if blocked {
		return wait, nil
	}
	result, err := redis_rate.NewLimiter(l.rdb).Allow(ctx, key, Every(every))
	if err != nil {
		return l.localClock().allow(key, every), nil
	}
	if result.Allowed > 0 {
		return 0, nil
	}
	return result.RetryAfter, nil
}

// Block keeps every replica away from key for at least cooldown, which is how
// an upstream 429 or an exhausted daily quota is honoured.
func (l *Limiter) Block(ctx context.Context, key string, cooldown time.Duration) error {
	if cooldown <= 0 {
		return nil
	}
	if l == nil || l.rdb == nil {
		l.localClock().block(key, cooldown)
		return nil
	}
	// Extend rather than overwrite: a short cooldown arriving after a long one
	// must not re-open the provider early.
	if ttl, err := l.rdb.PTTL(ctx, cooldownPrefix+key).Result(); err == nil && ttl >= cooldown {
		return nil
	}
	if err := l.rdb.Set(ctx, cooldownPrefix+key, 1, cooldown).Err(); err != nil {
		l.localClock().block(key, cooldown)
		return err
	}
	return nil
}

func (l *Limiter) cooldown(ctx context.Context, key string) (time.Duration, bool, error) {
	ttl, err := l.rdb.PTTL(ctx, cooldownPrefix+key).Result()
	if err != nil && !isMissing(err) {
		return 0, false, err
	}
	if ttl <= 0 {
		return 0, false, nil
	}
	return ttl, true, nil
}

func isMissing(err error) bool {
	return err == redis.Nil
}

func (l *local) allow(key string, every time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, ok := l.cooldowns[key]; ok {
		if wait := time.Until(until); wait > 0 {
			return wait
		}
		delete(l.cooldowns, key)
	}
	limiter, ok := l.limiters[key]
	if !ok {
		// Burst 1 with the interval as the token refill: the same spacing the
		// shared clock enforces, so a single replica behaves identically.
		limiter = rate.NewLimiter(rate.Every(every), 1)
		l.limiters[key] = limiter
	}
	reservation := limiter.Reserve()
	if delay := reservation.Delay(); delay > 0 {
		// Reserve() consumed a token; hand it back so the caller's next attempt
		// is not penalised twice for one wait.
		reservation.Cancel()
		return delay
	}
	return 0
}

func (l *local) block(key string, cooldown time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until := time.Now().Add(cooldown)
	if until.After(l.cooldowns[key]) {
		l.cooldowns[key] = until
	}
}

// Key builds a stable, readable rate-limit key.
func Key(parts ...string) string {
	return strings.Join(parts, ":")
}
