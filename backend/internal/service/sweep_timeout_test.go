package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func TestSweepRetainsSlowControllerObservation(t *testing.T) {
	// Both explicit failure and success arriving after the old 8s cutoff are evidence.
	for _, status := range []int{http.StatusOK, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/delay") {
					select {
					case <-time.After(8100 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(status)
					if status == http.StatusOK {
						_, _ = w.Write([]byte(`{"delay":8100}`))
					}
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"extra": map[string]any{transport.DelayTarget: map[string]any{"alive": status == http.StatusOK, "history": []map[string]any{{"time": time.Now(), "delay": 8100}}}}})
			}))
			defer server.Close()
			m := transport.NewManager(transport.MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
			node := domain.Node{Type: "http", Server: "192.0.2.1", Port: "443"}
			entries := []transport.RegistryEntry{{Identity: "slow", Incarnation: "one", Node: node, Digest: transport.ConfigDigest(node)}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := m.Sync(ctx, entries); err != nil {
				t.Fatal(err)
			}
			results := (&Repository{}).sweepAll(ctx, m, entries, 1)
			if len(results) != 1 || results[0].entry.Identity != "slow" || results[0].delay.Success != (status == http.StatusOK) {
				t.Fatalf("slow controller evidence lost: %+v", results)
			}
			if !m.Stats().Healthy {
				t.Fatal("slow probe poisoned kernel health")
			}
		})
	}
}
