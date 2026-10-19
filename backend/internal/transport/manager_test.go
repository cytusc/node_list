package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"gopkg.in/yaml.v3"
)

func entry(id string) RegistryEntry {
	n := domain.Node{Type: "http", Server: "127.0.0.1", Port: "80", RawConfig: domain.ProxyConfig{"type": "http", "server": "127.0.0.1", "port": 80}}
	return RegistryEntry{Identity: id, Incarnation: "inc-" + id, Node: n, Digest: ConfigDigest(n)}
}
func fakeManager(t *testing.T) (*Manager, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	failed := &atomic.Bool{}
	reloads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed.Load() {
			w.WriteHeader(500)
			return
		}
		if r.Method == http.MethodPut {
			reloads.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			var config mihomoConfig
			_ = yaml.Unmarshal([]byte(body["payload"]), &config)
			for i, p := range config.Proxies {
				if p["type"] == "invalid" {
					w.WriteHeader(400)
					_ = json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf("proxy %d: invalid", i)})
					return
				}
			}
			w.WriteHeader(204)
			return
		}
		if strings.Contains(r.URL.Path, "/delay") {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "proxy"})
	}))
	t.Cleanup(server.Close)
	return NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890", Secret: "controller"}), failed, reloads
}
func TestManagerFencesReloadExpiryAndRestart(t *testing.T) {
	m, failed, reloads := fakeManager(t)
	ctx := context.Background()
	entries := []RegistryEntry{entry("a"), entry("b")}
	if err := m.Sync(ctx, entries); err != nil {
		t.Fatal(err)
	}
	generation := m.Stats().Generation
	// Display name changes do not reload connection config.
	entries[0].Node.Name = "renamed"
	if err := m.Sync(ctx, entries); err != nil || reloads.Load() != 1 {
		t.Fatalf("unchanged config reloaded: %v", err)
	}
	l, err := m.Acquire(LeaseRequest{Targets: entries[:1], Kind: "strict"})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Validate(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	entries[0].Incarnation = "new"
	done := make(chan error, 1)
	go func() { done <- m.Sync(ctx, entries) }()
	deadline := time.Now().Add(time.Second)
	for m.Stats().Healthy && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err = m.Acquire(LeaseRequest{Targets: entries[:1], Kind: "strict"}); err == nil {
		t.Fatal("new lease admitted while draining")
	}
	select {
	case <-done:
		t.Fatal("reload did not drain lease")
	default:
	}
	m.mu.Lock()
	expired := m.leases[l.ID]
	expired.ExpiresAt = time.Now().Add(-time.Second)
	m.leases[l.ID] = expired
	m.mu.Unlock()
	if err = m.Validate(ctx, l.ID); err == nil {
		t.Fatal("expired lease accepted")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if m.Stats().Generation == generation {
		t.Fatal("reload did not advance generation")
	}
	l, err = m.Acquire(LeaseRequest{Targets: entries[:1], Kind: "strict"})
	if err != nil {
		t.Fatal(err)
	}
	failed.Store(true)
	if err = m.Validate(ctx, l.ID); err == nil || m.Stats().Healthy {
		t.Fatal("controller fault treated as valid proof")
	}
	failed.Store(false)
	if err = m.Sync(ctx, entries); err != nil {
		t.Fatal(err)
	}
	if err = m.Validate(ctx, l.ID); err == nil {
		t.Fatal("old lease survived restart")
	}
}
func TestManagerQuarantinesAndRestores(t *testing.T) {
	m, failed, reloads := fakeManager(t)
	entries := []RegistryEntry{entry("a"), entry("bad")}
	entries[1].Node.RawConfig["type"] = "invalid"
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	if s := m.Stats(); s.Loaded != 1 || s.Quarantined != 1 || reloads.Load() != 2 {
		t.Fatalf("quarantine: %+v", s)
	}
	previous := string(m.lastPayload)
	failed.Store(true)
	entries[0].Incarnation = "changed"
	if err := m.Sync(context.Background(), entries); err == nil {
		t.Fatal("failed update succeeded")
	}
	if string(m.lastPayload) != previous || m.Stats().Healthy {
		t.Fatal("failed update replaced valid state")
	}
}
func TestManagerAPIAuthAndDelaySeparation(t *testing.T) {
	m, _, _ := fakeManager(t)
	e := entry("a")
	if err := m.Sync(context.Background(), []RegistryEntry{e}); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(m.Handler("internal"))
	defer api.Close()
	resp, err := http.Get(api.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("missing auth accepted")
	}
	result, err := m.Delay(context.Background(), LeaseRequest{Targets: []RegistryEntry{e}})
	if err != nil || result.Success || result.Generation == "" {
		t.Fatalf("node timeout must remain a fast-only failure: %+v %v", result, err)
	}
}
func sparePort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func realManager(t *testing.T) *Manager {
	t.Helper()
	binary := os.Getenv("MIHOMO_TEST_BINARY")
	if binary == "" {
		t.Skip("set MIHOMO_TEST_BINARY for isolated kernel integration")
	}
	control, proxy := sparePort(t), sparePort(t)
	dir := t.TempDir()
	config := filepath.Join(dir, "bootstrap.yaml")
	raw := fmt.Sprintf("mixed-port: %d\nallow-lan: true\nexternal-controller: 127.0.0.1:%d\nsecret: controller\nmode: rule\nauthentication: [disabled:disabled]\nrules: [\"MATCH,REJECT\"]\n", proxy, control)
	if err := os.WriteFile(config, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-d", dir, "-f", config)
	logFile, err := os.Create(filepath.Join(dir, "kernel.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	m := NewManager(MihomoConfig{Controller: fmt.Sprintf("http://127.0.0.1:%d", control), ProxyURL: fmt.Sprintf("http://127.0.0.1:%d", proxy), Secret: "controller"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, status, err := m.controller(context.Background(), "GET", "/version", nil)
		if err == nil && status == 200 {
			break
		}
		if time.Now().After(deadline) {
			raw, _ := os.ReadFile(filepath.Join(dir, "kernel.log"))
			t.Fatalf("isolated kernel failed to start: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return m
}
func TestRealManagerRouteIsolation(t *testing.T) {
	m := realManager(t)
	ctx := context.Background()
	entries := []RegistryEntry{}
	for _, label := range []string{"a", "b"} {
		label := label
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, label) }))
		defer server.Close()
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		e := entry(label)
		e.Node.Port = port
		e.Node.RawConfig["port"] = atoi(port)
		e.Digest = ConfigDigest(e.Node)
		entries = append(entries, e)
	}
	if err := m.Sync(ctx, entries); err != nil {
		t.Fatal(err)
	}
	l, err := m.Acquire(LeaseRequest{Targets: entries, Kind: "strict"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Release(l.ID)
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			index := i % 2
			c := listenerClient(l.URLs[index])
			defer c.CloseIdleConnections()
			resp, e := c.Get("http://route-isolation.invalid/check")
			if e != nil {
				t.Error(e)
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if string(raw) != entries[index].Identity {
				t.Errorf("route crossed: %q", raw)
			}
		}(i)
	}
	wg.Wait()
	for index, u := range []string{m.Instance.ProxyURL, strings.Replace(l.URLs[0], m.credentials["a"], "wrong", 1)} {
		c := listenerClient(u)
		resp, e := c.Get("http://route-isolation.invalid/check")
		c.CloseIdleConnections()
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		expected := 407
		if index == 1 {
			expected = 403
		}
		if resp.StatusCode != expected {
			t.Fatalf("authentication bypass: %d", resp.StatusCode)
		}
	}
	// An authenticated user with no matching IN-USER rule must still be rejected.
	var config mihomoConfig
	_ = yaml.Unmarshal(m.lastPayload, &config)
	config.Authentication = append(config.Authentication, "orphan:password")
	payload, _ := marshalMihomoConfig(config)
	if _, e := m.push(ctx, payload); e != nil {
		t.Fatal(e)
	}
	c := listenerClient(strings.Replace(m.Instance.ProxyURL, "http://", "http://orphan:password@", 1))
	resp, e := c.Get("http://route-isolation.invalid/check")
	c.CloseIdleConnections()
	if e == nil {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 200 || string(raw) == "a" || string(raw) == "b" {
			t.Fatal("unmatched authenticated request escaped rejection")
		}
	}
}
func TestRealManagerThousandTimeouts(t *testing.T) {
	if os.Getenv("MIHOMO_TIMEOUT_BENCH") != "1" {
		t.Skip("set MIHOMO_TIMEOUT_BENCH=1 for the 1000-node, 2-second timeout acceptance test")
	}
	m := realManager(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2200 * time.Millisecond); w.WriteHeader(502) }))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	entries := make([]RegistryEntry, 1000)
	for i := range entries {
		entries[i] = entry(fmt.Sprint(i))
		entries[i].Node.RawConfig["port"] = atoi(port)
		entries[i].Node.Port = port
		entries[i].Digest = ConfigDigest(entries[i].Node)
	}
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	jobs := make(chan RegistryEntry)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				v, err := m.Delay(context.Background(), LeaseRequest{Targets: []RegistryEntry{e}})
				if err != nil || v.Success {
					t.Errorf("expected node timeout: %+v %v", v, err)
				}
			}
		}()
	}
	for _, e := range entries {
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("1000 2-second timeouts, concurrency 32: %s", elapsed)
	if elapsed > 90*time.Second {
		t.Fatalf("timeout round exceeded 90 seconds: %s", elapsed)
	}
}

