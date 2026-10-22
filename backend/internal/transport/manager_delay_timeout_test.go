package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagerDelayBoundsValidationWithoutPoisoningHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/delay") {
			_, _ = w.Write([]byte(`{"delay":1}`))
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	m := NewManager(MihomoConfig{Controller: server.URL, ProxyURL: "http://localhost:7890"})
	targets := []RegistryEntry{entry("a")}
	if err := m.Sync(context.Background(), targets); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := m.Delay(context.Background(), LeaseRequest{Targets: targets})
	if !errors.Is(err, context.DeadlineExceeded) || !result.CheckedAt.IsZero() {
		t.Fatalf("stalled validation became evidence: %+v %v", result, err)
	}
	if elapsed := time.Since(started); elapsed < delayValidationTimeout || elapsed > delayValidationTimeout+2*time.Second {
		t.Fatalf("validation budget not enforced: %s", elapsed)
	}
	if !m.Stats().Healthy {
		t.Fatal("validation timeout poisoned kernel health")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.leases) != 0 || m.fast != 0 || m.fastNormal != 0 {
		t.Fatal("timeout leaked capacity")
	}
}
