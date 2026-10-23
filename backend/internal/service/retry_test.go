package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

func TestRetryDelay(t *testing.T) {
	plain := errors.New("probe failed")
	if got := retryDelay(Job{Attempts: 1}, plain); got != 10*time.Second {
		t.Fatalf("first retry: %s", got)
	}
	if got := retryDelay(Job{Attempts: 4}, plain); got != 40*time.Second {
		t.Fatalf("increasing retry: %s", got)
	}
	if got := retryDelay(Job{Attempts: 40}, plain); got != 5*time.Minute {
		t.Fatalf("capped retry: %s", got)
	}
	if got := retryDelay(Job{Attempts: 3}, fmt.Errorf("exit probe: %w", context.DeadlineExceeded)); got != 30*time.Minute {
		t.Fatalf("timeout breaker: %s", got)
	}
	if got := retryDelay(Job{Attempts: 2}, context.DeadlineExceeded); got != 20*time.Second {
		t.Fatalf("early timeout retry: %s", got)
	}
	if got := retryDelay(Job{Attempts: 3}, dependencyWait(2)); got != 2*time.Second {
		t.Fatalf("waiting delay: %s", got)
	}
	if got := retryDelay(Job{Attempts: 3}, transport.ErrConfigRejected); got != 6*time.Hour {
		t.Fatalf("rejected delay: %s", got)
	}
	for i := 0; i < 20; i++ {
		if got := retryDelay(Job{Attempts: 5}, transport.ErrManagerBusy); got < 5*time.Second || got >= 10*time.Second {
			t.Fatalf("busy window: %s", got)
		}
	}
}
