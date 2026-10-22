package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerDelayUsesFreshExpectedStatusEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		alive   bool
		delay   int
		defect  string
		wantErr bool
	}{
		{"204 rounded to zero", 503, true, 0, "", false},
		{"wrong status but positive delay", 200, false, 0, "", false},
		{"confirmed timeout", 504, false, 0, "", false},
		{"success", 200, true, 23, "", false},
		{"missing history", 200, true, 1, "history", true},
		{"missing alive", 200, true, 1, "alive", true},
		{"wrong URL", 200, true, 1, "url", true},
		{"stale history", 200, true, 1, "stale", true},
		{"invalid response", 200, true, 1, "json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					return
				}
				if strings.HasSuffix(r.URL.Path, "/delay") {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"delay":23}`))
					return
				}
				at := time.Now()
				if tc.defect == "stale" {
					at = at.Add(-time.Hour)
				}
				state := map[string]any{"alive": tc.alive, "history": []map[string]any{{"time": at, "delay": tc.delay}}}
				target := DelayTarget
				switch tc.defect {
				case "url":
					target = "https://other.invalid/"
				case "alive":
					delete(state, "alive")
				case "history":
					delete(state, "history")
				case "json":
					_, _ = w.Write([]byte("invalid"))
					return
				}
				// Top-level alive does not enforce expected=204 and must not be used.
				_ = json.NewEncoder(w).Encode(map[string]any{"alive": true, "extra": map[string]any{target: state}})
			}))
			defer server.Close()
			m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
			entries := []RegistryEntry{entry("a")}
			if err := m.Sync(context.Background(), entries); err != nil {
				t.Fatal(err)
			}
			result, err := m.Delay(context.Background(), LeaseRequest{Targets: entries})
			if (err != nil) != tc.wantErr {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.wantErr {
				if !result.CheckedAt.IsZero() || result.Generation != "" {
					t.Fatal("uncertain state became evidence")
				}
			} else if result.Success != tc.alive || result.Delay != tc.delay || result.CheckedAt.IsZero() {
				t.Fatalf("incorrect evidence: %+v", result)
			}
			if !m.Stats().Healthy {
				t.Fatal("bad evidence invalidated global health")
			}
		})
	}
}

func TestManagerDelayCancellationDrainsBeforeSameIdentityRetry(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/delay") {
			if calls.Add(1) == 1 {
				close(entered)
				<-finish
			}
			_, _ = w.Write([]byte(`{"delay":1}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"extra": map[string]any{DelayTarget: map[string]any{"alive": true, "history": []map[string]any{{"time": time.Now(), "delay": 1}}}}})
	}))
	defer server.Close()
	defer unblock()
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
	entries := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := m.Delay(ctx, LeaseRequest{Targets: entries}); returned <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not enter")
	}
	if _, err := m.Delay(context.Background(), LeaseRequest{Targets: entries}); !errors.Is(err, ErrManagerBusy) {
		t.Fatalf("concurrent history overwrite allowed: %v", err)
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	if _, err := m.Delay(context.Background(), LeaseRequest{Targets: entries}); !errors.Is(err, ErrManagerBusy) {
		t.Fatalf("retry overlapped draining probe: %v", err)
	}
	unblock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		result, err := m.Delay(context.Background(), LeaseRequest{Targets: entries})
		if err == nil {
			if !result.Success {
				t.Fatal("retry lost proof")
			}
			break
		}
		if !errors.Is(err, ErrManagerBusy) || time.Now().After(deadline) {
			t.Fatalf("drain did not release identity: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("overlapping probes: %d", calls.Load())
	}
}
