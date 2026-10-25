package postgres

import (
	"context"
	"testing"
)

func TestMonitorSamplesUpgradeExistingSchema(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	// v1.5.37 committed schema v9 before the monitor sample table existed.
	// Two statements: a parameterised multi-statement Exec becomes a prepared
	// statement, which PostgreSQL refuses.
	if _, err := a.pool.Exec(ctx, `DROP TABLE node_explorer_monitor_samples`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `DELETE FROM node_explorer_service_migrations WHERE version>=$1`, schemaVersion); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.initSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var table, applied bool
	if err := a.pool.QueryRow(ctx, `SELECT to_regclass('node_explorer_monitor_samples') IS NOT NULL,
	 EXISTS(SELECT 1 FROM node_explorer_service_migrations WHERE version=$1)`, schemaVersion).Scan(&table, &applied); err != nil {
		t.Fatal(err)
	}
	if !table || !applied {
		t.Fatalf("upgrade skipped monitor sample table: table=%v applied=%v", table, applied)
	}
}
