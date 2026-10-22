package transport

import (
	"context"
	"encoding/json"
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

func TestManagerClientBusyCooldownIsSharedAndPoolScoped(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/v1/leases" {
			var req LeaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Kind == "exit" {
				w.WriteHeader(429)
			}
		}
	}))
	defer server.Close()
	client := &ManagerClient{URL: server.URL}
	req := LeaseRequest{Kind: "exit"}
	if err := client.call(context.Background(), http.MethodPost, "/v1/leases", req, nil); !errors.Is(err, ErrManagerBusy) {
		t.Fatalf("busy classification: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.call(ctx, http.MethodPost, "/v1/leases", req, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy admission bypass: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("busy pool hammered manager")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	for _, check := range []struct {
		method, path string
		in           any
	}{
		{http.MethodDelete, "/v1/leases/active", nil},
		{http.MethodPost, "/v1/leases/active/validate", nil},
		{http.MethodPost, "/v1/leases", LeaseRequest{Kind: "proof"}},
		{http.MethodPost, "/v1/leases", LeaseRequest{Kind: "risk"}},
	} {
		if err := client.call(ctx2, check.method, check.path, check.in, nil); err != nil {
			t.Fatalf("busy exit blocked %s: %v", check.path, err)
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("unexpected traffic: %d", calls.Load())
	}
}

func TestManagerBusyBackoffIsBoundedAndConcurrentWaveDoesNotExtendIt(t *testing.T) {
	c := &ManagerClient{}
	for i := 0; i < 12; i++ {
		c.pauseCapacity("exit")
		c.mu.Lock()
		state := c.capacity["exit"]
		c.mu.Unlock()
		delay := time.Until(state.retryAt)
		if delay <= 0 || delay > 30*time.Second {
			t.Fatalf("unbounded delay: %v", delay)
		}
		c.pauseCapacity("exit")
		c.mu.Lock()
		if c.capacity["exit"] != state {
			t.Fatal("same wave extended cooldown")
		}
		state.retryAt = time.Now().Add(-time.Second)
		c.capacity["exit"] = state
		c.mu.Unlock()
	}
	c.capacitySuccess("exit")
	c.pauseCapacity("exit")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity["exit"].failures != 1 {
		t.Fatal("successful admission did not reset backoff")
	}
}

func TestManagerExitCapacityOverride(t *testing.T) {
	m, _, _ := fakeManager(t)
	m.ExitConcurrency = 3
	targets := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	var leases []Lease
	for i := 0; i < 3; i++ {
		l, err := m.Acquire(LeaseRequest{Kind: "exit", Targets: targets})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	if _, err := m.Acquire(LeaseRequest{Kind: "exit", Targets: targets}); !errors.Is(err, ErrManagerBusy) {
		t.Fatalf("capacity exceeded: %v", err)
	}
	m.Release(leases[0].ID)
	if _, err := m.Acquire(LeaseRequest{Kind: "exit", Targets: targets}); err != nil {
		t.Fatal(err)
	}
}
