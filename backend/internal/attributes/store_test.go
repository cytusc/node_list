package attributes

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func fixture(t *testing.T) *DB {
	t.Helper()
	cfg := testutil.Postgres(t)
	cfg.RuntimeParams["search_path"] = strings.TrimPrefix(os.Getenv("PGOPTIONS"), "-c search_path=")
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig = cfg
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Only public input/output contracts exist: no snapshots, Redis or task services.
	_, err = pool.Exec(context.Background(), `CREATE TABLE node_explorer_node_registry(identity text PRIMARY KEY,incarnation text NOT NULL,node jsonb NOT NULL,available boolean NOT NULL);
 CREATE TABLE node_explorer_exits(identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity),ip text NOT NULL,version bigint NOT NULL DEFAULT 1,checked_at timestamptz NOT NULL,payload jsonb NOT NULL);
 CREATE TABLE node_explorer_ip_attributes(ip text PRIMARY KEY,checked_at timestamptz NOT NULL,payload jsonb NOT NULL);
 CREATE TABLE node_explorer_service_status(service text PRIMARY KEY,updated_at timestamptz NOT NULL,payload jsonb NOT NULL);`)

	if err != nil {
		t.Fatal(err)
	}
	db := &DB{Pool: pool}
	t.Cleanup(db.Close)
	if err = db.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func execSQL(t *testing.T, d *DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func putNode(t *testing.T, d *DB, id, ip string, available, verified bool) {
	t.Helper()
	execSQL(t, d, `INSERT INTO node_explorer_node_registry VALUES($1,'v1','{}',$2)`, id, available)
	b, _ := json.Marshal(domain.ExitState{IP: ip, Incarnation: "v1", Verified: verified})
	execSQL(t, d, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,$2,now(),$3)`, id, ip, b)
}
func pending(t *testing.T, d *DB) []Candidate {
	t.Helper()
	v, err := d.Due(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestDiscoveryFiltersAndDeduplicates(t *testing.T) {
	d := fixture(t)
	putNode(t, d, "a", "8.8.8.8", true, true)
	putNode(t, d, "b", "8.8.8.8", true, true)
	putNode(t, d, "dead", "1.1.1.1", false, true)
	putNode(t, d, "unverified", "9.9.9.9", true, false)
	putNode(t, d, "old", "4.2.2.2", true, true)
	execSQL(t, d, `UPDATE node_explorer_exits SET payload=payload-'incarnation' WHERE identity='old'`)
	for i, ip := range []string{"127.0.0.1", "10.0.0.1", "not-an-ip", "8.8.8.8/32", "2001:db8::1", "::ffff:8.8.8.8", "8.8.8", "8"} {
		putNode(t, d, string(rune('k'+i)), ip, true, true)
	}
	v := pending(t, d)
	if len(v) != 1 || v[0].IP != "8.8.8.8" || !v[0].RiskMissing || !v[0].ClassificationMissing {
		t.Fatalf("unexpected inputs: %+v", v)
	}
}
func TestCompletionAndPartialRetry(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	putNode(t, d, "a", "8.8.8.8", true, true)
	n := pending(t, d)[0]
	evidence := []ipcheck.Evidence{{Provider: "ipinfo", IP: n.IP, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), UsageType: "business", Outcome: "unknown", QueryStatus: "success"}}
	if err := d.Save(ctx, n, Result{Evidence: evidence, RiskAttempted: true, RiskError: "http-429"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if v := pending(t, d); len(v) != 0 {
		t.Fatal("retry cooldown ignored")
	}
	execSQL(t, d, `UPDATE node_explorer_attribute_attempts SET next_at=now()-interval '1 second'`)
	n = pending(t, d)[0]
	if n.ClassificationMissing || !n.RiskMissing {
		t.Fatalf("successful partial information requested again: %+v", n)
	}
	if err := d.Save(ctx, n, Result{RiskAttempted: true, Risk: &collector.IPPureResult{IP: n.IP, Score: 10, Country: "US", Attributes: []string{"商业宽带"}}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	execSQL(t, d, `UPDATE node_explorer_attribute_attempts SET next_at=now()-interval '1 second'`)
	if v := pending(t, d); len(v) != 0 {
		t.Fatal("complete attributes refreshed")
	}
	var available bool
	execSQL(t, d, `SELECT 1`)
	if err := d.Pool.QueryRow(ctx, `SELECT available FROM node_explorer_node_registry WHERE identity='a'`).Scan(&available); err != nil || !available {
		t.Fatal("availability modified", err)
	}
}

func TestSharedIPReusesAttributesForNewNodes(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	putNode(t, d, "a", "8.8.8.8", true, true)
	n := pending(t, d)[0]
	if err := d.Save(ctx, n, Result{RiskAttempted: true, Risk: &collector.IPPureResult{
		IP: n.IP, Country: "US", Attributes: []string{"住宅IP"},
	}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	putNode(t, d, "b", n.IP, true, true)
	putNode(t, d, "c", n.IP, true, true)
	// Completed shared attributes must skip queries even without a retry record.
	execSQL(t, d, `DELETE FROM node_explorer_attribute_attempts`)
	if v := pending(t, d); len(v) != 0 {
		t.Fatalf("new nodes caused completed shared IP to be queried: %+v", v)
	}
	execSQL(t, d, `UPDATE node_explorer_ip_attributes SET payload=payload-'country'`)
	v := pending(t, d)
	if len(v) != 1 || v[0].IP != n.IP || !v[0].CountryMissing || v[0].ClassificationMissing || v[0].RiskMissing {
		t.Fatalf("shared IP should only need one country lookup: %+v", v)
	}
	if err := d.Save(ctx, v[0], Result{Country: "US"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	execSQL(t, d, `DELETE FROM node_explorer_attribute_attempts`)
	if v := pending(t, d); len(v) != 0 {
		t.Fatalf("completed shared country was requested again: %+v", v)
	}
}
func TestLateResultRequiresAnyCurrentAvailableUser(t *testing.T) {
	for _, change := range []string{`UPDATE node_explorer_node_registry SET available=false`, `UPDATE node_explorer_node_registry SET incarnation='v2'`, `UPDATE node_explorer_exits SET ip='1.1.1.1'`, `UPDATE node_explorer_exits SET payload=payload||'{"verified":false}'`} {
		t.Run(change, func(t *testing.T) {
			d := fixture(t)
			putNode(t, d, "a", "8.8.8.8", true, true)
			n := pending(t, d)[0]
			execSQL(t, d, change)
			if ok, err := d.Current(context.Background(), n); err != nil || ok {
				t.Fatal("stale input accepted", ok, err)
			}
			result := Result{Country: "US"}
			if err := d.Save(context.Background(), n, result, time.Hour); err != nil {
				t.Fatal(err)
			}
			var count int
			d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_ip_attributes`).Scan(&count)
			if count != 0 {
				t.Fatal("stale write")
			}
			putNode(t, d, "b", "8.8.8.8", true, true)
			if err := d.Save(context.Background(), n, result, time.Hour); err != nil {
				t.Fatal(err)
			}
			d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_ip_attributes`).Scan(&count)
			if count != 1 {
				t.Fatal("shared IP result discarded")
			}
		})
	}
}
func TestIPv6ClassificationOnly(t *testing.T) {
	d := fixture(t)
	putNode(t, d, "a", "2606:4700:4700::1111", true, true)
	v := pending(t, d)
	if len(v) != 1 || v[0].RiskMissing || !v[0].ClassificationMissing {
		t.Fatalf("IPv6 %+v", v)
	}
}
func TestOwnershipLossPreventsCommitAndDuplicate(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	putNode(t, d, "a", "8.8.8.8", true, true)
	n := pending(t, d)[0]
	other := &DB{Pool: d.Pool}
	if err := other.Acquire(ctx); err == nil {
		t.Fatal("duplicate accepted")
	}
	other.Close()
	if err := d.Heartbeat(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := d.Healthy(ctx); err != nil {
		t.Fatal(err)
	}
	var killed bool
	if err := d.Pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, d.owner.PgConn().PID()).Scan(&killed); err != nil || !killed {
		t.Fatal(err)
	}
	if err := other.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := d.Save(ctx, n, Result{Country: "US"}, time.Hour); err == nil {
		t.Fatal("lost owner saved")
	}
	if err := other.Heartbeat(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := other.Healthy(ctx); err == nil {
		t.Fatal("stopped marked healthy")
	}
}
func TestProviderCooldownSurvivesClientRestart(t *testing.T) {
	d := fixture(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	if err := d.AfterProvider(ctx, "ipinfo", until); err != nil {
		t.Fatal(err)
	}
	if err := d.AfterProvider(ctx, "ipinfo", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := (&DB{Pool: d.Pool}).BeforeProvider(ctx, "ipinfo"); err == nil {
		t.Fatal("cooldown lost")
	}
}
