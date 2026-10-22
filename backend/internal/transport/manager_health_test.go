package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerCallerCancellationDoesNotInvalidateOtherLeases(t *testing.T) {
	for _, operation := range []string{"check", "validate"} {
		for _, expired := range []bool{false, true} {
			t.Run(operation+"/expired="+strconv.FormatBool(expired), func(t *testing.T) {
				m, _, _ := fakeManager(t)
				targets := []RegistryEntry{entry("a")}
				if err := m.Sync(context.Background(), targets); err != nil {
					t.Fatal(err)
				}
				lease, err := m.Acquire(LeaseRequest{Targets: targets, Kind: "exit"})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				if expired {
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				}
				cancel()
				if operation == "check" {
					err = m.Check(ctx)
				} else {
					err = m.Validate(ctx, lease.ID)
				}
				if !errors.Is(err, ctx.Err()) {
					t.Fatalf("cancellation lost: %v, want %v", err, ctx.Err())
				}
				if !m.Stats().Healthy {
					t.Fatal("caller cancellation invalidated the shared kernel")
				}
				if err := m.Validate(context.Background(), lease.ID); err != nil {
					t.Fatalf("unrelated lease was lost: %v", err)
				}
			})
		}
	}
}

func TestManagerValidationConfirmsTransientControllerFailure(t *testing.T) {
	var failOnce atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if failOnce.Swap(false) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
	targets := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(LeaseRequest{Targets: targets, Kind: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	failOnce.Store(true)
	if err = m.Validate(context.Background(), lease.ID); err == nil {
		t.Fatal("failed validation accepted as proof")
	}
	if !m.Stats().Healthy {
		t.Fatal("single transient response poisoned shared health")
	}
	if err = m.Validate(context.Background(), lease.ID); err != nil {
		t.Fatalf("healthy controller could not validate again: %v", err)
	}
}

func TestManagerInFlightCancellationDoesNotInvalidateKernel(t *testing.T) {
	for _, operation := range []string{"check", "validate"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					return
				}
				close(entered)
				<-r.Context().Done()
			}))
			defer server.Close()
			m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
			if err := m.Sync(context.Background(), []RegistryEntry{entry("a")}); err != nil {
				t.Fatal(err)
			}
			lease, err := m.Acquire(LeaseRequest{Targets: []RegistryEntry{entry("a")}, Kind: "exit"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if operation == "check" {
					done <- m.Check(ctx)
				} else {
					done <- m.Validate(ctx, lease.ID)
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("controller request did not start")
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if !m.Stats().Healthy {
				t.Fatal("cancelled request invalidated global state")
			}
		})
	}
}

func TestManagerHealthProbeTimeoutStillInvalidatesKernel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			<-r.Context().Done()
		}
	}))
	defer server.Close()
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
	if err := m.Sync(context.Background(), []RegistryEntry{entry("a")}); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(context.Background()); err == nil || m.Stats().Healthy {
		t.Fatalf("unresponsive controller considered healthy: %v", err)
	}
	if err := m.Generation(context.Background(), m.Stats().Generation); err == nil {
		t.Fatal("unhealthy kernel accepted as commit evidence")
	}
}

func TestRealManagerCancellationAndRecoveryThroughHTTP(t *testing.T) {
	m := realManager(t)
	ctx := context.Background()
	targets := []RegistryEntry{entry("a")}
	if err := m.Sync(ctx, targets); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(m.Handler("internal"))
	defer api.Close()
	client := &ManagerClient{URL: api.URL, Token: "internal"}
	request := LeaseRequest{Targets: targets, Kind: "exit"}
	var lease Lease
	if err := client.call(ctx, http.MethodPost, "/v1/leases", request, &lease); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.Validate(cancelled, lease.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if err := client.call(ctx, http.MethodPost, "/v1/leases/"+lease.ID+"/validate", nil, nil); err != nil {
		t.Fatalf("another request could not use the kernel: %v", err)
	}
	// Remove all registered proxies from this isolated real kernel, as happens
	// after state loss. The active generation and every old lease must be fenced.
	config := fmt.Sprintf("mixed-port: %s\nexternal-controller: %s\nsecret: controller\nmode: rule\nrules: [\"MATCH,REJECT\"]\n", strings.TrimPrefix(m.Instance.ProxyURL, "http://127.0.0.1:"), strings.TrimPrefix(m.Instance.Controller, "http://"))
	if _, err := m.push(ctx, []byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(ctx); err == nil || m.Stats().Healthy {
		t.Fatal("real kernel state loss was not detected")
	}
	if err := client.Generation(ctx, lease.Generation); err == nil {
		t.Fatal("lost kernel accepted as evidence")
	}
	if err := m.Sync(ctx, targets); err != nil {
		t.Fatal(err)
	}
	if m.Stats().Generation == lease.Generation {
		t.Fatal("recovery reused the lost generation")
	}
	if err := m.Validate(ctx, lease.ID); err == nil {
		t.Fatal("old lease survived recovery")
	}
	if err := client.call(ctx, http.MethodPost, "/v1/leases", request, &lease); err != nil {
		t.Fatalf("HTTP client failed after recovery: %v", err)
	}
	if err := client.call(ctx, http.MethodPost, "/v1/leases/"+lease.ID+"/validate", nil, nil); err != nil {
		t.Fatalf("replacement lease invalid: %v", err)
	}
}

func TestManagerStaleValidationCannotInvalidateReloadedKernel(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/proxies/") && block.Swap(false) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	defer close(release)
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
	targets := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(LeaseRequest{Targets: targets, Kind: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	done := make(chan error, 1)
	go func() { done <- m.Validate(context.Background(), lease.ID) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("validation did not start")
	}
	// Expiry/release permits reload while the old controller response is in flight.
	m.Release(lease.ID)
	targets[0].Incarnation = "replacement"
	if err := m.Sync(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	if err := <-done; err == nil {
		t.Fatal("stale lease accepted")
	}
	if !m.Stats().Healthy {
		t.Fatal("old generation response invalidated a replacement kernel")
	}
}

func TestManagerTransientHealthTimeoutKeepsUnrelatedLease(t *testing.T) {
	var stall atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			return
		}
		if stall.Swap(false) {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
	targets := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(LeaseRequest{Targets: targets, Kind: "exit"})
	if err != nil {
		t.Fatal(err)
	}
	stall.Store(true)
	if err := m.Check(context.Background()); err != nil || !m.Stats().Healthy {
		t.Fatalf("transient health timeout caused reload: %v", err)
	}
	if err := m.Validate(context.Background(), lease.ID); err != nil {
		t.Fatalf("unrelated lease lost: %v", err)
	}
}

func TestManagerWaitingHealthCheckHonorsCallerDeadline(t *testing.T) {
	m, _, _ := fakeManager(t)
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Check(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health serialization ignored caller cancellation")
	}
}
