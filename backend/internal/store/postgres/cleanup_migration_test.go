package postgres

import (
	"context"
	"testing"
)

func TestCleanupRetirementMigrationIsScopedAndIdempotent(t *testing.T) {
	a := testAdapter(t)
	ctx := context.Background()
	// Simulate the deployed schema, including an older version-8 marker.
	_, err := a.pool.Exec(ctx, `DELETE FROM node_explorer_service_migrations WHERE version>=13;
 INSERT INTO node_explorer_service_migrations(version) VALUES(8) ON CONFLICT DO NOTHING;
 INSERT INTO node_explorer_work(kind,key,due_at,state,generation,lease_token,lease_until,payload) VALUES
 ('node-cleanup','old',now(),'running',3,'old-token',now()+interval '1 minute','{}'),
 ('connectivity','old-dependency',now(),'running',3,'old-token',now()+interval '1 minute','{"session":"cleanup:1:3"}'),
 ('lifecycle','old-cleanup',now(),'queued',3,NULL,NULL,'{"action":"cleanup"}'),
 ('recovery','keep',now(),'running',3,'keep-token',now()+interval '1 minute','{}'),
 ('connectivity','keep-dependency',now(),'queued',3,NULL,NULL,'{"session":"recovery:2:3"}'),
 ('lifecycle','keep-lifecycle',now(),'queued',3,NULL,NULL,'{"action":"retire"}');
 INSERT INTO node_explorer_probe_results VALUES
 ('cleanup:1:3','connectivity','old',now(),'{}'),('recovery:2:3','connectivity','keep',now(),'{}');`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = a.initSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var retired, retained, proofs int
	err = a.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM node_explorer_work WHERE key LIKE 'old%' AND state='done' AND generation=4 AND lease_token IS NULL AND lease_until IS NULL),
 (SELECT count(*) FROM node_explorer_work WHERE key LIKE 'keep%' AND state='done' AND generation=4),
 (SELECT count(*) FROM node_explorer_probe_results WHERE session='recovery:2:3')`).Scan(&retired, &retained, &proofs)
	if err != nil || retired != 3 || retained != 3 || proofs != 1 {
		t.Fatalf("retired=%d retained=%d proofs=%d err=%v", retired, retained, proofs, err)
	}
	var oldProofs int
	if err = a.pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_probe_results WHERE session LIKE 'cleanup:%'`).Scan(&oldProofs); err != nil || oldProofs != 0 {
		t.Fatalf("old proofs=%d err=%v", oldProofs, err)
	}
}
