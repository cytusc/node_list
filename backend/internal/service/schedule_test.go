package service

import (
	"context"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/jackc/pgx/v5"
)

func TestRiskRefreshIntervalDistinguishesFailureFromNoData(t *testing.T) {
	p, clean, dirty := 50.0, 10.0, 30.0
	for _, tc := range []struct {
		v    domain.IPPureResult
		base time.Duration
		want time.Duration
	}{
		{domain.IPPureResult{TrafficRatioStatus: "query-failed"}, 0, 30 * time.Minute},
		{domain.IPPureResult{TrafficRatioStatus: "no-data"}, 0, 6 * time.Hour},
		// A clean ASN keeps the configured steady-state window.
		{domain.IPPureResult{HumanPercent: &p, BotPercent: &clean, TrafficRatioStatus: "available"}, 24 * time.Hour, 24 * time.Hour},
		{domain.IPPureResult{HumanPercent: &p, BotPercent: &clean, TrafficRatioStatus: "available"}, 6 * time.Hour, 6 * time.Hour},
		// A dirty ASN re-checks at least four times faster, but never slower
		// than the configured base: one IP swung from 2% to 70% in two hours.
		{domain.IPPureResult{HumanPercent: &p, BotPercent: &dirty, TrafficRatioStatus: "available"}, 24 * time.Hour, 6 * time.Hour},
		{domain.IPPureResult{HumanPercent: &p, BotPercent: &dirty, TrafficRatioStatus: "available"}, 6 * time.Hour, 2 * time.Hour},
		{domain.IPPureResult{HumanPercent: &p, BotPercent: &dirty, TrafficRatioStatus: "available"}, 0, 6 * time.Hour},
	} {
		if got := riskRefreshInterval(tc.v, tc.base); got != tc.want {
			t.Fatalf("%+v base=%s: %s != %s", tc.v, tc.base, got, tc.want)
		}
	}
}

func TestMissingRatioHonorsLatestFailedRiskQueryBackoff(t *testing.T) {
	now := time.Now()
	attr := domain.IPPureResult{Status: "success", TrafficRatioStatus: "query-failed",
		TrafficRatioCheckedAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		RiskQueryStatus:       "query-failed", RiskQueryCheckedAt: now.Format(time.RFC3339Nano)}
	if missingTrafficRatio(attr, now) {
		t.Fatal("old ratio timestamp bypassed the latest failed query's backoff")
	}
	if !missingTrafficRatio(attr, now.Add(31*time.Minute)) {
		t.Fatal("failed query was not eligible after its backoff")
	}
}

func TestSchedulerPrioritizesReachableNodesWithoutExit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "")
	f.exec(t, `DELETE FROM node_explorer_exits`)
	f.runner.Config.Role = "exit-provision"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var priority int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT priority FROM node_explorer_work WHERE kind='exit' AND key=$1`, f.id).Scan(&priority); err != nil || priority != 1 {
		t.Fatalf("missing exit was not prioritized: %d, %v", priority, err)
	}
	// A previously queued normal task must also be promoted without waiting
	// for its scheduled time to change.
	f.exec(t, `UPDATE node_explorer_work SET priority=0 WHERE kind='exit'`)
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT priority FROM node_explorer_work WHERE kind='exit' AND key=$1`, f.id).Scan(&priority); err != nil || priority != 1 {
		t.Fatalf("queued exit was not promoted: %d, %v", priority, err)
	}
}

func TestBatchScheduleDeduplicatesSharedRiskAndKeepsPriority(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	due := time.Now().Add(time.Hour)
	work := []scheduledWork{
		{Kind: KindRisk, Key: "8.8.4.4", Payload: Payload{IP: "8.8.4.4", Identity: "first"}, Due: due, Mode: "risk"},
		{Kind: KindRisk, Key: "8.8.4.4", Payload: Payload{IP: "8.8.4.4", Identity: "second"}, Priority: 1, Due: due.Add(-time.Minute), Mode: "risk"},
	}
	apply := func() {
		t.Helper()
		if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error { return scheduleMany(ctx, tx, work) }); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	// An ordinary caller can advance the deadline but cannot demote the shared
	// task or replace its existing payload while it is queued.
	work = []scheduledWork{{Kind: KindRisk, Key: "8.8.4.4", Payload: Payload{Identity: "replacement"}, Due: due.Add(-time.Hour), Mode: "risk"}}
	apply()
	var priority int
	var generation int64
	var identity string
	var actualDue time.Time
	if err := f.repo.Pool.QueryRow(ctx, `SELECT priority,generation,payload->>'identity',due_at FROM node_explorer_work WHERE kind='risk'`).Scan(&priority, &generation, &identity, &actualDue); err != nil {
		t.Fatal(err)
	}
	if priority != 1 || generation != 2 || identity != "first" || actualDue.Sub(work[0].Due).Abs() > time.Microsecond {
		t.Fatalf("shared risk task changed incorrectly: priority=%d generation=%d identity=%s due=%v", priority, generation, identity, actualDue)
	}
}

// A source that only cooled down has not disagreed: it never answered, so a
// thin consensus must not defer the next attempt by a whole day.
func TestAttributesRetryDelayWaitsForUnansweredSources(t *testing.T) {
	ok := func(provider, usage string) ipcheck.Evidence {
		return ipcheck.Evidence{Provider: provider, QueryStatus: "success", UsageType: usage}
	}
	cool := ipcheck.Evidence{Provider: "ipinfo", QueryStatus: "quota-cooldown"}
	for _, tc := range []struct {
		name     string
		evidence []ipcheck.Evidence
		priority int
		want     time.Duration
	}{
		{"no typed source", []ipcheck.Evidence{cool, cool}, 0, 30 * time.Minute},
		{"no typed source on priority lane", []ipcheck.Evidence{cool}, 5, 5 * time.Minute},
		{"one typed source plus cooldown", []ipcheck.Evidence{ok("ip-api", "数据"), cool}, 0, 6 * time.Hour},
		{"two typed sources settle", []ipcheck.Evidence{ok("ip-api", "x"), ok("ippure", "y"), cool}, 0, 24 * time.Hour},
		{"single answer without cooldown keeps the day window", []ipcheck.Evidence{ok("ip-api", "x")}, 0, 24 * time.Hour},
		// Every source answered, none named a type: the answer is stable, so a
		// five-minute retry would only spend quota the queue needs.
		{"answered but untyped is not retried soon", []ipcheck.Evidence{ok("ip-api", ""), ok("ippure", "")}, 0, 6 * time.Hour},
		{"answered but untyped on priority lane", []ipcheck.Evidence{ok("ip-api", "")}, 5, 6 * time.Hour},
		// A request that failed did not answer: retry while the quota recovers.
		{"failed request retries soon", []ipcheck.Evidence{{Provider: "ip-api", QueryStatus: "request-failed"}}, 0, 30 * time.Minute},
		{"failed request on priority lane", []ipcheck.Evidence{{Provider: "ip-api", QueryStatus: "http-error"}}, 5, 5 * time.Minute},
	} {
		if got := attributesRetryDelay(tc.evidence, tc.priority, 5*time.Minute); got != tc.want {
			t.Fatalf("%s: %s != %s", tc.name, got, tc.want)
		}
	}
}
