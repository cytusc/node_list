package ratelimit

import (
	"context"
	"testing"
	"time"
)

// Two callers must never start in the same instant: the upstream quotas are
// expressed as "one request per interval", not "N per interval".
func TestAllowSpacesConsecutiveCalls(t *testing.T) {
	l := New(nil)
	ctx := context.Background()
	if wait, err := l.Allow(ctx, "ip-api", 1500*time.Millisecond); err != nil || wait != 0 {
		t.Fatalf("first call wait=%s err=%v", wait, err)
	}
	wait, err := l.Allow(ctx, "ip-api", 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if wait <= 0 || wait > 1500*time.Millisecond {
		t.Fatalf("second call wait=%s, want (0,1500ms]", wait)
	}
	// A different provider has its own clock.
	if wait, _ := l.Allow(ctx, "ipinfo", 1500*time.Millisecond); wait != 0 {
		t.Fatalf("unrelated provider was paced: %s", wait)
	}
}

// Without Redis the clock is per Limiter. Sharing one Limiter is how a caller
// emulates what Redis does across processes.
func TestLocalClockIsSharedOnlyWhenLimiterIsShared(t *testing.T) {
	shared := New(nil)
	ctx := context.Background()
	if wait, _ := shared.Allow(ctx, "ip-api", time.Minute); wait != 0 {
		t.Fatalf("first call waited %s", wait)
	}
	if wait, _ := shared.Allow(ctx, "ip-api", time.Minute); wait <= 0 {
		t.Fatal("shared limiter did not pace the second caller")
	}
	if wait, _ := New(nil).Allow(ctx, "ip-api", time.Minute); wait != 0 {
		t.Fatalf("independent limiter was paced: %s", wait)
	}
}

// A cooldown from an upstream 429 must outlast a pacing interval, and a shorter
// cooldown must not shorten a longer one already in force.
func TestBlockHoldsUntilCooldownExpires(t *testing.T) {
	l := New(nil)
	ctx := context.Background()
	if err := l.Block(ctx, "ip-api", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	wait, err := l.Allow(ctx, "ip-api", 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if wait < time.Second || wait > 2*time.Second {
		t.Fatalf("cooldown wait=%s, want ~2s", wait)
	}
	if err := l.Block(ctx, "ip-api", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if wait, _ := l.Allow(ctx, "ip-api", 1500*time.Millisecond); wait < 500*time.Millisecond {
		t.Fatalf("short cooldown shortened the active one: %s", wait)
	}
}

func TestZeroIntervalDoesNotPace(t *testing.T) {
	l := New(nil)
	for i := 0; i < 5; i++ {
		if wait, err := l.Allow(context.Background(), "any", 0); err != nil || wait != 0 {
			t.Fatalf("call %d waited %s err=%v", i, wait, err)
		}
	}
}

// A nil limiter must still pace rather than panic: the field is optional.
func TestNilLimiterFallsBackToLocalClock(t *testing.T) {
	var l *Limiter
	ctx := context.Background()
	if wait, err := l.Allow(ctx, "nil-key", time.Minute); err != nil || wait != 0 {
		t.Fatalf("first call wait=%s err=%v", wait, err)
	}
	if wait, _ := l.Allow(ctx, "nil-key", time.Minute); wait <= 0 {
		t.Fatal("nil limiter did not pace")
	}
	if err := l.Block(ctx, "nil-key", time.Second); err != nil {
		t.Fatal(err)
	}
}
