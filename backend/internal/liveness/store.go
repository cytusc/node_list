package liveness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
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
// SaveItem is one completed probe result awaiting persistence.
type SaveItem struct {
	Node     Candidate
	Result   Result
	Interval time.Duration
}

func (d *DB) Save(ctx context.Context, n Candidate, r Result, interval time.Duration) error {
	return d.SaveBatch(ctx, []SaveItem{{Node: n, Result: r, Interval: interval}})
}

// SaveBatch persists a batch of completed probe results in one transaction:
// one registry lookup, one previous-attempt lookup, and one multi-row
// statement per table, instead of one transaction with up to three
// statements per result. The runner buffers results and flushes every
// few seconds (or when the buffer fills), so a single probe no longer
// costs a full round trip.
func (d *DB) SaveBatch(ctx context.Context, items []SaveItem) error {
	if len(items) == 0 {
		return nil
	}
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
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.Node.Identity
	}
	type regRow struct {
		available           bool
		incarnation, digest string
	}
	reg := map[string]regRow{}
	rows, err := tx.Query(ctx, `SELECT identity,available,incarnation,digest FROM node_explorer_node_registry WHERE identity=ANY($1) FOR UPDATE`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var rr regRow
		if err = rows.Scan(&id, &rr.available, &rr.incarnation, &rr.digest); err != nil {
			rows.Close()
			return err
		}
		reg[id] = rr
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	type attRow struct {
		incarnation, digest string
		payload             []byte
	}
	prev := map[string]attRow{}
	rows, err = tx.Query(ctx, `SELECT identity,incarnation,digest,payload FROM node_explorer_liveness_attempts WHERE identity=ANY($1)`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var a attRow
		if err = rows.Scan(&id, &a.incarnation, &a.digest, &a.payload); err != nil {
			rows.Close()
			return err
		}
		prev[id] = a
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	now := time.Now()
	type attempt struct {
		id, incarnation, digest string
		checkedAt, nextAt       time.Time
		payload                 []byte
	}
	var attempts []attempt
	type flip struct {
		id        string
		checkedAt time.Time
		payload   []byte
		available bool
	}
	var flips []flip
	for _, it := range items {
		n, r := it.Node, it.Result
		rr, ok := reg[n.Identity]
		if !ok || rr.incarnation != n.Incarnation || rr.digest != n.Digest {
			// Node changed or was deleted; skip this result.
			continue
		}
		// Retry deterministic configuration errors less often without treating
		// them as negative availability verdicts. Changed configurations start fresh.
		var last Result
		if a, ok := prev[n.Identity]; ok && a.incarnation == n.Incarnation && a.digest == n.Digest {
			_ = json.Unmarshal(a.payload, &last)
		}
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
		delay := it.Interval
		if r.FailureStreak > 0 {
			delay = backoff(it.Interval, r.FailureStreak, d.MaxBackoff)
		} else if r.ErrorStreak > 0 {
			delay = backoff(it.Interval, r.ErrorStreak, time.Hour)
		} else if r.Status == "infrastructure" && it.Interval > 0 {
			delay += time.Duration(rand.Int64N(int64(it.Interval)/5 + 1))
		} else if r.Status == "reachable" && r.SuccessStreak == 1 && !rr.available {
			// A first success lists the node immediately; re-probe soon
			// (1 minute, the retry cadence) to confirm it quickly instead
			// of waiting a full interval.
			if delay > time.Minute {
				delay = time.Minute
			}
		}
		attempts = append(attempts, attempt{n.Identity, n.Incarnation, n.Digest, r.CheckedAt, now.Add(delay), b})
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
			sb, _ := json.Marshal(state)
			flips = append(flips, flip{n.Identity, r.CheckedAt, sb, r.Reachable})
		}
	}
	if len(attempts) > 0 {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload) VALUES `)
		args := make([]any, 0, len(attempts)*6)
		for i, a := range attempts {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d)", i*6+1, i*6+2, i*6+3, i*6+4, i*6+5, i*6+6)
			args = append(args, a.id, a.incarnation, a.digest, a.checkedAt, a.nextAt, a.payload)
		}
		sb.WriteString(` ON CONFLICT(identity) DO UPDATE SET incarnation=EXCLUDED.incarnation,digest=EXCLUDED.digest,checked_at=EXCLUDED.checked_at,next_at=EXCLUDED.next_at,payload=EXCLUDED.payload`)
		if _, err = tx.Exec(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	if len(flips) > 0 {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES `)
		args := make([]any, 0, len(flips)*3)
		for i, f := range flips {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d,$%d,$%d)", i*3+1, i*3+2, i*3+3)
			args = append(args, f.id, f.checkedAt, f.payload)
		}
		sb.WriteString(` ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`)
		if _, err = tx.Exec(ctx, sb.String(), args...); err != nil {
			return err
		}
		var sb2 strings.Builder
		sb2.WriteString(`UPDATE node_explorer_node_registry r SET available=v.available FROM (VALUES `)
		args2 := make([]any, 0, len(flips)*2)
		for i, f := range flips {
			if i > 0 {
				sb2.WriteString(",")
			}
			fmt.Fprintf(&sb2, "($%d,$%d::boolean)", i*2+1, i*2+2)
			args2 = append(args2, f.id, f.available)
		}
		sb2.WriteString(`) AS v(identity,available) WHERE r.identity=v.identity`)
		if _, err = tx.Exec(ctx, sb2.String(), args2...); err != nil {
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
