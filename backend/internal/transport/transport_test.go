package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestMihomoPerInstanceCacheOrderAndLease(t *testing.T) {
	var counts [2]atomic.Int32
	instances := make([]MihomoConfig, 2)
	for i := range instances {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { counts[i].Add(1); w.WriteHeader(204) }))
		defer server.Close()
		instances[i] = MihomoConfig{Controller: server.URL, ProxyURL: "http://127.0.0.1:18000"}
	}
	engine := NewMihomoEngine(instances, time.Second)
	nodes := []domain.Node{{RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.1", "port": 443, "password": "a"}}, {RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.2", "port": 443, "password": "b"}}}
	for range 2 {
		_, release, err := engine.Reserve(context.Background(), nodes)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if counts[0].Load() != 1 || counts[1].Load() != 1 {
		t.Fatalf("per-instance reloads=%d,%d", counts[0].Load(), counts[1].Load())
	}
	_, release0, err := engine.Reserve(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	_, release1, err := engine.Reserve(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := engine.Reserve(ctx, nodes); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy instance lease was reused: %v", err)
	}
	release0()
	release0()
	release1() // release is idempotent
	reversed := []domain.Node{nodes[1], nodes[0]}
	for range 2 {
		_, release, err := engine.Reserve(context.Background(), reversed)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if counts[0].Load() != 2 || counts[1].Load() != 2 {
		t.Fatal("position-dependent listeners reused reordered config")
	}
	// Credential changes and nested fields must invalidate the cached definition.
	nodes[0].RawConfig["password"] = "changed"
	_, release, err := engine.Reserve(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	release()
	reloads, skipped := engine.Stats()
	if reloads != 5 || skipped != 2 {
		t.Fatalf("reloads=%d skipped=%d", reloads, skipped)
	}
}

func TestMihomoConcurrentReservations(t *testing.T) {
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	engine := NewMihomoEngine([]MihomoConfig{{Controller: server.URL, ProxyURL: "http://127.0.0.1:18000"}}, time.Second)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			nodes := []domain.Node{{RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.1", "port": 443, "name": i}}}
			_, release, err := engine.Reserve(context.Background(), nodes)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
		})
	}
	wg.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("overlapping reloads: %d", maximum.Load())
	}
}

func TestMihomoReloadFailureReleasesLease(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	engine := NewMihomoEngine([]MihomoConfig{{Controller: server.URL, ProxyURL: "http://127.0.0.1:18000"}}, time.Second)
	nodes := []domain.Node{{Type: "ss", Server: "192.0.2.1", Port: "443"}}
	_, release, err := engine.Reserve(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	release()
	fail.Store(true)
	nodes[0].Server = "192.0.2.2"
	if _, _, err := engine.Reserve(context.Background(), nodes); err == nil {
		t.Fatal("expected reload failure")
	}
	fail.Store(false)
	nodes[0].Server = "192.0.2.1"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err = engine.Reserve(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	release()
	reloads, _ := engine.Stats()
	if reloads != 2 {
		t.Fatal("failed reload left stale cached configuration")
	}
}

func TestNativeRoutingHonorsProxyTLSOptions(t *testing.T) {
	native := NewNativeEngine()
	fallback := NewMihomoEngine(nil, time.Second)
	router := NewRouter(native).WithFallback(fallback)
	for _, tc := range []struct {
		config domain.ProxyConfig
		want   Engine
	}{
		{domain.ProxyConfig{"type": "http", "server": "example.com", "port": 8080}, native},
		{domain.ProxyConfig{"type": "https", "server": "example.com", "port": 443}, native},
		{domain.ProxyConfig{"type": "http", "tls": true, "sni": "proxy.example.com"}, fallback},
		{domain.ProxyConfig{"type": "https", "skip-cert-verify": true}, fallback},
		{domain.ProxyConfig{"type": "socks5", "tls": true}, fallback},
	} {
		engine, ok := router.EngineFor(domain.Node{Type: tc.config["type"].(string), RawConfig: tc.config})
		if !ok || engine != tc.want {
			t.Fatalf("config %v chose %T", tc.config, engine)
		}
	}
	url, err := outboundURL(domain.ProxyConfig{"type": "https", "server": "example.com", "port": 443})
	if err != nil || url.Scheme != "https" {
		t.Fatalf("HTTPS endpoint: %v %v", url, err)
	}
}
