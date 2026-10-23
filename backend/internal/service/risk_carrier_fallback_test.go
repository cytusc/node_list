package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A risk job with nothing to lease and no registry row to fall back on used to
// take the whole process down: the carrier loop never ran, so queryErr stayed
// nil, the success branch was entered and a nil verdict was dereferenced.
// v1.5.49 shipped that path and crash-looped the exit-attributes service, so
// the empty pool has to be an ordinary recorded failure.
func TestRiskWithoutCarrierFailsOverInsteadOfPanicking(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "risk")

	// carriers() only returns nodes with reachability evidence, so wiping the
	// connectivity table leaves the pool empty for real.
	f.exec(t, `DELETE FROM node_explorer_connectivity`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 4xx is not retried, which keeps the test fast while still exercising
		// the direct-call fallback.
		http.Error(w, "unavailable", http.StatusBadRequest)
	}))
	defer api.Close()
	f.runner.Config.APIOrigin = api.URL

	// An identity that was never synced into the registry: Target returns nil,
	// so the fallback carrier stays empty too.
	payload := Payload{IP: "8.8.4.4", Identity: "residential:203.0.113.9:443"}
	if err := f.repo.Schedule(ctx, KindRisk, payload.IP, payload, 1, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	j := f.claim(t, KindRisk)

	if err := f.runner.risk(ctx, j, false); err != nil {
		t.Fatalf("risk()=%v; a missing carrier is a recorded failure, not a job error", err)
	}

	var status, message string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->>'riskQueryStatus', COALESCE(payload->>'riskQueryError','') FROM node_explorer_ip_attributes WHERE ip=$1`, payload.IP).Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "query-failed" || message == "" {
		t.Fatalf("risk status=%q error=%q", status, message)
	}
}

// A mirror that answers 200 {"ok":false} has lost its own upstream. Re-asking
// from a residential address five minutes later cannot fix that, and the old
// residential retry window made every refusal cost a slot in the fastest lane.
func TestRiskUpstreamRefusalBacksOffOnResidentialLane(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "risk")
	f.exec(t, `DELETE FROM node_explorer_connectivity`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer api.Close()
	f.runner.Config.APIOrigin = api.URL
	f.runner.Config.ResidentialInterval = 5 * time.Minute

	payload := Payload{IP: "8.8.4.4", Identity: "residential:203.0.113.9:443"}
	if err := f.repo.Schedule(ctx, KindRisk, payload.IP, payload, 1, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	j := f.claim(t, KindRisk)
	if err := f.runner.risk(ctx, j, true); err != nil {
		t.Fatalf("risk()=%v", err)
	}

	var status, code string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->>'riskQueryStatus', COALESCE(payload->>'riskQueryError','') FROM node_explorer_ip_attributes WHERE ip=$1`, payload.IP).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "query-failed" || code != "upstream-refused" {
		t.Fatalf("status=%q code=%q", status, code)
	}
	var wait time.Duration
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at-now() FROM node_explorer_work WHERE kind=$1 AND key=$2`, KindRisk, payload.IP).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait < 119*time.Minute {
		t.Fatalf("refused mirror retried in %v; the lane cannot retire work faster than an outage re-queues it", wait)
	}
}

// A timeout may well be the carrier's fault, so it keeps the short retry that
// lets a different node succeed.
func TestRiskCarrierFailureKeepsShortRetry(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "risk")
	f.exec(t, `DELETE FROM node_explorer_connectivity`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusTooManyRequests)
	}))
	defer api.Close()
	f.runner.Config.APIOrigin = api.URL
	f.runner.Config.ResidentialInterval = 5 * time.Minute

	payload := Payload{IP: "8.8.4.4", Identity: "residential:203.0.113.9:443"}
	if err := f.repo.Schedule(ctx, KindRisk, payload.IP, payload, 1, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	j := f.claim(t, KindRisk)
	if err := f.runner.risk(ctx, j, true); err != nil {
		t.Fatalf("risk()=%v", err)
	}
	var wait time.Duration
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at-now() FROM node_explorer_work WHERE kind=$1 AND key=$2`, KindRisk, payload.IP).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if wait > 6*time.Minute {
		t.Fatalf("rate limit retried in %v; a different carrier may still succeed", wait)
	}
}
