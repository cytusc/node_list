package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

func TestRejectedExitPersistsReasonAndPreservesHistoricalIP(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "")
	if err := f.repo.Schedule(ctx, KindExit, f.id, Payload{Identity: f.id}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	j := f.claim(t, KindExit)
	target, err := f.repo.Target(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	j.Payload.Incarnation = target.Incarnation
	if err = f.runner.rejectedExit(ctx, j, target, &transport.ConfigRejection{Reason: "missing-credentials"}); err != nil {
		t.Fatal(err)
	}
	var ip string
	var raw []byte
	if err = f.repo.Pool.QueryRow(ctx, `SELECT ip,payload FROM node_explorer_exits WHERE identity=$1`, f.id).Scan(&ip, &raw); err != nil {
		t.Fatal(err)
	}
	var v domain.ExitState
	json.Unmarshal(raw, &v)
	if ip != "8.8.4.4" || v.IP != ip || v.Verified || !strings.Contains(v.Error, "missing-credentials") {
		t.Fatalf("observation: %s %+v", ip, v)
	}
	var state, message string
	var due time.Time
	if err = f.repo.Pool.QueryRow(ctx, `SELECT state,error,due_at FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state, &message, &due); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || !strings.HasPrefix(message, strings.TrimSuffix(configRejectedPattern(), "%")) || due.Before(time.Now().Add(5*time.Hour)) {
		t.Fatalf("retry: %s %s %s", state, message, due)
	}
	if err = f.runner.rejectedExit(ctx, j, target, &transport.ConfigRejection{Reason: "kernel-rejected"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale lease wrote observation: %v", err)
	}
}

func TestFailedRiskRefreshPreservesScoreAndRecordsFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := 50.0
	initial := &domain.IPPureResult{IP: "8.8.4.4", Status: "success", Score: 3, HumanPercent: &p, BotPercent: &p, TrafficRatioStatus: "available", RiskQueryStatus: "success", RiskQueryCheckedAt: "2026-10-01T00:00:00Z"}
	merge := func(v *domain.IPPureResult) {
		t.Helper()
		if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error { return mergeAttributes(ctx, tx, "8.8.4.4", nil, v) }); err != nil {
			t.Fatal(err)
		}
	}
	merge(initial)
	var before, answered string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->>'checkedAt', payload->>'riskQueryCheckedAt' FROM node_explorer_ip_attributes WHERE ip='8.8.4.4'`).Scan(&before, &answered); err != nil {
		t.Fatal(err)
	}
	merge(&domain.IPPureResult{Status: "error", Error: "风险查询失败: http-403", FailureStage: "ippure-basic", RiskQueryStatus: "query-failed", RiskQueryError: "http-403"})
	var raw []byte
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip='8.8.4.4'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got domain.IPPureResult
	json.Unmarshal(raw, &got)
	// The verdict survives the refusal untouched: same status, same score, same
	// check time, and the ratio keeps the moment it was actually measured.
	if got.Status != "success" || got.Score != 3 || got.CheckedAt != before || got.HumanPercent == nil {
		t.Fatalf("refresh overwrote the verdict: %+v", got)
	}
	if got.RiskQueryCheckedAt != answered {
		t.Fatalf("failed attempt made the stored verdict look fresh: %q, want %q", got.RiskQueryCheckedAt, answered)
	}
	// The attempt itself is still visible, so an operator can see the source is
	// failing while the last answer keeps its own timestamp.
	if got.RiskQueryStatus != "query-failed" || got.RiskQueryError != "http-403" || got.RiskQueryAttemptAt == "" {
		t.Fatalf("failed attempt was not recorded: %+v", got)
	}
	if got.RiskQueryAttemptAt == answered {
		t.Fatalf("attempt marker did not move: %+v", got)
	}
}

// A refused lookup is a fact about the attempt. It must not evict the evidence
// that made an address residential, or the exit list silently drops the node.
func TestFailedRefreshKeepsResidentialEvidence(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "")
	var before string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->'classification'->>'state' FROM node_explorer_ip_attributes WHERE ip='8.8.4.4'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != "residential" {
		t.Fatalf("fixture is not residential: %s", before)
	}
	failed := ipcheck.Evidence{Provider: "ipinfo", IP: "8.8.4.4", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Outcome: "unknown", QueryStatus: "request-failed"}
	if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		return mergeAttributes(ctx, tx, "8.8.4.4", []ipcheck.Evidence{failed}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip='8.8.4.4'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got domain.IPPureResult
	json.Unmarshal(raw, &got)
	if got.Classification == nil || got.Classification.State != "residential" {
		t.Fatalf("failed refresh dropped the residential verdict: %+v", got.Classification)
	}
	if strings.Contains(strings.Join(got.Attributes, ","), "类型未知") || !strings.Contains(strings.Join(got.Attributes, ","), "住宅IP") {
		t.Fatalf("attributes lost the residential label: %v", got.Attributes)
	}
	answers := 0
	for _, e := range got.Classification.Evidence {
		if e.Provider == "ipinfo" && e.QueryStatus == "success" {
			answers++
		}
	}
	if answers == 0 {
		t.Fatalf("successful evidence was replaced by the failure: %+v", got.Classification.Evidence)
	}
}
