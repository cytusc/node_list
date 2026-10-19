package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRecoveryStatisticsSurviveHistoryLimitAndWorkDeletion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := Payload{Identity: f.id}
	var last Job
	for i := 0; i < 20; i++ {
		if err := f.repo.Schedule(ctx, KindNormalRecovery, "statistics", p, 1, time.Now()); err != nil {
			t.Fatal(err)
		}
		j := f.claim(t, KindNormalRecovery)
		result := "probe-failed"
		if i%2 == 1 {
			result = "config-rejected"
		}
		complete := func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, j, result, "") }
		if err := f.repo.Finish(ctx, j, complete); err != nil {
			t.Fatal(err)
		}
		// Re-delivery of a finished lease must not increment either counter.
		if err := f.repo.Finish(ctx, j, complete); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("duplicate completion: %v", err)
		}
		var raw []byte
		if err := f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		last = j
	}
	if len(p.RecoveryHistory) != 16 || p.RecoveryAttempts != 20 {
		t.Fatalf("unexpected bounded work history: %+v", p)
	}
	var events, total int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_recovery_events WHERE work_id=$1`, last.ID).Scan(&events); err != nil || events != 20 {
		t.Fatalf("lost assessment events: %d %v", events, err)
	}
	f.exec(t, `DELETE FROM node_explorer_work WHERE id=$1`, last.ID)
	f.exec(t, `UPDATE node_explorer_recovery_events SET checked_at=checked_at-interval '3 hours'`)
	if err := f.repo.pruneCompletedWork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_recovery_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("rolling event retention: %d %v", events, err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT sum(completed) FROM node_explorer_recovery_totals`).Scan(&total); err != nil || total != 20 {
		t.Fatalf("lifetime statistics changed after pruning: %d %v", total, err)
	}
}
