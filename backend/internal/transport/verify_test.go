package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestLoadVerificationProxiesNamesEveryUsableNode(t *testing.T) {
	var payload string
	var authorized bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized = r.Header.Get("Authorization") == "Bearer s3cret"
		if r.Method != http.MethodPut || r.URL.Path != "/configs" || r.URL.Query().Get("force") != "true" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		var body struct {
			Payload string `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("payload: %v", err)
		}
		payload = body.Payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	instance := MihomoConfig{Controller: server.URL, Secret: "s3cret"}
	names, picked, err := LoadVerificationProxies(context.Background(), instance, []domain.Node{
		{Type: "http", Server: "192.0.2.1", Port: "8080"},
		{Type: "vless", RawConfig: domain.ProxyConfig{"type": "vless", "server": "198.51.100.7", "port": 443, "uuid": "x"}},
		{Type: "http"}, // no endpoint: skipped, and the mapping must say so
	})
	if err != nil {
		t.Fatal(err)
	}
	if !authorized {
		t.Fatal("controller call carried no secret")
	}
	if len(names) != 2 || picked[0] != 0 || picked[1] != 1 {
		t.Fatalf("names=%v picked=%v", names, picked)
	}
	for _, want := range []string{"name: verify-0", "name: verify-1", "server: 192.0.2.1", "server: 198.51.100.7", "MATCH,DIRECT"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("payload missing %q:\n%s", want, payload)
		}
	}
	if strings.Contains(payload, "192.0.2.1") && strings.Count(payload, "verify-") < 2 {
		t.Fatal("proxy names not applied")
	}
}

func TestProxyDelayReadsOnlyTheAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxies/verify-0/delay" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("url") != DelayTarget {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("timeout") == "1" {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		if r.URL.Query().Get("timeout") != "5000" {
			t.Errorf("unexpected timeout %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"delay":42}`))
	}))
	defer server.Close()
	instance := MihomoConfig{Controller: server.URL}
	if alive, err := ProxyDelay(context.Background(), instance, "verify-0", 5*time.Second); err != nil || !alive {
		t.Fatalf("answered handshake: alive=%v err=%v", alive, err)
	}
	if alive, err := ProxyDelay(context.Background(), instance, "verify-0", time.Millisecond); err != nil || alive {
		t.Fatalf("timeout must be a verdict, not an error: alive=%v err=%v", alive, err)
	}
}

func TestProxyCountConfirmsWaveLanded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxies" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"proxies":{"DIRECT":{},"REJECT":{},"verify-0":{}}}`))
	}))
	defer server.Close()
	n, err := ProxyCount(context.Background(), MihomoConfig{Controller: server.URL})
	if err != nil || n != 3 {
		t.Fatalf("count=%d err=%v", n, err)
	}
}

func TestLoadVerificationProxiesDropsRejectedEntry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Payload string `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// The second attempt must no longer carry the rejected endpoint.
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`proxy 1: unsupported field`))
			return
		}
		if strings.Contains(body.Payload, "198.51.100.7") {
			t.Errorf("rejected proxy survived:\n%s", body.Payload)
		}
		if strings.Count(body.Payload, "name: verify-") != 1 {
			t.Errorf("expect exactly one surviving proxy:\n%s", body.Payload)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	names, picked, err := LoadVerificationProxies(context.Background(), MihomoConfig{Controller: server.URL}, []domain.Node{
		{Type: "http", Server: "192.0.2.1", Port: "8080"},
		{Type: "vless", RawConfig: domain.ProxyConfig{"type": "vless", "server": "198.51.100.7", "port": 443, "uuid": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(names) != 1 || picked[0] != 0 {
		t.Fatalf("calls=%d names=%v picked=%v", calls, names, picked)
	}
}

func TestProxyDelayProbesDirectEgress(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		if r.URL.Path != "/proxies/DIRECT/delay" {
			t.Errorf("direct probe hit %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"delay":8}`))
	}))
	defer server.Close()
	ok, err := ProxyDelay(context.Background(), MihomoConfig{Controller: server.URL}, DirectProxy, 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("direct egress probe: ok=%v err=%v path=%s", ok, err, seen)
	}
}
