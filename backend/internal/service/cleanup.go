package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

// How long a node must have gone without being new or last known available
// before its death earns a second, controlled verification. The default keeps a
// week of grace for flapping nodes; the fresh-proof rules in deleteUnavailable
// are the real guard, so an operator can shorten the window (a registry whose
// rows all postdate a column migration cannot satisfy a week) without weakening
// them.
const defaultCleanupMinAge = 7 * 24 * time.Hour

func cleanupMinAge() time.Duration {
	return Seconds("NODE_CLEANUP_MIN_AGE_SECONDS", int(defaultCleanupMinAge.Seconds()))
}

// cleanupMinPool stops deletions from emptying the survival set. A source
// outage, an egress fault or a mass false-negative would otherwise delete every
// temporarily unreachable node and leave nothing to serve. Below this many
// available nodes deletions are refused and the same nodes are retried later,
// when the pool has recovered. 0 disables the floor.
func cleanupMinPool() int {
	v, e := strconv.Atoi(os.Getenv("NODE_CLEANUP_MIN_POOL"))
	if e != nil || v < 0 {
		return 500
	}
	return min(v, 1000000)
}

func intervalParam(d time.Duration) string {
	return fmt.Sprintf("%f seconds", d.Seconds())
}

// tcpVerifiable identifies TCP endpoints eligible for the source admission check.
// QUIC-based transports bypass this check. Deletion always requires a controlled
// protocol handshake, regardless of endpoint reachability.
func tcpVerifiable(n domain.Node) bool {
	switch strings.ToLower(n.Type) {
	case "http", "https", "socks5", "socks", "ss", "ssr", "vmess", "vless", "trojan", "anytls":
		return n.Server != "" && n.Port != ""
	}
	return false
}

// dialEndpoint is a direct TCP connect with no proxy stack in the path. A dial
// that completes proves only that the endpoint accepts connections; it cannot
// establish protocol health and is used only by source admission.
func dialEndpoint(ctx context.Context, server, port string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(server, port))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Imports never refresh the monotonic successful observation clock.
func recordAvailable(ctx context.Context, tx pgx.Tx, id, incarnation, stamp string) error {
	_, err := tx.Exec(ctx, `UPDATE node_explorer_node_registry SET last_available_at=greatest(last_available_at,$3::timestamptz)
 WHERE identity=$1 AND incarnation=$2 AND $3::timestamptz<=clock_timestamp()`, id, incarnation, stamp)
	return err
}
func (r *Repository) probeTarget(ctx context.Context, p Payload) (*Target, error) {
	return r.Target(ctx, p.Identity)
}

