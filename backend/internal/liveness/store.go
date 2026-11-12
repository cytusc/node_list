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

// Fresh nodes are probed first: a node that has never been probed, whose
// config changed, or whose first success is still waiting to list it can
// reach the homepage with one more probe, so it gets half the slots.
// Available nodes keep a third so the live list stays fresh, and dead
// nodes keep a sixth so a revived address is noticed. No group can starve
// the others, and an empty group leaves its slots to the remaining ones.
const dueSQL = `WITH base AS (
 SELECT n.identity,n.incarnation,n.digest,n.node,n.available,
  a.identity AS attempt_id,a.incarnation AS attempt_incarnation,a.digest AS attempt_digest,
  coalesce((a.payload->>'successStreak')::int,0) AS streak,
  coalesce(a.next_at,'-infinity'::timestamptz) AS due_at
 FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_liveness_attempts a USING(identity)
)
SELECT identity,incarnation,digest,node FROM (
 SELECT identity,incarnation,digest,node,
  CASE WHEN attempt_id IS NULL OR attempt_incarnation<>incarnation OR attempt_digest<>digest OR (NOT available AND streak>=1) THEN 0
   WHEN available THEN 1 ELSE 2 END AS prio,
  CASE WHEN NOT available AND streak>=1 THEN 0 ELSE 1 END AS subprio,
  due_at
 FROM base
 WHERE attempt_id IS NULL OR attempt_incarnation<>incarnation OR attempt_digest<>digest OR due_at<=now()
) s
ORDER BY (row_number() OVER (PARTITION BY prio ORDER BY subprio,due_at,identity)-1)
 / CASE WHEN prio=0 THEN 3 WHEN prio=1 THEN 2 ELSE 1 END,
 prio,subprio,due_at,identity LIMIT $1`

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
	var prevAvailable bool
	err = tx.QueryRow(ctx, `SELECT identity,available FROM node_explorer_node_registry
 WHERE identity=$1 AND incarnation=$2 AND md5((node - 'rawLink' - 'generatedLink')::text)=$3 FOR UPDATE`, n.Identity, n.Incarnation, n.Digest).Scan(&id, &prevAvailable)
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
	// Streaks are consecutive-verdict counters, not per-round resets.
	// infrastructure / config_error / cancelled carry no verdict about
	// the node, so they must neither increment nor clear either streak:
	// the old code reset FailureStreak to 0 on every such round, so a
	// dead node alternating unreachable/infrastructure never reached
	// the 2-failure threshold and stayed available forever.
	// A reachable verdict breaks a failure streak (and vice versa);
	// only a verdict of the same kind extends its streak.
	r.FailureStreak, r.SuccessStreak, r.ErrorStreak = last.FailureStreak, last.SuccessStreak, last.ErrorStreak
	switch r.Status {
	case "unreachable":
		r.FailureStreak = last.FailureStreak + 1
		r.SuccessStreak = 0
	case "reachable":
		r.SuccessStreak = last.SuccessStreak + 1
		r.FailureStreak = 0
	case "config_error":
		r.ErrorStreak = 1
		if last.Status == r.Status && last.Reason == r.Reason {
			r.ErrorStreak = last.ErrorStreak + 1
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
	} else if r.Status == "reachable" && r.SuccessStreak == 1 && !prevAvailable {
		// A first success lists the node immediately; re-probe soon
		// (1 minute, the retry cadence) to confirm it quickly instead
		// of waiting a full interval.
		if delay > time.Minute {
			delay = time.Minute
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(identity) DO UPDATE SET incarnation=EXCLUDED.incarnation,digest=EXCLUDED.digest,
 checked_at=EXCLUDED.checked_at,next_at=EXCLUDED.next_at,payload=EXCLUDED.payload`, id, n.Incarnation, n.Digest, r.CheckedAt, time.Now().Add(delay), b); err != nil {
		return err
	}
	// Availability goes up on the first success and down on 2 consecutive
	// failures: a new node is listed after one good probe, while one
	// transient failure never delists a healthy node.
	flipUp := r.Reachable
	flipDown := !r.Reachable && r.FailureStreak >= 2
	if r.Valid() && (flipUp || flipDown) {
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
