package service

import (
	"context"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
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

func TestSchedulerSkipsRetiredNodesAndExistingRecoveryBeforeReadingFacts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "scheduler")
	p := f.retirement(t)
	// The sweeper round seeds the first recovery marker for an eligible
	// automatic retirement; inject it directly the way step 6 Commit does.
	if err := f.repo.Schedule(ctx, KindRecovery, p.Identity+":"+p.RetiredAt, p, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='recovery'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing initial recovery: %d %v", count, err)
	}
	// These facts cannot decode into an IP result. None of the schedulers
	// should read them for a retired node with an existing recovery cycle.
	f.exec(t, `UPDATE node_explorer_ip_attributes SET payload='[]'::jsonb`)
	for _, role := range []string{"recovery", "exit-provision"} {
		f.runner.Config.Role = role
		if err := f.runner.schedule(ctx); err != nil {
			t.Fatalf("%s read irrelevant facts: %v", role, err)
		}
	}
	f.exec(t, `DELETE FROM node_explorer_work`)
	f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual'`)
	f.runner.Config.Role = "recovery"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatalf("manual retirement read irrelevant facts: %v", err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("manual retirement created work: %d %v", count, err)
	}
}

func TestBatchSchedulePreservesUnchangedAndRunningWork(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	due := time.Now().Add(time.Minute)
	work := []scheduledWork{
		{Kind: KindLive, Key: f.id, Payload: Payload{Identity: f.id}, Due: due},
		{Kind: KindRecovery, Key: "cycle", Payload: Payload{Identity: f.id}, Due: due, Mode: "once"},
	}
	apply := func() {
		t.Helper()
		if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error { return scheduleMany(ctx, tx, work) }); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	var before, after string
	readVersion := func() string {
		t.Helper()
		var version string
		if err := f.repo.Pool.QueryRow(ctx, `SELECT string_agg(kind||':'||generation::text||':'||xmin::text,',' ORDER BY kind) FROM node_explorer_work`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	before = readVersion()
	apply()
	after = readVersion()
	if before != after {
		t.Fatalf("unchanged batch wrote task rows: %s -> %s", before, after)
	}
	f.exec(t, `UPDATE node_explorer_work SET state=CASE WHEN kind='recovery' THEN 'done' ELSE 'running' END`)
	before = readVersion()
	for i := range work {
		work[i].Due = due.Add(-time.Hour)
		work[i].Priority = 1
	}
	apply()
	if after = readVersion(); before != after {
		t.Fatalf("batch reopened a recovery cycle or replaced running work: %s -> %s", before, after)
	}
}

func TestBatchScheduleRetriesRejectedNodesAfterParserVersionChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	due := time.Now().Add(-time.Second)
	work := []scheduledWork{{Kind: KindFast, Key: f.id, Payload: Payload{Identity: f.id}, Due: due}}
	f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,state,due_at,attempts,error)
	 VALUES($1,$2,jsonb_build_object('identity',$3::text),'queued',now()+interval '4 hours',35,'node_config_rejected: legacy parser error')`,
		KindFast, f.id, f.id)
	apply := func() {
		t.Helper()
		if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error { return scheduleMany(ctx, tx, work) }); err != nil {
			t.Fatal(err)
		}
	}
	apply()
	var state, message string
	var attempts int
	var generation int64
	var actualDue time.Time
	read := func() {
		t.Helper()
		if err := f.repo.Pool.QueryRow(ctx, `SELECT state,attempts,error,generation,due_at FROM node_explorer_work WHERE kind=$1 AND key=$2`, KindFast, f.id).
			Scan(&state, &attempts, &message, &generation, &actualDue); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if state != "queued" || attempts != 0 || message != "" || generation != 2 || actualDue.After(time.Now()) {
		t.Fatalf("stale parser rejection was not retried immediately: state=%s attempts=%d error=%q generation=%d due=%s", state, attempts, message, generation, actualDue)
	}

	// A rejection from the current parser still backs off, so a truly malformed
	// node cannot cause the scheduler to retry it on every pass.
	f.exec(t, `UPDATE node_explorer_work SET due_at=now()+interval '4 hours',attempts=1,error=$3 WHERE kind=$1 AND key=$2`, KindFast, f.id, configRejectedMessage())
	work[0].Due = time.Now().Add(-time.Minute)
	apply()
	read()
	if attempts != 1 || message != configRejectedMessage() || generation != 2 || !actualDue.After(time.Now().Add(3*time.Hour)) {
		t.Fatalf("current parser rejection lost its backoff: attempts=%d error=%q generation=%d due=%s", attempts, message, generation, actualDue)
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
