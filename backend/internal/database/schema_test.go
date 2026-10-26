package database_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cytusc/node_list/backend/internal/database"
	"github.com/cytusc/node_list/backend/internal/testutil"
)

func TestRetirementPreservesFactsAndIsRepeatable(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
 INSERT INTO node_explorer_snapshots(id,fingerprint,imported_at,node_count,payload) VALUES(1,'fixture','now',1,'{}');
 INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index,available) VALUES('kept','{"rawLink":"fixture","name":"kept"}',1,0,true);
 INSERT INTO node_explorer_snapshot_members VALUES(1,0,'kept');
 INSERT INTO node_explorer_connectivity VALUES('kept',now(),'{"reachable":true}');
 INSERT INTO node_explorer_exits VALUES('kept','192.0.2.1',7,now(),'{"ip":"192.0.2.1"}');
 INSERT INTO node_explorer_ip_attributes VALUES('192.0.2.1',now(),'{"ip":"192.0.2.1","status":"success"}');
 ALTER TABLE node_explorer_node_registry ADD COLUMN historical_residential boolean DEFAULT true, ADD COLUMN connection_key text DEFAULT 'legacy', ADD COLUMN source_at timestamptz DEFAULT now();
 CREATE TABLE node_explorer_retired_nodes(identity text);
 CREATE TABLE node_explorer_work(id int);
 INSERT INTO node_explorer_work VALUES(1);
 DROP VIEW node_explorer_live_nodes;
 DROP VIEW node_explorer_live_facts;
 CREATE VIEW node_explorer_live_facts AS SELECT * FROM node_explorer_retired_nodes;
 CREATE VIEW node_explorer_live_nodes AS SELECT * FROM node_explorer_live_facts;
 `)
	if err != nil {
		t.Fatal(err)
	}
	facts := func() string {
		t.Helper()
		var out string
		err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
   (SELECT jsonb_agg(to_jsonb(n)-'historical_residential'-'connection_key'-'source_at') FROM node_explorer_node_registry n),
   (SELECT jsonb_agg(to_jsonb(n)) FROM node_explorer_snapshots n),
   (SELECT jsonb_agg(to_jsonb(n)) FROM node_explorer_snapshot_members n),
   (SELECT jsonb_agg(to_jsonb(n)) FROM node_explorer_connectivity n),
   (SELECT jsonb_agg(to_jsonb(n)) FROM node_explorer_exits n),
   (SELECT jsonb_agg(to_jsonb(n)) FROM node_explorer_ip_attributes n))::text`).Scan(&out)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := facts()
	raw, err := os.ReadFile("../../../deploy/sql/016-retire-legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.ReplaceAll(string(raw), `\ir ../../backend/internal/database/schema.sql`, database.Schema)
	// An unexpected dependency must abort the transaction, never cascade-delete it.
	if _, err := pool.Exec(ctx, `CREATE VIEW unrelated_view AS SELECT * FROM node_explorer_work`); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, migration); err == nil {
		t.Fatal("unexpected dependency was deleted")
	}
	if _, err = conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	var legacyExists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('node_explorer_work') IS NOT NULL").Scan(&legacyExists); err != nil || !legacyExists {
		t.Fatal("failed migration was not rolled back", err)
	}
	if _, err := pool.Exec(ctx, "DROP VIEW unrelated_view"); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if _, err := pool.Exec(ctx, migration); err != nil {
			t.Fatal(err)
		}
		if facts() != before {
			t.Fatal("retained facts changed")
		}
	}
	var removed bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('node_explorer_work') IS NULL AND to_regclass('node_explorer_retired_nodes') IS NULL AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='node_explorer_node_registry' AND column_name IN ('historical_residential','connection_key','source_at'))`).Scan(&removed); err != nil || !removed {
		t.Fatal("legacy structure retained", err)
	}
	var available int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM node_explorer_live_nodes").Scan(&available); err != nil || available != 1 {
		t.Fatal("live view failed", err)
	}
}
