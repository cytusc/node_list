package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerClientSharesOutageCooldownAndAllowsRelease(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	client := &ManagerClient{URL: server.URL}
	var failure *ManagerHTTPError
	if err := client.call(context.Background(), http.MethodPost, "/v1/leases", nil, nil); !errors.As(err, &failure) || failure.Status != 503 {
		t.Fatalf("503 classification lost: %v", err)
	}
	// Another lane must wait without issuing HTTP or spending a full cooldown.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.call(ctx, http.MethodPost, "/v1/leases", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cooldown ignored caller deadline: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("another lane hammered the unavailable manager: %d", calls.Load())
	}
	if err := client.call(context.Background(), http.MethodDelete, "/v1/leases/old", nil, nil); err != nil {
		t.Fatalf("cooldown prevented lease release: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("release did not reach manager")
	}
	client.mu.Lock()
	client.retryAt = time.Now().Add(-time.Second)
	client.mu.Unlock()
	if err := client.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerClientCancellationDoesNotPauseOtherLanes(t *testing.T) {
	client := &ManagerClient{URL: "http://127.0.0.1:1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.call(ctx, http.MethodPost, "/v1/leases", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if !client.retryAt.IsZero() {
		t.Fatal("one caller paused other lanes")
	}
}
