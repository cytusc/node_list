package connectivitybench

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestReadOnlyStartupOverridesEnvironment(t *testing.T) {
	t.Setenv("PGOPTIONS", "-c default_transaction_read_only=off")
	cfg, err := readOnlyConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeParams["default_transaction_read_only"] != "on" {
		t.Fatal("read-only bypass")
	}
}
func TestDatabaseRejectsWrites(t *testing.T) {
	isolated := testutil.Postgres(t)
	t.Setenv("PGHOST", isolated.Host)
	t.Setenv("PGPORT", strconv.Itoa(int(isolated.Port)))
	t.Setenv("PGDATABASE", isolated.Database)
	t.Setenv("PGUSER", isolated.User)
	t.Setenv("PGPASSWORD", isolated.Password)
	t.Setenv("PGOPTIONS", os.Getenv("PGOPTIONS")+" -c default_transaction_read_only=off")
	cfg, err := readOnlyConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "CREATE TABLE benchmark_write_canary(id integer)")
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "25006" {
		t.Fatalf("server accepted write or wrong failure: %v", err)
	}
	var readOnly string
	if err = conn.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
		t.Fatalf("startup guard missing: %v", err)
	}
}

func TestReadOnlySamplingFencesStaleRecoveryCandidates(t *testing.T) {
	isolated := testutil.Postgres(t)
	t.Setenv("PGHOST", isolated.Host)
	t.Setenv("PGPORT", strconv.Itoa(int(isolated.Port)))
	t.Setenv("PGDATABASE", isolated.Database)
	t.Setenv("PGUSER", isolated.User)
	t.Setenv("PGPASSWORD", isolated.Password)
	writable := isolated.Copy()
	writable.RuntimeParams["options"] = os.Getenv("PGOPTIONS")
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, writable)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `CREATE TABLE node_explorer_node_registry(identity text PRIMARY KEY,node jsonb,snapshot_id int,available bool,historical_residential bool);
 CREATE TABLE node_explorer_retired_nodes(identity text,retired_at text,reason text);
 CREATE TABLE node_explorer_connectivity(identity text,payload jsonb,checked_at timestamptz);
 CREATE TABLE node_explorer_snapshots(id int);
 CREATE TABLE node_explorer_work(kind text,state text,payload jsonb,due_at timestamptz);
 INSERT INTO node_explorer_snapshots VALUES(1);
 INSERT INTO node_explorer_node_registry VALUES('target','{"type":"ss","server":"target.example"}',1,true,false);
 INSERT INTO node_explorer_retired_nodes VALUES('target','current','dead');
 INSERT INTO node_explorer_work VALUES('normal-recovery','queued','{"identity":"target","retiredAt":"stale","reason":"dead"}',now()-interval '1 second');`)
	if err != nil {
		t.Fatal(err)
	}
	all, err := ReadCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Due || !all[0].Retired {
		t.Fatal("stale recovery admitted", all)
	}
	if _, err = conn.Exec(ctx, `UPDATE node_explorer_work SET payload=jsonb_set(payload,'{retiredAt}','"current"')`); err != nil {
		t.Fatal(err)
	}
	all, err = ReadCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !all[0].Due {
		t.Fatal("current due candidate omitted")
	}
	if _, err = conn.Exec(ctx, `UPDATE node_explorer_node_registry SET available=false`); err != nil {
		t.Fatal(err)
	}
	all, err = ReadCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Due {
		t.Fatal("unpublished candidate admitted")
	}
}
