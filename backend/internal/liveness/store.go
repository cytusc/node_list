package liveness

import (
	"context"
	"encoding/json"
	"errors"
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
const dueSQL = `SELECT n.identity,n.incarnation,md5(n.node::text),n.node
 FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_liveness_attempts a USING(identity)
 WHERE EXISTS(SELECT 1 FROM node_explorer_snapshot_members m WHERE m.identity=n.identity AND m.snapshot_id=n.snapshot_id)
 AND (a.identity IS NULL OR a.incarnation<>n.incarnation OR a.digest<>md5(n.node::text) OR a.next_at<=now())
 ORDER BY coalesce(a.next_at,'-infinity'::timestamptz),n.identity LIMIT $1`

const schemaSQL = `CREATE TABLE IF NOT EXISTS node_explorer_liveness_attempts (
 identity text PRIMARY KEY REFERENCES node_explorer_node_registry(identity) ON DELETE CASCADE,
 incarnation text NOT NULL, digest text NOT NULL, checked_at timestamptz NOT NULL,
 next_at timestamptz NOT NULL, payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS node_explorer_liveness_due ON node_explorer_liveness_attempts(next_at,identity);`

type DB struct {
	Pool  *pgxpool.Pool
	owner *pgx.Conn
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
 WHERE identity=$1 AND incarnation=$2 AND md5(node::text)=$3
 AND EXISTS(SELECT 1 FROM node_explorer_snapshot_members m WHERE m.identity=node_explorer_node_registry.identity AND m.snapshot_id=node_explorer_node_registry.snapshot_id) FOR UPDATE`, n.Identity, n.Incarnation, n.Digest).Scan(&id)
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
	if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_liveness_attempts(identity,incarnation,digest,checked_at,next_at,payload)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(identity) DO UPDATE SET incarnation=EXCLUDED.incarnation,digest=EXCLUDED.digest,
 checked_at=EXCLUDED.checked_at,next_at=EXCLUDED.next_at,payload=EXCLUDED.payload`, id, n.Incarnation, n.Digest, r.CheckedAt, time.Now().Add(interval), b); err != nil {
		return err
	}
	if r.Valid() {
		state := map[string]any{"reachable": r.Reachable, "checkedAt": r.CheckedAt, "probeTarget": Target, "engine": r.Engine, "latencyMs": r.LatencyMS, "status": r.Status, "incarnation": n.Incarnation, "configDigest": n.Digest}
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