func TestFastResidentialReservationAndBorrowing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, _, _ := fakeManager(t)
	if err := m.Sync(ctx, []RegistryEntry{entry("a")}); err != nil {
		t.Fatal(err)
	}
	// Simulate a full ordinary lane and a queued residential call.
	m.mu.Lock()
	m.fast = 32
	m.fastNormal = 32
	m.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := m.Delay(ctx, LeaseRequest{Targets: []RegistryEntry{entry("a")}, High: true})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		waiting := m.highWaiting
		m.mu.Unlock()
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("residential did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	// One normal completion must admit the waiting residential request.
	m.mu.Lock()
	m.fast--
	m.fastNormal--
	m.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("residential starved")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.highWaiting != 0 || m.fast != 31 || m.fastNormal != 31 {
		t.Fatalf("reserved capacity not returned: waiting=%d fast=%d normal=%d", m.highWaiting, m.fast, m.fastNormal)
	}
}

func TestManagerBatchIsolationPreservesRegistry(t *testing.T) {
	m, _, _ := fakeManager(t)
	entries := make([]RegistryEntry, 300)
	for i := range entries {
		entries[i] = entry(fmt.Sprintf("batch-%03d", i))
		if i%2 == 0 {
			entries[i].Node.RawConfig["type"] = "invalid"
		}
		entries[i].Digest = ConfigDigest(entries[i].Node)
	}
	before, _ := json.Marshal(entries)
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(entries)
	if string(before) != string(after) {
		t.Fatal("batch validation mutated registry")
	}
	if s := m.Stats(); s.Loaded != 150 || s.Quarantined != 150 || !s.Healthy {
		t.Fatalf("batch isolation: %+v", s)
	}
}

