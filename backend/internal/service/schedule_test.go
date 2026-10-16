package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSchedulerSkipsRetiredNodesAndExistingRecoveryBeforeReadingFacts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "scheduler")
	f.retirement(t)
	f.runner.Config.Role = "residential"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='recovery'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing initial recovery: %d %v", count, err)
	}
	// These facts cannot decode into an IP result. None of the schedulers
	// should read them for a retired node with an existing recovery cycle.
	f.exec(t, `UPDATE node_explorer_ip_attributes SET payload='[]'::jsonb`)
	for _, role := range []string{"residential", "connectivity", "ip"} {
		f.runner.Config.Role = role
		if err := f.runner.schedule(ctx); err != nil {
			t.Fatalf("%s read irrelevant facts: %v", role, err)
		}
	}
	f.exec(t, `DELETE FROM node_explorer_work`)
	f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual'`)
	f.runner.Config.Role = "residential"
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