func cleanupGuard(ctx context.Context, tx pgx.Tx, p Payload) (bool, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT identity FROM node_explorer_node_registry WHERE identity=$1
 AND incarnation=$2 AND md5(node::text)=$3 FOR UPDATE`, p.Identity, p.Incarnation, p.Reason).Scan(&id)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// The registry lock fences successful commits. Active probes and config changes veto deletion.
// deleteUnavailable applies every gate and removes the node. compact=true also
// rewrites the snapshot that carries the identity, which costs O(snapshot) and
// needs the exclusive snapshot locks; bulk paths pass false and let
// CompactDeleted pay that cost once per batch instead of once per node.
func deleteUnavailable(ctx context.Context, tx pgx.Tx, j Job, proof domain.ConnectivityState, session string, compact bool) (bool, string, error) {
	if proof.Reachable || !proof.ConfirmedDead || !fresh(proof.CheckedAt, time.Minute) {
		return false, "stale-or-live-proof", nil
	}
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); e != nil {
		return false, "", e
	}
	if compact {
		// The registry advisory lock already serializes this deletion with source
		// folding; the exclusive snapshot locks only guard the payload rewrite.
		if _, e := tx.Exec(ctx, `LOCK TABLE node_explorer_snapshots,node_explorer_ippure_results IN EXCLUSIVE MODE`); e != nil {
			return false, "", e
		}
	}
	ok, e := cleanupGuard(ctx, tx, j.Payload)
	if e != nil || !ok {
		return false, "guard", e
	}
	var admitted bool
	if e = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext(current_schema()),hashtext($1))`, "node-probe:"+j.Payload.Identity).Scan(&admitted); e != nil || !admitted {
		return false, "probe-in-flight", e
	}
	// One statement returns either the refusal reason or nothing, so a parked
	// deletion says why instead of a generic "not-safe".
	var refusal string
	e = tx.QueryRow(ctx, `SELECT CASE
 WHEN (SELECT count(*) FROM (SELECT 1 FROM node_explorer_node_registry WHERE available LIMIT ($5::int + 1)) pool) <= $5::int THEN 'pool-floor'
 WHEN NOT (greatest(first_seen_at,coalesce(last_available_at,first_seen_at))<now()-$4::interval
  AND NOT EXISTS(SELECT 1 FROM node_explorer_work WHERE id<>$2 AND payload->>'identity'=$1 AND state='running' AND lease_until>now())
  AND NOT EXISTS(SELECT 1 FROM node_explorer_connectivity WHERE identity=$1 AND checked_at>$3::timestamptz)) THEN 'not-safe'
 ELSE '' END
 FROM node_explorer_node_registry WHERE identity=$1`,
		j.Payload.Identity, j.ID, proof.CheckedAt, intervalParam(cleanupMinAge()), cleanupMinPool()).Scan(&refusal)
	if e != nil {
		return false, "", e
	}
	if refusal != "" {
		return false, refusal, nil
	}
	var raw []byte
	e = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_probe_results WHERE session=$1 AND kind=$2 AND identity=$3`, session, KindLive, j.Payload.Identity).Scan(&raw)
	if e == pgx.ErrNoRows {
		return false, "no-proof", nil
	}
	if e != nil {
		return false, "", e
	}
	var stored domain.ConnectivityState
	if e = json.Unmarshal(raw, &stored); e != nil {
		return false, "", e
	}
	if stored.CheckedAt != proof.CheckedAt || stored.Reachable || !stored.ConfirmedDead {
		return false, "proof-mismatch", nil
	}
	if compact {
		if e = removeSnapshotIdentity(ctx, tx, j.Payload.Identity); e != nil {
			return false, "", e
		}
	}
	for _, q := range []string{
		`DELETE FROM node_explorer_connectivity WHERE identity=$1`,
		`DELETE FROM node_explorer_exits WHERE identity=$1`,
		`DELETE FROM node_explorer_retired_nodes WHERE identity=$1`,
		`DELETE FROM node_explorer_probe_results WHERE identity=$1`,
		`DELETE FROM node_explorer_work WHERE payload->>'identity'=$1 AND kind NOT IN ('attributes','risk')`,
		`DELETE FROM node_explorer_node_registry WHERE identity=$1`,
		`INSERT INTO node_explorer_deleted_nodes(identity,deleted_at) VALUES($1,clock_timestamp()) ON CONFLICT(identity) DO UPDATE SET deleted_at=EXCLUDED.deleted_at`,
	} {
		if _, e = tx.Exec(ctx, q, j.Payload.Identity); e != nil {
			return false, "", e
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO node_explorer_lifecycle_events(identity,action,dry_run,reason) VALUES($1,'delete',false,$2)`,
		j.Payload.Identity, fmt.Sprintf("%s without availability; fresh controlled probe failed", cleanupMinAge()))
	if e != nil {
		return false, "", e
	}
	var legacy bool
	if e = tx.QueryRow(ctx, `SELECT to_regclass('node_explorer_current_nodes') IS NOT NULL`).Scan(&legacy); e != nil {
		return false, "", e
	}
	if legacy {
		if _, e = tx.Exec(ctx, `DELETE FROM node_explorer_current_nodes WHERE identity=$1`, j.Payload.Identity); e != nil {
			return false, "", e
		}
	}
	return true, "", nil
}
func removeSnapshotIdentity(ctx context.Context, tx pgx.Tx, id string) error {
	return removeSnapshotIdentities(ctx, tx, []string{id})
}

