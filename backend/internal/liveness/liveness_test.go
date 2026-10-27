package liveness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConservativeRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Proxy
		want bool
	}{
		{"http", Proxy{"type": "http", "server": "127.0.0.1", "port": 80}, true},
		{"tls-vless", Proxy{"type": "vless", "uuid": "123e4567-e89b-12d3-a456-426614174000", "tls": true}, true},
		{"insecure", Proxy{"type": "trojan", "password": "secret", "skip-cert-verify": true}, false},
		{"plain-vless", Proxy{"type": "vless", "uuid": "id"}, false},
		{"hysteria2", Proxy{"type": "hysteria2", "password": "secret"}, false},
		{"vmess-legacy", Proxy{"type": "vmess", "uuid": "id", "alterId": 64}, false},
		{"unused-tls-options", Proxy{"type": "http", "sni": "example.com"}, false},
		{"empty-reality", Proxy{"type": "vless", "uuid": "id", "tls": true, "reality-opts": Proxy{}}, false},
		{"plugin", Proxy{"type": "ss", "cipher": "aes-128-gcm", "password": "secret", "plugin": "obfs"}, false},
		{"wrong-protocol-option", Proxy{"type": "http", "uuid": "ignored"}, false},
		{"wrong-transport-option", Proxy{"type": "http", "ws-opts": Proxy{"path": "/"}}, false},
		{"invalid-type", Proxy{"type": "http", "tls": "true"}, false},
		{"grpc-extra", Proxy{"type": "vless", "uuid": "id", "tls": true, "network": "grpc", "grpc-opts": Proxy{"grpc-service-name": "svc", "extra": true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, reason := XrayOutbound(tc.p)
			if (reason == "") != tc.want {
				t.Fatalf("routing reason %q", reason)
			}
		})
	}
}
func TestHTTPRequiresExpectedStatusAndDeadline(t *testing.T) {
	for _, code := range []int{204, 200, 302, 500} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Error("wrong method")
			}
			w.WriteHeader(code)
		}))
		_, err := request(context.Background(), &http.Transport{}, s.URL, 204)
		s.Close()
		if (err == nil) != (code == 204) {
			t.Fatalf("status %d: %v", code, err)
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := request(ctx, &http.Transport{}, s.URL, 204)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestHungKernelIsKilledAndReaped(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kernel")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := ProcessEngine{Name: "xray", Binary: bin, Target: Target, Expected: 204, Timeout: 70 * time.Millisecond}
	start := time.Now()
	r := e.Probe(context.Background(), Proxy{"type": "http", "server": "127.0.0.1", "port": 80})
	if r.Status != "infrastructure" || r.Reason != "startup_timeout" || time.Since(start) > time.Second {
		t.Fatalf("unbounded or invalid result: %+v", r)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("unexpected files")
	}
}

func TestUntrustedTargetIsRejected(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := request(ctx, &http.Transport{}, s.URL, 204); err == nil {
		t.Fatal("untrusted target certificate accepted")
	}
}

func TestFailedControlPreservesAvailabilityVerdict(t *testing.T) {
	st := &memoryStore{nodes: []Candidate{candidate("control", "hysteria2")}, saved: map[string]Result{}, done: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := fakeEngine(func(context.Context, Proxy) Result {
		return Result{Engine: "mihomo", Status: "unreachable", CheckedAt: time.Now()}
	})
	r := Runner{Store: st, Xray: failed, Mihomo: failed, XrayWorkers: 1, MihomoWorkers: 1, Interval: time.Hour, Retry: time.Minute, Control: func(context.Context) bool { return false }}
	finished := make(chan error, 1)
	go func() { finished <- r.Run(ctx) }()
	select {
	case <-st.done:
	case <-time.After(time.Second):
		t.Fatal("control result did not complete")
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	v := st.saved["control"]
	if v.Valid() || v.Status != "infrastructure" || v.Reason != "control_unavailable" {
		t.Fatalf("control outage produced availability verdict: %+v", v)
	}
}

type fakeEngine func(context.Context, Proxy) Result

func (f fakeEngine) Probe(c context.Context, p Proxy) Result { return f(c, p) }

type memoryStore struct {
	mu    sync.Mutex
	nodes []Candidate
	saved map[string]Result
	done  chan string
}

func (s *memoryStore) Due(context.Context, int) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Candidate
	for _, n := range s.nodes {
		if _, ok := s.saved[n.Identity]; !ok {
			out = append(out, n)
		}
	}
	return out, nil
}
func (s *memoryStore) Save(_ context.Context, n Candidate, r Result, _ time.Duration) error {
	s.mu.Lock()
	s.saved[n.Identity] = r
	s.mu.Unlock()
	s.done <- n.Identity
	return nil
}
func (s *memoryStore) Heartbeat(context.Context, any) error { return nil }
func candidate(id, proto string) Candidate {
	b, _ := json.Marshal(map[string]any{"rawConfig": Proxy{"type": proto, "server": "127.0.0.1", "port": 80}})
	return Candidate{Identity: id, Node: b}
}
func TestIndependentPoolsAndSingleFallbackResult(t *testing.T) {
	st := &memoryStore{saved: map[string]Result{}, done: make(chan string, 20)}
	for i := 0; i < 8; i++ {
		st.nodes = append(st.nodes, candidate(string(rune('a'+i)), "hysteria2"))
	}
	st.nodes = append(st.nodes, candidate("xray", "http"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	x := fakeEngine(func(context.Context, Proxy) Result {
		return Result{Engine: "xray", Status: "reachable", Reachable: true, CheckedAt: time.Now()}
	})
	m := fakeEngine(func(ctx context.Context, _ Proxy) Result { <-ctx.Done(); return Result{Status: "cancelled"} })
	r := Runner{Store: st, Xray: x, Mihomo: m, XrayWorkers: 1, MihomoWorkers: 1, Interval: time.Hour, Retry: time.Minute, Control: func(context.Context) bool { return true }}
	go func() { finished <- r.Run(ctx) }()
	select {
	case id := <-st.done:
		if id != "xray" {
			t.Fatal(id)
		}
	case <-time.After(time.Second):
		t.Fatal("Mihomo queue blocked Xray")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	// Failure of Xray is never persisted before the fallback verdict.
	st = &memoryStore{nodes: []Candidate{candidate("fallback", "http")}, saved: map[string]Result{}, done: make(chan string, 10)}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	r.Store = st
	r.Xray = fakeEngine(func(context.Context, Proxy) Result { return Result{Engine: "xray", Status: "unreachable"} })
	r.Mihomo = fakeEngine(func(context.Context, Proxy) Result {
		return Result{Engine: "mihomo", Status: "reachable", Reachable: true, CheckedAt: time.Now()}
	})
	go func() { finished <- r.Run(ctx) }()
	select {
	case <-st.done:
	case <-time.After(time.Second):
		t.Fatal("fallback never completed")
	}
	cancel()
	<-finished
	st.mu.Lock()
	defer st.mu.Unlock()
	v := st.saved["fallback"]
	if !v.Reachable || v.Engine != "mihomo" || v.Fallback == "" || len(st.done) != 0 {
		t.Fatalf("wrong final verdict: %+v", v)
	}
}

func TestBackupTargetUsesRemainingDeadline(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer primary.Close()
	var calls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer backup.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, target, results, err := requestTargets(ctx, &http.Transport{}, []string{primary.URL, backup.URL}, 204)
	if err != nil || target != backup.URL || results[primary.URL] || !results[backup.URL] || calls.Load() != 1 {
		t.Fatalf("backup not reached within deadline: target=%q results=%v err=%v", target, results, err)
	}
	_, target, results, err = requestTargets(ctx, &http.Transport{}, []string{backup.URL, primary.URL}, 204)
	if err != nil || target != backup.URL || len(results) != 1 {
		t.Fatalf("successful primary did not stop: %v %v", results, err)
	}
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer untrusted.Close()
	_, _, results, err = requestTargets(ctx, &http.Transport{}, []string{untrusted.URL, untrusted.URL + "/backup"}, 204)
	if err == nil || len(results) != 2 {
		t.Fatalf("untrusted TLS accepted: %v %v", results, err)
	}
}

func TestConcurrentFailuresShareControlCheck(t *testing.T) {
	st := &memoryStore{saved: map[string]Result{}, done: make(chan string, 8)}
	for i := 0; i < 8; i++ {
		st.nodes = append(st.nodes, candidate(string(rune('a'+i)), "hysteria2"))
	}
	var calls atomic.Int32
	failed := fakeEngine(func(context.Context, Proxy) Result {
		return Result{Engine: "mihomo", Status: "unreachable", CheckedAt: time.Now()}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := Runner{Store: st, Xray: failed, Mihomo: failed, XrayWorkers: 1, MihomoWorkers: 4, Interval: time.Hour, Retry: time.Minute, Control: func(context.Context) bool { calls.Add(1); return false }}
	finished := make(chan error, 1)
	go func() { finished <- r.Run(ctx) }()
	for range st.nodes {
		select {
		case <-st.done:
		case <-time.After(time.Second):
			t.Fatal("failed probes did not complete")
		}
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("control checked %d times", calls.Load())
	}
	for _, r := range st.saved {
		if r.Status != "infrastructure" {
			t.Fatalf("wrong verdict: %+v", r)
		}
	}
}
