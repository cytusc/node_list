package postgres

import (
	"context"
	"testing"
)

func TestRecoveryStatisticsMigrationImportsOnlyRetainedOutcomes(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	_, err := a.pool.Exec(ctx, `
	 DROP TABLE node_explorer_recovery_events,node_explorer_recovery_totals;
	 DELETE FROM node_explorer_service_migrations WHERE version=7;
	 INSERT INTO node_explorer_service_migrations(version) VALUES(6) ON CONFLICT DO NOTHING;
	 INSERT INTO node_explorer_work(kind,key,state,due_at,payload) VALUES
	 ('normal-recovery','mixed','queued',now(),jsonb_build_object(
	 'recoveryResult','config-rejected','recoveryAttempts',100,'recoveryHistory',jsonb_build_array(
	 jsonb_build_object('result','probe-failed','checkedAt',now()-interval '3 hours'),
	 jsonb_build_object('result','config-rejected','checkedAt',now()-interval '10 minutes')))),
	 ('recovery','legacy','done',now(),jsonb_build_object('recoveryResult','eligible','checkedAt',now())),
	 ('recovery','cancelled','done',now(),jsonb_build_object('recoveryResult','cancelled-historical-backlog','checkedAt',now())),
	 ('recovery','empty-history','done',now(),jsonb_build_object('recoveryResult','skipped-datacenter-asn','checkedAt',now(),'recoveryHistory','[]'::jsonb))`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.initSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var total, recent, outcomes int
	if err := a.pool.QueryRow(ctx, `SELECT sum(completed),count(*) FROM node_explorer_recovery_totals`).Scan(&total, &outcomes); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_recovery_events`).Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if total != 4 || outcomes != 4 || recent != 3 {
		t.Fatalf("migration invented, dropped or duplicated outcomes: total=%d outcomes=%d recent=%d", total, outcomes, recent)
	}
}
