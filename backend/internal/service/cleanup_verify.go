package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// vetoError reports which gate refused a deletion, so the batch verifier can
// tell a transient refusal from a systematic one.
type vetoError struct{ reason string }

func (e *vetoError) Error() string { return "gate veto: " + e.reason }

// VerifiedCandidate is one node a batch verifier may test.
type VerifiedCandidate struct {
	Identity    string
	Incarnation string
	Digest      string
	Node        domain.Node
}

// VerificationSession names the evidence a batch verifier writes for one node.
// Deriving it from the identity keeps concurrent waves from overwriting each
// other's proof and lets the deletion gate reload exactly the row it was given.
// The SQL side spells the same expression: 'dead-verify:'||md5(identity).
func VerificationSession(identity string) string {
	sum := md5.Sum([]byte(identity))
	return "dead-verify:" + hex.EncodeToString(sum[:])
}

// DeleteVerifiedDead applies the queue's deletion gates to a verdict produced
// outside the queue. The proof must be fresh (under a minute) and the digest
// must name the exact configuration that was tested; the registry lock, the
// exclusive snapshot locks, the age window, the running-work veto and the
// newer-observation veto are the same code that guards the queue path.
//
// It never decides death by itself: the caller supplies a proof that already
// says ConfirmedDead, and every gate re-checks that claim.
func (r *Repository) DeleteVerifiedDead(ctx context.Context, identity, incarnation, digest string, proof domain.ConnectivityState) (bool, error) {
	// The deletion transaction takes an exclusive lock on the snapshot tables,
	// which ingestion and the queue's own deletions also hold. Queue behind it
	// briefly instead of eating the 30-second session statement timeout, and let
	// the caller's next wave retry a node that lost the race.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var deleted bool
		deleted, err = r.deleteVerifiedDeadOnce(ctx, identity, incarnation, digest, proof)
		if err == nil || errors.As(err, new(*vetoError)) {
			return deleted, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return false, err
}

func (r *Repository) deleteVerifiedDeadOnce(ctx context.Context, identity, incarnation, digest string, proof domain.ConnectivityState) (bool, error) {
	session := VerificationSession(identity)
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='3s'`); err != nil {
		return false, err
	}
	if err = writeProbe(ctx, tx, session, KindLive, identity, proof); err != nil {
		return false, err
	}
	j := Job{Kind: KindCleanup, Payload: Payload{Identity: identity, Incarnation: incarnation, Reason: digest, Action: "cleanup"}}
	// Bulk deletions skip snapshot compaction: rewriting a snapshot per node
	// costs O(snapshot) and holds the exclusive table lock for seconds. The
	// tombstone already stops the node from coming back; CompactDeleted reclaims
	// the payload space in batches.
	deleted, reason, err := deleteUnavailable(ctx, tx, j, proof, session, false)
	if err == nil && !deleted {
		return false, &vetoError{reason: reason}
	}
	if err != nil {
		return false, err
	}
	// A refusal leaves the node for the queue path, but the observation still
	// commits: without it every wave would retest the same head against the same
	// gate and never reach the rest of the pool.
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return deleted, nil
}

// VerifiedCandidates lists the nodes a batch verifier may test: available,
// currently unreachable, older than the window and without a running probe.
// Nodes verified within recheck are skipped so successive waves cover the pool
// instead of retesting the same head forever.
func (r *Repository) VerifiedCandidates(ctx context.Context, minAge, recheck time.Duration, limit int) ([]VerifiedCandidate, error) {
	rows, err := r.Pool.Query(ctx, `SELECT n.identity,n.incarnation,md5(n.node::text),n.node
 FROM node_explorer_node_registry n
 JOIN node_explorer_connectivity c USING(identity)
 WHERE n.available AND c.payload->>'reachable'='false'
 AND greatest(n.first_seen_at,coalesce(n.last_available_at,n.first_seen_at))<now()-$1::interval
 AND NOT EXISTS(SELECT 1 FROM node_explorer_work w WHERE w.payload->>'identity'=n.identity AND w.state='running' AND w.lease_until>now())
 AND NOT EXISTS(SELECT 1 FROM node_explorer_probe_results p WHERE p.session='dead-verify:'||md5(n.identity) AND p.checked_at>now()-$2::interval)
 ORDER BY n.first_seen_at,n.identity LIMIT $3`, intervalParam(minAge), intervalParam(recheck), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VerifiedCandidate{}
	for rows.Next() {
		var c VerifiedCandidate
		var raw []byte
		if err = rows.Scan(&c.Identity, &c.Incarnation, &c.Digest, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &c.Node); err != nil {
			return nil, fmt.Errorf("decode node %s: %w", c.Identity, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// VerificationControls returns recently reachable nodes that every wave keeps
// as an egress control. If none of them answers, the wave's failures say
// nothing about the candidates and no verdict may be applied.
func (r *Repository) VerificationControls(ctx context.Context, limit int) ([]domain.Node, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := r.Pool.Query(ctx, `SELECT n.node FROM node_explorer_node_registry n
 JOIN node_explorer_connectivity c USING(identity)
 WHERE n.available AND c.payload->>'reachable'='true' AND c.checked_at>now()-interval '30 minutes'
 ORDER BY c.checked_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Node{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var n domain.Node
		if err = json.Unmarshal(raw, &n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CompactDeleted rewrites the snapshots that still carry tombstoned identities.
// Bulk deletions skip that rewrite so each removal stays O(1); this pass pays
// the O(snapshot) cost once for a whole batch instead of once per node. It is
// idempotent — a compacted identity no longer joins — so repeated calls drain
// the residue.
func (r *Repository) CompactDeleted(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		limit = 200
	}
	var pending bool
	if err := r.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_deleted_nodes d
 JOIN node_explorer_snapshot_members m ON m.identity=d.identity)`).Scan(&pending); err != nil {
		return 0, err
	}
	if !pending {
		return 0, nil
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// Payload rewrites are rare here, so waiting for the fold to finish beats
	// failing the batch.
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='20s'`); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT d.identity FROM node_explorer_deleted_nodes d
 JOIN node_explorer_snapshot_members m ON m.identity=d.identity LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE node_explorer_snapshots,node_explorer_ippure_results IN EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	if err = removeSnapshotIdentities(ctx, tx, ids); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}
