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

func TestManagerClientPreservesOnlyFixedRejectionReasons(t *testing.T) {
	for _, reason := range []string{"missing-credentials", "password=private-secret"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeManagerError(w, &ConfigRejection{Reason: reason}) }))
		client := &ManagerClient{URL: server.URL}
		err := client.call(context.Background(), http.MethodPost, "/v1/leases", nil, nil)
		server.Close()
		want := "missing-credentials"
		if reason != "missing-credentials" {
			want = "link-format"
		}
		if !errors.Is(err, ErrConfigRejected) || ConfigRejectionCode(err) != want || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("unsafe or missing reason: %v", err)
		}
	}
}

func TestManagerSyncIncludesQilanECHResolverPolicy(t *testing.T) {
	var config mihomoConfig
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := yaml.Unmarshal([]byte(body["payload"]), &config); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	proxy, err := domain.ShareLinkProxy("vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&ech=cloudflare-ech.com+https://dns.alidns.com/dns-query", "ech-probe")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{Type: "vless", Server: "example.com", Port: "443", RawConfig: proxy}
	registryEntry := RegistryEntry{Identity: "qilan-ech", Incarnation: "qilan-ech-test", Node: node, Digest: ConfigDigest(node)}
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://127.0.0.1:7890"})
	if err := m.Sync(context.Background(), []RegistryEntry{registryEntry}); err != nil {
		t.Fatal(err)
	}
	wantResolver := "https://dns.alidns.com/dns-query"
	if config.DNS == nil || !config.DNS.Enable || len(config.DNS.ProxyServerNameserver) != 1 || config.DNS.ProxyServerNameserver[0] != wantResolver {
		t.Fatalf("Manager.Sync omitted Qilan's ECH resolver: %#v", config.DNS)
	}
	if got := config.DNS.ProxyServerNameserverPolicy["cloudflare-ech.com"]; len(got) != 1 || got[0] != wantResolver {
		t.Fatalf("ECH query is not routed through its source resolver: %#v", config.DNS.ProxyServerNameserverPolicy)
	}
	if _, leaked := config.Proxies[0]["__ech-resolver"]; leaked {
		t.Fatal("internal resolver metadata leaked into the outbound config")
	}
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
	l, err := m.Acquire(LeaseRequest{Targets: entries[:1], Kind: "exit"})
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
	if _, err = m.Acquire(LeaseRequest{Targets: entries[:1], Kind: "exit"}); err == nil {
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
	l, err = m.Acquire(LeaseRequest{Targets: entries[:1], Kind: "exit"})
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

func TestRealManagerAcceptsXHTTPShareLinkOptions(t *testing.T) {
	m := realManager(t)
	ctx := context.Background()
	extra := `{"scMaxConcurrentPosts":32,"scMaxEachPostBytes":1000000,"scMinPostsIntervalMs":30,"sessionIDKey":"session","sessionIDPlacement":"query","xmux":{"maxConcurrency":"16-32","maxConnections":0,"hMaxRequestTimes":"600-900","hMaxReusableSecs":"1800-3000","hKeepAlivePeriod":30}}`
	node := domain.Node{Name: "xhttp", Type: "vless", RawLink: "vless://custom-user@127.0.0.1:443?security=tls&type=xhttp&extra=" + url.QueryEscape(extra)}
	e := RegistryEntry{Identity: "xhttp", Incarnation: "inc-xhttp", Node: node, Digest: ConfigDigest(node)}
	if err := m.Sync(ctx, []RegistryEntry{e}); err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(LeaseRequest{Targets: []RegistryEntry{e}, Kind: "exit"})
	if err != nil {
		t.Fatalf("Mihomo rejected converted XHTTP options: %v", err)
	}
	m.Release(lease.ID)
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
	l, err := m.Acquire(LeaseRequest{Targets: entries, Kind: "exit"})
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

func TestRealManagerAcceptsQilanECHResolverConfig(t *testing.T) {
	m := realManager(t)
	proxy, err := domain.ShareLinkProxy("vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&ech=cloudflare-ech.com+https://dns.alidns.com/dns-query", "ech-probe")
	if err != nil {
		t.Fatal(err)
	}
	node := domain.Node{Type: "vless", Server: "example.com", Port: "443", RawConfig: proxy}
	entry := RegistryEntry{Identity: "qilan-ech", Incarnation: "qilan-ech-test", Node: node, Digest: ConfigDigest(node)}
	if err := m.Sync(context.Background(), []RegistryEntry{entry}); err != nil {
		t.Fatalf("Mihomo rejected the generated Qilan ECH DNS config: %v", err)
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
	if _, err := m.Acquire(LeaseRequest{Targets: []RegistryEntry{entry("a")}, Kind: "exit"}); err != nil {
		t.Fatal(err)
	}
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
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "proxy", "extra": map[string]any{"unused": map[string]any{"alive": false, "history": []map[string]any{{"time": time.Now(), "delay": 0}}}}})
	}))
	t.Cleanup(server.Close)
	return NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890", Secret: "controller"}), failed, reloads
}
