package liveness

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Candidate struct {
	Identity, Incarnation, Digest string
	Node                          json.RawMessage
}

// These are input and output contracts, not calls into other services.
// available is an output only: even previously unreachable nodes are scheduled.
const dueSQL = `SELECT n.identity,n.incarnation,md5((n.node - 'rawLink' - 'generatedLink')::text),n.node
 FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_liveness_attempts a USING(identity)
 WHERE (a.identity IS NULL OR a.incarnation<>n.incarnation OR a.digest<>md5((n.node - 'rawLink' - 'generatedLink')::text) OR a.next_at<=now())
 -- Interleave three available nodes with one other node. Neither group can
 -- starve the other, and an empty group leaves all slots to the remaining one.
 ORDER BY (row_number() OVER (PARTITION BY n.available ORDER BY coalesce(a.next_at,'-infinity'::timestamptz),n.identity)-1)
 / CASE WHEN n.available THEN 3 ELSE 1 END, n.available DESC,
 coalesce(a.next_at,'-infinity'::timestamptz),n.identity LIMIT $1`

const schemaSQL = `CREATE TABLE IF NOT EXISTS node_explorer_liveness_attempts (
 identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity) ON DELETE CASCADE,
 incarnation text NOT NULL, digest text NOT NULL, checked_at timestamptz NOT NULL,
 next_at timestamptz NOT NULL, payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS node_explorer_liveness_due ON node_explorer_liveness_attempts(next_at,identity);`

type DB struct {
	Pool  *pgxpool.Pool
	owner *pgx.Conn
	// MaxBackoff caps the retry delay for addresses that keep failing. Without
	// it every dead address returns after Interval, the due queue grows faster
	// than the workers can drain it, and availability conclusions age by hours.
	MaxBackoff time.Duration
}

// backoff doubles the retry delay per consecutive failure, capped at max.
// The first failure keeps the normal interval; each further consecutive
// failure doubles it, so a dead address stops competing with live ones.
func backoff(interval time.Duration, streak int, max time.Duration) time.Duration {
	if interval <= 0 || streak <= 0 {
		return interval
	}
	if max <= 0 {
		max = 24 * time.Hour
	}
	delay := interval
	for i := 1; i < streak && delay < max; i++ {
		delay *= 2
	}
	if delay > max {
		delay = max
	}
	return delay
}

func (d *DB) Acquire(ctx context.Context) error {
	cfg := d.Pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "node-liveness"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	d.owner = conn
	var ok bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext(current_schema()),hashtext('node-liveness'))`).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("node-liveness already running")
	}
	_, err = conn.Exec(ctx, schemaSQL)
	return err
}
func (d *DB) Close() {
	if d.owner != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = d.owner.Close(ctx)
	}
}
func (d *DB) Due(ctx context.Context, limit int) ([]Candidate, error) {
	rows, err := d.Pool.Query(ctx, dueSQL, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Candidate
	for rows.Next() {
		var n Candidate
		if err = rows.Scan(&n.Identity, &n.Incarnation, &n.Digest, &n.Node); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
func (d *DB) Save(ctx context.Context, n Candidate, r Result, interval time.Duration) error {
	// The single writer uses the very session holding ownership. A lost lock
	// connection cannot leave pooled writers committing after ownership changes.
	tx, err := d.owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}()
	var id string
	err = tx.QueryRow(ctx, `SELECT identity FROM node_explorer_node_registry
 WHERE identity=$1 AND incarnation=$2 AND md5((node - 'rawLink' - 'generatedLink')::text)=$3 FOR UPDATE`, n.Identity, n.Incarnation, n.Digest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Retry deterministic configuration errors less often without treating
	// them as negative availability verdicts. Changed configurations start fresh.
	var previous []byte
	if err = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_liveness_attempts WHERE identity=$1 AND incarnation=$2 AND digest=$3`, id, n.Incarnation, n.Digest).Scan(&previous); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var last Result
	_ = json.Unmarshal(previous, &last)
	r.FailureStreak, r.ErrorStreak = 0, 0
	if r.Status == "unreachable" {
		r.FailureStreak = last.FailureStreak + 1
	} else if r.Status == "config_error" {
		r.ErrorStreak = 1
		if last.Status == r.Status && last.Reason == r.Reason {
			r.ErrorStreak += last.ErrorStreak
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	delay := interval
	if r.FailureStreak > 0 {
		delay = backoff(interval, r.FailureStreak, d.MaxBackoff)
	} else if r.ErrorStreak > 0 {
		delay = backoff(interval, r.ErrorStreak, time.Hour)
	} else if r.Status == "infrastructure" && interval > 0 {
		delay += time.Duration(rand.Int64N(int64(interval)/5 + 1))
	}
	if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(identity) DO UPDATE SET incarnation=EXCLUDED.incarnation,digest=EXCLUDED.digest,
 checked_at=EXCLUDED.checked_at,next_at=EXCLUDED.next_at,payload=EXCLUDED.payload`, id, n.Incarnation, n.Digest, r.CheckedAt, time.Now().Add(delay), b); err != nil {
		return err
	}
	// One failed round can be transient. Keep the last positive verdict until
	// a second consecutive failure; its timestamp still enforces freshness.
	if r.Valid() && (r.Reachable || r.FailureStreak >= 2) {
		state := map[string]any{"reachable": r.Reachable, "checkedAt": r.CheckedAt, "probeTarget": r.ProbeTarget, "targets": r.Targets, "engine": r.Engine, "latencyMs": r.LatencyMS, "status": r.Status, "incarnation": n.Incarnation, "configDigest": n.Digest}
		if !r.Reachable {
			state["error"] = "target_unreachable"
		}
		b, _ = json.Marshal(state)
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,$2,$3)
 ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload
 `, id, r.CheckedAt, b); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE node_explorer_node_registry SET available=$2 WHERE identity=$1`, id, r.Reachable); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (d *DB) Heartbeat(ctx context.Context, stats any) error {
	b, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	_, err = d.owner.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('node-liveness',now(),$1)
 ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, b)
	return err
}
