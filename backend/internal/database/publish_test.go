package database_test

import (
	"context"
	"testing"

	"github.com/cytusc/node_list/backend/internal/testutil"
)

// The page publishes a node only while three independent facts line up: the
// scheduling flag, a recent connectivity verdict, and a confirmed exit. Each
// one alone has been enough to keep long-dead addresses on the page.
func TestLiveFactsRequireRecentVerdictAndVerifiedExit(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
 INSERT INTO node_explorer_node_registry(identity,node,available) VALUES
  ('fresh','{"name":"fresh"}',true),
  ('stale','{"name":"stale"}',true),
  ('unverified','{"name":"unverified"}',true),
  ('negative','{}',true),
  ('changed','{}',true),
  ('replaced','{}',true),
  ('legacy','{}',true),
  ('scheduling-off','{"name":"off"}',false);
 INSERT INTO node_explorer_connectivity VALUES
  ('fresh',now(),'{"reachable":true}'),
  ('stale',now()-interval '7 hours','{"reachable":true}'),
  ('unverified',now(),'{"reachable":true}'),
  ('scheduling-off',now(),'{"reachable":true}');
 INSERT INTO node_explorer_connectivity
 SELECT identity,now(),'{"reachable":true}' FROM node_explorer_node_registry WHERE identity IN ('negative','changed','replaced','legacy');
 UPDATE node_explorer_connectivity c SET payload=c.payload || jsonb_build_object('incarnation',n.incarnation,'configDigest',md5(n.node::text))
 FROM node_explorer_node_registry n WHERE c.identity=n.identity AND n.identity<>'legacy';
 UPDATE node_explorer_connectivity SET payload=payload || '{"reachable":false}' WHERE identity='negative';
 UPDATE node_explorer_node_registry SET node='{"changed":true}' WHERE identity='changed';
 UPDATE node_explorer_node_registry SET incarnation='replacement' WHERE identity='replaced';
 INSERT INTO node_explorer_exits VALUES
  ('fresh','192.0.2.10',1,now(),'{"ip":"192.0.2.10","verified":true}'),
  ('stale','192.0.2.11',1,now(),'{"ip":"192.0.2.11","verified":true}'),
  ('unverified','192.0.2.12',1,now(),'{"ip":"192.0.2.12","verified":false,"error":"echo_failed"}'),
  ('scheduling-off','192.0.2.13',1,now(),'{"ip":"192.0.2.13","verified":true}');
 INSERT INTO node_explorer_exits SELECT identity,'192.0.2.14',1,now(),'{"verified":true}'
 FROM node_explorer_node_registry WHERE identity IN ('negative','changed','replaced','legacy');
 `); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT identity FROM node_explorer_live_facts ORDER BY identity`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("published %v, want only [fresh]", got)
	}
}
