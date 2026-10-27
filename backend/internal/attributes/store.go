package attributes

import (
	"context"
	"encoding/json"
	"errors"

	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool  *pgxpool.Pool
	owner *pgx.Conn
}

const schemaSQL = `CREATE TABLE IF NOT EXISTS node_explorer_ip_attributes(ip text PRIMARY KEY,checked_at timestamptz NOT NULL,payload jsonb NOT NULL);
 CREATE TABLE IF NOT EXISTS node_explorer_attribute_attempts(ip text PRIMARY KEY,checked_at timestamptz NOT NULL,next_at timestamptz NOT NULL);
 CREATE INDEX IF NOT EXISTS node_explorer_attribute_due ON node_explorer_attribute_attempts(next_at,ip);
 CREATE TABLE IF NOT EXISTS node_explorer_attribute_limits(provider text PRIMARY KEY,next_at timestamptz NOT NULL);`

// Require a current, successful exit observation for an available registry row.
// Legacy observations without incarnation are intentionally left for node-exit.
const eligible = `n.available AND e.ip<>'' AND e.payload->>'verified'='true' AND coalesce(e.payload->>'error','')=''
 AND e.payload->>'incarnation'=n.incarnation AND e.payload->>'ip'=e.ip
 AND pg_input_is_valid(e.ip,'inet') AND e.ip !~ '/' AND e.ip !~ '%'
 AND e.ip=host(CASE WHEN pg_input_is_valid(e.ip,'inet') THEN e.ip::inet END)
 AND NOT (CASE WHEN pg_input_is_valid(e.ip,'inet') THEN e.ip::inet END <<= ANY(ARRAY[
 '0.0.0.0/8','10.0.0.0/8','100.64.0.0/10','127.0.0.0/8','169.254.0.0/16','172.16.0.0/12','192.168.0.0/16','192.0.0.0/24','192.0.2.0/24','198.18.0.0/15','198.51.100.0/24','203.0.113.0/24','224.0.0.0/3','::/128','::1/128','::ffff:0:0/96','fc00::/7','fe80::/10','ff00::/8','2001:db8::/32']::inet[]))`
const hasClass = `(jsonb_typeof(a.payload->'classification'->'types')='array' AND a.payload->'classification'->'types'<>'[]'::jsonb
 OR coalesce(a.payload->'attributes','[]'::jsonb) ?| ARRAY['住宅IP','商业宽带','机房IP','移动网络','教育IP','政府IP','银行网络','组织网络','军事网络','图书馆网络','CDN','搜索引擎','保留IP','其他网络'])`
const classMissing = `NOT coalesce(` + hasClass + `,false)`
const riskMissing = `(position(':' in e.ip)=0 AND coalesce(a.payload->>'status','')<>'success')`
const countryMissing = `coalesce(a.payload->>'country','')=''`
const missing = `(` + classMissing + ` OR ` + riskMissing + ` OR ` + countryMissing + `)`

