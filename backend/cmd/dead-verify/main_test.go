package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func TestDryRunReportsVerdictsWithoutDatabaseWrites(t *testing.T) {
	for _, alive := range []bool{false, true} {
		t.Run(map[bool]string{false: "dead", true: "alive"}[alive], func(t *testing.T) {
			controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPut && r.URL.Path == "/configs":
					w.WriteHeader(http.StatusNoContent)
				case r.URL.Path == "/proxies/DIRECT/delay" || alive:
					_, _ = w.Write([]byte(`{"delay":10}`))
				default:
					w.WriteHeader(http.StatusGatewayTimeout)
				}
			}))
			defer controller.Close()
			wave := []service.VerifiedCandidate{{Identity: "fixture", Node: domain.Node{Type: "http", Server: "192.0.2.1", Port: "443"}}}
			// A nil repository makes any attempted observation or deletion fail loudly.
			stats, err := verifyWave(context.Background(), nil, transport.MihomoConfig{Controller: controller.URL}, wave, options{dryRun: true, workers: 1, deleters: 1, timeout: time.Second})
			if err != nil || stats.deleted != 0 || stats.vetoed != 0 || (alive && stats.alive != 1) || (!alive && stats.dead != 1) {
				t.Fatalf("stats=%+v err=%v", stats, err)
			}
		})
	}
}

func TestDryRunDoesNotRecordRejectedConfigurations(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"delay":10}`)) }))
	defer controller.Close()
	wave := []service.VerifiedCandidate{{Identity: "unparseable", Node: domain.Node{Type: "http"}}}
	if _, err := verifyWave(context.Background(), nil, transport.MihomoConfig{Controller: controller.URL}, wave, options{dryRun: true, workers: 1, deleters: 1, timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
}
