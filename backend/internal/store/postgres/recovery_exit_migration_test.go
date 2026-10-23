package postgres

import (
	"context"
	"testing"
)

func TestRecoveryExitIndexUpgradesExistingSchema(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	// Simulate an existing v8 database, whose startup would otherwise skip DDL.
	if _, err := a.pool.Exec(ctx, `DROP INDEX node_explorer_work_recovery_exit;
	 DELETE FROM node_explorer_service_migrations WHERE version IN (9,10);
 INSERT INTO node_explorer_service_migrations(version) VALUES(8) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.initSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var index, applied bool
	if err := a.pool.QueryRow(ctx, `SELECT to_regclass('node_explorer_work_recovery_exit') IS NOT NULL,
	 EXISTS(SELECT 1 FROM node_explorer_service_migrations WHERE version=10)`).Scan(&index, &applied); err != nil {
		t.Fatal(err)
	}
	if !index || !applied {
		t.Fatalf("upgrade skipped recovery index: index=%v applied=%v", index, applied)
	}
}
