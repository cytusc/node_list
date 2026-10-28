package exitprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Candidate struct {
	Identity, Incarnation, Digest string
	Node                          json.RawMessage
}
type DB struct {
	Pool  *pgxpool.Pool
	owner *pgx.Conn
}

const schemaSQL = `CREATE TABLE IF NOT EXISTS node_explorer_exit_attempts (
 identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity) ON DELETE CASCADE,
 incarnation text NOT NULL,digest text NOT NULL,checked_at timestamptz NOT NULL,next_at timestamptz NOT NULL,payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS node_explorer_exit_due ON node_explorer_exit_attempts(next_at,identity);`

func (d *DB) Acquire(ctx context.Context) error {
	cfg := d.Pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "node-exit"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	d.owner = conn
	var ok bool
	// Same ownership key as the retired worker: old and new writers cannot coexist.
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext(current_schema()),hashtext('node-explorer-service:exit-provision'))`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("exit-provision already running")
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
	rows, err := d.Pool.Query(ctx, `SELECT n.identity,n.incarnation,md5(n.node::text),n.node FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_exit_attempts a USING(identity)
 WHERE n.available AND (a.identity IS NULL OR a.incarnation<>n.incarnation OR a.digest<>md5(n.node::text) OR a.next_at<=now())
 ORDER BY coalesce(a.next_at,'-infinity'::timestamptz),n.identity LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var n Candidate
		if err = rows.Scan(&n.Identity, &n.Incarnation, &n.Digest, &n.Node); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
func (d *DB) Current(ctx context.Context, n Candidate) (bool, error) {
	var ok bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_node_registry WHERE identity=$1 AND available AND incarnation=$2 AND md5(node::text)=$3)`, n.Identity, n.Incarnation, n.Digest).Scan(&ok)
	return ok, err
}
func (d *DB) Save(ctx context.Context, n Candidate, r Result, interval time.Duration) error {
	address, parseErr := netip.ParseAddr(r.IP)
	if r.CheckedAt.IsZero() || (r.Error == "" && (parseErr != nil || !publicIP(address))) {
		return errors.New("invalid_exit_result")
	}
	// All writes use the locked session, serially. A lost session cannot commit
	// after a successor acquires ownership, even if an old probe is still running.
	tx, err := d.owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	var id string
	err = tx.QueryRow(ctx, `SELECT identity FROM node_explorer_node_registry WHERE identity=$1 AND available AND incarnation=$2 AND md5(node::text)=$3 FOR UPDATE`, n.Identity, n.Incarnation, n.Digest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_exit_attempts(identity,incarnation,digest,checked_at,next_at,payload) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(identity) DO UPDATE SET incarnation=EXCLUDED.incarnation,digest=EXCLUDED.digest,checked_at=EXCLUDED.checked_at,next_at=EXCLUDED.next_at,payload=EXCLUDED.payload`, id, n.Incarnation, n.Digest, r.CheckedAt, time.Now().Add(interval), b); err != nil {
		return err
	}
	var previous domain.ExitState
	var old []byte
	var version int64
	var oldIP string
	err = tx.QueryRow(ctx, `SELECT payload,version,ip FROM node_explorer_exits WHERE identity=$1`, id).Scan(&old, &version, &oldIP)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if len(old) > 0 {
		if err = json.Unmarshal(old, &previous); err != nil {
			return err
		}
	}
	previous.IP = oldIP // Canonical column may predate a matching payload field.
	// Preserve the last confirmed location before replacing the exit mapping.
	// Capture only the matching old IP's attributes, never the new IP's type.
	_, err = tx.Exec(ctx, `UPDATE node_explorer_exits e SET pinned_country=upper(c.country)
 FROM LATERAL (SELECT coalesce(
   (SELECT nullif(a.payload->>'country','') FROM node_explorer_ip_attributes a
    WHERE a.ip=$2 AND a.payload->>'ip'=$2),nullif($3::jsonb->>'country','')) AS country) c
 WHERE e.identity=$1 AND e.pinned_country IS NULL AND c.country ~ '^[a-zA-Z]{2}$'`, id, oldIP, old)
	if err != nil {
		return err
	}
	v := domain.ExitState{Incarnation: n.Incarnation, IP: r.IP, Endpoint: r.Endpoint, Source: "echo", Verified: r.Error == "" && r.IP != "", CheckedAt: r.CheckedAt.Format(time.RFC3339Nano), ProbeMillis: r.Millis, Error: r.Error, Version: max(1, version)}
	if v.Verified {
		if version > 0 && previous.IP != v.IP {
			v.Version++
		}
		v.AddressFamily = 6
		if netip.MustParseAddr(v.IP).Is4() {
			v.AddressFamily = 4
		}
	} else {
		v.IP = previous.IP
		v.Endpoint = previous.Endpoint
		v.AddressFamily = previous.AddressFamily
		v.FailureStreak = 1
		if previous.Incarnation == n.Incarnation {
			v.FailureStreak = min(16, previous.FailureStreak+1)
		}
	}
	b, err = json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_exits(identity,ip,version,checked_at,payload) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(identity) DO UPDATE SET ip=EXCLUDED.ip,version=EXCLUDED.version,checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, id, v.IP, v.Version, r.CheckedAt, b)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (d *DB) Heartbeat(ctx context.Context, running bool) error {
	b, _ := json.Marshal(map[string]any{"running": running, "error": ""})
	_, err := d.owner.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('exit-provision',now(),$1) ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, b)
	return err
}
func (d *DB) Healthy(ctx context.Context) error {
	var ok bool
	err := d.Pool.QueryRow(ctx, `SELECT updated_at>now()-interval '30 seconds' AND payload->>'running'='true' AND coalesce(payload->>'error','')='' FROM node_explorer_service_status WHERE service='exit-provision'`).Scan(&ok)
	if err != nil || !ok {
		return errors.New("exit heartbeat unavailable")
	}
	return nil
}