// Compact all affected snapshots once, including their archived result indices.
func removeSnapshotIdentities(ctx context.Context, tx pgx.Tx, ids []string) error {
	removed := make(map[string]bool, len(ids))
	for _, id := range ids {
		removed[id] = true
	}
	rows, e := tx.Query(ctx, `SELECT s.id,s.payload FROM node_explorer_snapshots s
 LEFT JOIN node_explorer_registry_batches b ON b.snapshot_id=s.id
 LEFT JOIN node_explorer_results_changes c ON c.snapshot_id=s.id
 WHERE b.snapshot_id IS NULL OR b.revision<>coalesce(c.revision,0)
 OR EXISTS(SELECT 1 FROM node_explorer_snapshot_members m WHERE m.snapshot_id=s.id AND m.identity=ANY($1::text[]))
 ORDER BY s.id`, ids)
	if e != nil {
		return e
	}
	type snapshot struct {
		id  int
		raw []byte
	}
	var all []snapshot
	for rows.Next() {
		var s snapshot
		if e = rows.Scan(&s.id, &s.raw); e != nil {
			rows.Close()
			return e
		}
		all = append(all, s)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, s := range all {
		var payload map[string]json.RawMessage
		if e = json.Unmarshal(s.raw, &payload); e != nil {
			return e
		}
		var nodes []json.RawMessage
		if e = json.Unmarshal(payload["nodes"], &nodes); e != nil {
			return e
		}
		kept := make([]json.RawMessage, 0, len(nodes))
		var oldIndices []int
		for index, raw := range nodes {
			var n domain.Node
			if e = json.Unmarshal(raw, &n); e != nil {
				return e
			}
			if !removed[domain.AggregateIdentity(n)] {
				kept = append(kept, raw)
				oldIndices = append(oldIndices, index)
			}
		}
		if len(kept) == len(nodes) {
			continue
		}
		if _, e = tx.Exec(ctx, `DELETE FROM node_explorer_ippure_results WHERE snapshot_id=$1 AND NOT(node_index=ANY(coalesce($2::int[],'{}')))`, s.id, oldIndices); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE node_explorer_ippure_results SET node_index=-node_index-1 WHERE snapshot_id=$1`, s.id); e != nil {
			return e
		}

		if _, e = tx.Exec(ctx, `UPDATE node_explorer_ippure_results e SET node_index=m.new_index-1
 FROM unnest($2::int[]) WITH ORDINALITY m(old_index,new_index)
 WHERE e.snapshot_id=$1 AND e.node_index=-m.old_index-1`, s.id, oldIndices); e != nil {
			return e
		}
		// Membership is a source index, so it also covers nonwinning historical occurrences.
		if _, e = tx.Exec(ctx, `DELETE FROM node_explorer_snapshot_members WHERE snapshot_id=$1 AND NOT(node_index=ANY(coalesce($2::int[],'{}')))`, s.id, oldIndices); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE node_explorer_snapshot_members SET node_index=-node_index-1 WHERE snapshot_id=$1`, s.id); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE node_explorer_snapshot_members s SET node_index=m.new_index-1
 FROM unnest($2::int[]) WITH ORDINALITY m(old_index,new_index)
 WHERE s.snapshot_id=$1 AND s.node_index=-m.old_index-1`, s.id, oldIndices); e != nil {
			return e
		}

		// Registry indices change together, avoiding compaction collisions.
		if _, e = tx.Exec(ctx, `UPDATE node_explorer_node_registry n SET node_index=m.new_index-1
 FROM unnest($2::int[]) WITH ORDINALITY m(old_index,new_index)
 WHERE n.snapshot_id=$1 AND n.node_index=m.old_index AND NOT(n.identity=ANY($3::text[]))`, s.id, oldIndices, ids); e != nil {
			return e
		}
		payload["nodes"], _ = json.Marshal(kept)
		raw, e := json.Marshal(payload)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE node_explorer_snapshots SET payload=$2,node_count=$3 WHERE id=$1`, s.id, string(raw), len(kept)); e != nil {
			return e
		}
	}
	return nil
}
