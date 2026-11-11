package liveness

import (
	"testing"
	"time"
)

func TestBackoffDoublesPerConsecutiveFailure(t *testing.T) {
	interval := 30 * time.Minute
	for _, tc := range []struct {
		streak int
		want   time.Duration
	}{
		{0, 30 * time.Minute},
		{1, 30 * time.Minute},
		{2, 60 * time.Minute},
		{3, 120 * time.Minute},
		{4, 240 * time.Minute},
	} {
		if got := backoff(interval, tc.streak, 24*time.Hour); got != tc.want {
			t.Fatalf("streak %d: got %s want %s", tc.streak, got, tc.want)
		}
	}
}

func TestBackoffHonoursCap(t *testing.T) {
	interval := 30 * time.Minute
	if got := backoff(interval, 10, 2*time.Hour); got != 2*time.Hour {
		t.Fatalf("cap ignored: got %s", got)
	}
	// An unset cap must still terminate instead of overflowing the duration.
	if got := backoff(interval, 40, 0); got != 24*time.Hour {
		t.Fatalf("default cap not applied: got %s", got)
	}
}

func TestBackoffIgnoresInvalidInterval(t *testing.T) {
	if got := backoff(0, 3, time.Hour); got != 0 {
		t.Fatalf("zero interval changed: got %s", got)
	}
	if got := backoff(-time.Minute, 3, time.Hour); got != -time.Minute {
		t.Fatalf("negative interval changed: got %s", got)
	}
}
