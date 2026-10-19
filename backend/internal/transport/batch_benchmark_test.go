package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"gopkg.in/yaml.v3"
)

func TestBatchRejectedNodePreservesListenerIdentity(t *testing.T) {
	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload string `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		var config mihomoConfig
		if err := yaml.Unmarshal([]byte(body.Payload), &config); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		loads.Add(1)
		for j, p := range config.Proxies {
			if p["server"] == "invalid.example" {
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"message":"proxy %d: secret error detail"}`, j)
				return
			}
			if config.Listeners[j].Proxy != p["name"] || config.Listeners[j].Port != 7891+j {
				t.Error("listener was assigned to wrong outbound")
			}
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	e := NewMihomoEngine([]MihomoConfig{{Controller: server.URL, ProxyURL: "http://127.0.0.1:7890"}}, time.Second)
	var nodes []domain.Node
	for _, host := range []string{"first.example", "invalid.example", "third.example"} {
		nodes = append(nodes, domain.Node{Type: "ss", RawConfig: domain.ProxyConfig{"type": "ss", "server": host, "port": 443}})
	}
	paths, release, err := e.Reserve(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if paths[1].Error == nil || paths[0].Client == nil || paths[2].Client == nil || loads.Load() != 2 {
		t.Fatal("bad node disrupted batch")
	}
	for _, tc := range []struct {
		index int
		port  string
	}{{0, "7891"}, {2, "7892"}} {
		req, _ := http.NewRequest("GET", "https://github.com/", nil)
		u, err := paths[tc.index].Client.Transport.(*http.Transport).Proxy(req)
		if err != nil || u.Port() != tc.port {
			t.Fatal("compaction changed node identity")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err = e.Reserve(ctx, nodes[:1]); err == nil {
		t.Fatal("wave reloaded while still leased")
	}
	if loads.Load() != 2 {
		t.Fatal("occupied instance was reconfigured")
	}
}

func TestBatch128AndReferencesHaveDistinctListeners(t *testing.T) {
	var seen atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload string `json:"payload"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		var cfg mihomoConfig
		yaml.Unmarshal([]byte(body.Payload), &cfg)
		seen.Store(int32(len(cfg.Listeners)))
		ports := map[int]bool{}
		for _, l := range cfg.Listeners {
			if ports[l.Port] || l.Port == cfg.MixedPort {
				t.Error("listener collision")
			}
			ports[l.Port] = true
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	var configs []domain.ProxyConfig
	for j := range 130 {
		configs = append(configs, domain.ProxyConfig{"type": "ss", "server": fmt.Sprintf("node-%d.example", j), "port": 443})
	}
	urls, err := ConfigureProxyBatch(context.Background(), MihomoConfig{Controller: server.URL, ProxyURL: "http://127.0.0.1:7890"}, configs)
	if err != nil || len(urls) != 130 || seen.Load() != 130 || urls[129] != "http://127.0.0.1:8020" {
		t.Fatalf("bad mapping: %v", err)
	}
}