func TestManagerSuccessfulRollbackKeepsPriorGeneration(t *testing.T) {
	var fail atomic.Bool
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && fail.Swap(false) {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer controller.Close()
	m := NewManager(MihomoConfig{Controller: controller.URL, ProxyURL: "http://localhost:7890"})
	entries := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	generation := m.Stats().Generation
	entries[0].Incarnation = "changed"
	fail.Store(true)
	if err := m.Sync(context.Background(), entries); err == nil {
		t.Fatal("invalid update accepted")
	}
	if s := m.Stats(); !s.Healthy || s.Generation != generation {
		t.Fatalf("rollback lost usable prior state: %+v", s)
	}
	if _, err := m.Acquire(LeaseRequest{Targets: []RegistryEntry{entry("a")}, Kind: "strict"}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPauseIsBackpressureWithoutAbnormalResult(t *testing.T) {
	m, _, _ := fakeManager(t)
	if err := m.Sync(context.Background(), []RegistryEntry{entry("a")}); err != nil {
		t.Fatal(err)
	}
	var counted atomic.Int32
	m.OnDelay = func(context.Context, bool, bool) { counted.Add(1) }
	m.mu.Lock()
	m.reloading = true
	m.mu.Unlock()
	api := httptest.NewServer(m.Handler("test"))
	defer api.Close()
	client := ManagerClient{URL: api.URL, Token: "test"}
	_, err := client.Delay(context.Background(), entry("a").Node, "", false)
	if !errors.Is(err, ErrManagerBusy) || counted.Load() != 0 {
		t.Fatalf("pause became abnormal: %v count=%d", err, counted.Load())
	}
}

func TestRealKernelDelayRequiresExpected204(t *testing.T) {
	m := realManager(t)
	e := entry("status-check")
	e.Node.RawConfig["type"] = "direct"
	e.Node.Type = "direct"
	e.Digest = ConfigDigest(e.Node)
	ctx := context.Background()
	if err := m.Sync(ctx, []RegistryEntry{e}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{200, 204, 403} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		_, got, err := m.controller(ctx, "GET", "/proxies/"+proxyName(e.Identity)+"/delay?timeout=2000&expected=204&url="+url.QueryEscape(server.URL), nil)
		server.Close()
		if err != nil || (got == 200) != (status == 204) {
			t.Fatalf("HTTP %d accepted incorrectly: controller=%d err=%v", status, got, err)
		}
	}
}