func (d *DB) Acquire(ctx context.Context) error {
	cfg := d.Pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "node-attributes"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	d.owner = conn
	var ok bool
	// Same ownership key as the retired worker: old and new writers cannot coexist.
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext(current_schema()),hashtext('node-explorer-service:exit-attributes'))`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("exit-attributes already running")
	}
	_, err = conn.Exec(ctx, schemaSQL)
	return err
}
func (d *DB) Close() {
	if d.owner != nil {
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = d.owner.Close(ctx)
	}
}
func (d *DB) Due(ctx context.Context, limit int) ([]Candidate, error) {
	rows, err := d.Pool.Query(ctx, `SELECT DISTINCT e.ip,coalesce(a.payload,'{}'::jsonb),`+classMissing+`,`+riskMissing+`,`+countryMissing+`,coalesce(t.next_at,'1970-01-01'::timestamptz)
 FROM node_explorer_exits e JOIN node_explorer_node_registry n USING(identity)
 LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip AND coalesce(a.payload->>'ip',a.ip)=e.ip LEFT JOIN node_explorer_attribute_attempts t ON t.ip=e.ip
 WHERE `+eligible+` AND `+missing+` AND (t.ip IS NULL OR t.next_at<=now())
 ORDER BY coalesce(t.next_at,'1970-01-01'::timestamptz),e.ip LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var n Candidate
		var b []byte
		var next time.Time
		if err = rows.Scan(&n.IP, &b, &n.ClassificationMissing, &n.RiskMissing, &n.CountryMissing, &next); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &n.Previous); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func (d *DB) Current(ctx context.Context, n Candidate) (bool, error) {
	var ok bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_exits e JOIN node_explorer_node_registry n USING(identity) LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip AND coalesce(a.payload->>'ip',a.ip)=e.ip WHERE e.ip=$1 AND `+eligible+` AND `+missing+`)`, n.IP).Scan(&ok)
	return ok, err
}
func (d *DB) Save(ctx context.Context, n Candidate, r Result, retry time.Duration) error {
	tx, err := d.owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = tx.Rollback(c)
	}()
	var id string
	// Hold the selected user's availability and exit mapping until commit. If
	// another available node still uses the IP, the shared result remains useful.
	err = tx.QueryRow(ctx, `SELECT n.identity FROM node_explorer_exits e JOIN node_explorer_node_registry n USING(identity)
 WHERE e.ip=$1 AND `+eligible+` ORDER BY n.identity LIMIT 1 FOR SHARE OF n,e`, n.IP).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var b []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip=$1 FOR UPDATE`, n.IP).Scan(&b)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if len(b) > 0 {
		if err = json.Unmarshal(b, &n.Previous); err != nil {
			return err
		}
	}
	if n.Previous.IP != "" && n.Previous.IP != n.IP {
		n.Previous = domain.IPPureResult{}
	}
	now := time.Now().UTC()
	v := merge(n, r, now)
	b, err = json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_ip_attributes(ip,checked_at,payload) VALUES($1,$2,$3)
 ON CONFLICT(ip) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, n.IP, now, b)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_attribute_attempts VALUES($1,$2,$3)
 ON CONFLICT(ip) DO UPDATE SET checked_at=EXCLUDED.checked_at,next_at=EXCLUDED.next_at`, n.IP, now, now.Add(retry))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Persist upstream cooldowns so a process restart cannot bypass Retry-After.
func (d *DB) BeforeProvider(ctx context.Context, provider string) error {
	var next time.Time
	err := d.Pool.QueryRow(ctx, `SELECT next_at FROM node_explorer_attribute_limits WHERE provider=$1`, provider).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	wait := time.Until(next)
	if wait > 10*time.Second {
		return errors.New("provider cooling down")
	}
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (d *DB) AfterProvider(ctx context.Context, provider string, next time.Time) error {
	_, err := d.Pool.Exec(ctx, `INSERT INTO node_explorer_attribute_limits VALUES($1,$2) ON CONFLICT(provider) DO UPDATE SET next_at=greatest(node_explorer_attribute_limits.next_at,EXCLUDED.next_at)`, provider, next)
	return err
}
func (d *DB) Heartbeat(ctx context.Context, running bool) error {
	b, _ := json.Marshal(map[string]any{"running": running, "error": ""})
	_, err := d.owner.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('exit-attributes',now(),$1) ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, b)
	return err
}
func (d *DB) Healthy(ctx context.Context) error {
	var ok bool
	err := d.Pool.QueryRow(ctx, `SELECT updated_at>now()-interval '30 seconds' AND payload->>'running'='true' AND coalesce(payload->>'error','')='' FROM node_explorer_service_status WHERE service='exit-attributes'`).Scan(&ok)
	if err != nil || !ok {
		return errors.New("attributes heartbeat unavailable")
	}
	return nil
}
