package service

import (
	"context"
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"time"
)

// Imports never refresh the monotonic successful observation clock.
func recordAvailable(ctx context.Context, tx pgx.Tx, id, incarnation, stamp string) error {
	_, err := tx.Exec(ctx, `UPDATE node_explorer_node_registry SET last_available_at=greatest(last_available_at,$3::timestamptz)
 WHERE identity=$1 AND incarnation=$2 AND $3::timestamptz<=clock_timestamp()`, id, incarnation, stamp)
	return err
}
func (r *Repository) probeTarget(ctx context.Context, p Payload) (*Target, error) {
	if p.Action != "cleanup" {
		return r.Target(ctx, p.Identity)
	}
	t := &Target{Identity: p.Identity}
	var raw []byte
	err := r.Pool.QueryRow(ctx, `SELECT node,incarnation FROM node_explorer_node_registry
 WHERE identity=$1 AND incarnation=$2 AND md5(node::text)=$3`, p.Identity, p.Incarnation, p.Reason).Scan(&raw, &t.Incarnation)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(raw, &t.Node)
	return t, err
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
func (r *Runner) scheduleCleanup(ctx context.Context) error {
	_, err := r.Repo.Pool.Exec(ctx, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at)
 SELECT 'node-cleanup',n.identity||':'||n.incarnation,
 jsonb_build_object('identity',n.identity,'incarnation',n.incarnation,'reason',md5(n.node::text),'action','cleanup'),0,now()
 FROM node_explorer_node_registry n
 WHERE greatest(n.first_seen_at,coalesce(n.last_available_at,n.first_seen_at))<now()-interval '7 days'
 AND NOT EXISTS(SELECT 1 FROM node_explorer_work w WHERE w.kind='node-cleanup' AND w.key=n.identity||':'||n.incarnation)
 ORDER BY greatest(n.first_seen_at,coalesce(n.last_available_at,n.first_seen_at)),n.identity LIMIT 256
 ON CONFLICT(kind,key) DO NOTHING`)
	return err
}
func (r *Runner) cleanup(ctx context.Context, j Job) error {
	var eligible bool
	err := r.Repo.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_node_registry n
 WHERE identity=$1 AND incarnation=$2 AND greatest(first_seen_at,coalesce(last_available_at,first_seen_at))<now()-interval '7 days')`, j.Payload.Identity, j.Payload.Incarnation).Scan(&eligible)
	if err != nil {
		return err
	}
	deferCycle := func() error {
		return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE node_explorer_work w SET payload=jsonb_set(w.payload,'{reason}',to_jsonb(md5(n.node::text))) FROM node_explorer_node_registry n WHERE w.id=$1 AND n.identity=$2 AND n.incarnation=$3`, j.ID, j.Payload.Identity, j.Payload.Incarnation)
			if e != nil {
				return e
			}
			return recur(ctx, tx, j, time.Hour)
		})
	}
	expired, e := r.assessmentExpired(ctx, "cleanup:"+j.Ref(), j.Payload.StartedAt)
	if e != nil {
		return e
	}
	if !eligible || expired {
		return deferCycle()
	}
	session := "cleanup:" + j.Ref()
	p := j.Payload
	p.Session = session
	var busy bool
	err = r.Repo.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_work WHERE payload->>'identity'=$1 AND id<>$2
 AND state<>'done' AND (state='running' OR payload ? 'session' OR kind IN ('recovery','normal-recovery') AND payload ? 'startedAt') AND key<>$3)`, p.Identity, j.ID, session).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return waiting{30 * time.Second}
	}
	if err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		kind := KindLive
		if r.Config.Manager != nil {
			kind = KindFast
		}
		return once(ctx, tx, kind, session, p, 0)
	}); err != nil {
		return err
	}
	var proof domain.ConnectivityState
	ready, err := r.Repo.Probe(ctx, session, KindLive, &proof)
	if err != nil {
		return err
	}
	if !ready {
		return waiting{time.Second}
	}
	if proof.Reachable || !proof.ConfirmedDead || !fresh(proof.CheckedAt, time.Minute) {
		return deferCycle()
	}
	ctx, release, paths, err := r.proofFence(ctx, j.Payload)
	if err != nil {
		return err
	}
	defer release()
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		// Match deletion/source lock order before locking the registry proof row.
		if r.Config.Manager != nil {
			if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); e != nil {
				return e
			}
		}
		if e := validatePaths(ctx, tx, paths); e != nil {
			return e
		}
		if r.Config.Manager != nil {
			if e := r.Config.Manager.Generation(ctx, proof.KernelGeneration); e != nil {
				return e
			}
		}
		if r.Config.DryRun {
			_, e := tx.Exec(ctx, `INSERT INTO node_explorer_lifecycle_events(identity,action,dry_run,reason) VALUES($1,'delete',true,'validation mode: deletion suppressed')`, j.Payload.Identity)
			if e != nil {
				return e
			}
			return recur(ctx, tx, j, time.Hour)
		}
		deleted, e := deleteUnavailable(ctx, tx, j, proof)
		if e != nil {
			return e
		}
		if !deleted {
			return recur(ctx, tx, j, time.Hour)
		}
		return nil
	})
}

// The registry lock fences successful commits. Active probes and config changes veto deletion.
func deleteUnavailable(ctx context.Context, tx pgx.Tx, j Job, proof domain.ConnectivityState) (bool, error) {
	if proof.Reachable || !proof.ConfirmedDead || !fresh(proof.CheckedAt, time.Minute) {
		return false, nil
	}
	if _, e := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); e != nil {
		return false, e
	}
	if _, e := tx.Exec(ctx, `LOCK TABLE node_explorer_snapshots,node_explorer_ippure_results IN EXCLUSIVE MODE`); e != nil {
		return false, e
	}
	ok, e := cleanupGuard(ctx, tx, j.Payload)
	if e != nil || !ok {
		return false, e
	}
	var admitted bool
	if e = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext(current_schema()),hashtext($1))`, "node-probe:"+j.Payload.Identity).Scan(&admitted); e != nil || !admitted {
		return false, e
	}
	var safe bool
	e = tx.QueryRow(ctx, `SELECT greatest(first_seen_at,coalesce(last_available_at,first_seen_at))<now()-interval '7 days'
 AND NOT EXISTS(SELECT 1 FROM node_explorer_work WHERE id<>$2 AND payload->>'identity'=$1 AND state='running' AND lease_until>now())
 AND NOT EXISTS(SELECT 1 FROM node_explorer_connectivity WHERE identity=$1 AND checked_at>$3::timestamptz)
 FROM node_explorer_node_registry WHERE identity=$1`, j.Payload.Identity, j.ID, proof.CheckedAt).Scan(&safe)
	if e != nil || !safe {
		return false, e
	}
	var raw []byte
	e = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_probe_results WHERE session=$1 AND kind=$2 AND identity=$3`, "cleanup:"+j.Ref(), KindLive, j.Payload.Identity).Scan(&raw)
	if e == pgx.ErrNoRows {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	var stored domain.ConnectivityState
	if e = json.Unmarshal(raw, &stored); e != nil {
		return false, e
	}
	if stored.CheckedAt != proof.CheckedAt || stored.Reachable || !stored.ConfirmedDead {
		return false, nil
	}
	if e = removeSnapshotIdentity(ctx, tx, j.Payload.Identity); e != nil {
		return false, e
	}
	for _, q := range []string{
		`DELETE FROM node_explorer_connectivity WHERE identity=$1`,
		`DELETE FROM node_explorer_exits WHERE identity=$1`,
		`DELETE FROM node_explorer_retired_nodes WHERE identity=$1`,
		`DELETE FROM node_explorer_probe_results WHERE identity=$1`,
		`DELETE FROM node_explorer_work WHERE payload->>'identity'=$1 AND kind NOT IN ('attributes','risk')`,
		`DELETE FROM node_explorer_node_registry WHERE identity=$1`,
		`INSERT INTO node_explorer_deleted_nodes(identity,deleted_at) VALUES($1,clock_timestamp()) ON CONFLICT(identity) DO UPDATE SET deleted_at=EXCLUDED.deleted_at`,
		`INSERT INTO node_explorer_lifecycle_events(identity,action,dry_run,reason) VALUES($1,'delete',false,'7 days without availability; fresh controlled probe failed')`,
	} {
		if _, e = tx.Exec(ctx, q, j.Payload.Identity); e != nil {
			return false, e
		}
	}
	var legacy bool
	if e = tx.QueryRow(ctx, `SELECT to_regclass('node_explorer_current_nodes') IS NOT NULL`).Scan(&legacy); e != nil {
		return false, e
	}
	if legacy {
		if _, e = tx.Exec(ctx, `DELETE FROM node_explorer_current_nodes WHERE identity=$1`, j.Payload.Identity); e != nil {
			return false, e
		}
	}
	return true, nil
}
func removeSnapshotIdentity(ctx context.Context, tx pgx.Tx, id string) error {
	rows, e := tx.Query(ctx, `SELECT s.id,s.payload FROM node_explorer_snapshots s
 LEFT JOIN node_explorer_registry_batches b ON b.snapshot_id=s.id
 LEFT JOIN node_explorer_results_changes c ON c.snapshot_id=s.id
 WHERE b.snapshot_id IS NULL OR b.revision<>coalesce(c.revision,0)
 OR EXISTS(SELECT 1 FROM node_explorer_snapshot_members m WHERE m.snapshot_id=s.id AND m.identity=$1)
 ORDER BY s.id`, id)
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
			if domain.AggregateIdentity(n) != id {
				kept = append(kept, raw)
				oldIndices = append(oldIndices, index)
			}
		}
		if len(kept) == len(nodes) {
			continue
		}
		if _, e = tx.Exec(ctx, `DELETE FROM node_explorer_ippure_results WHERE snapshot_id=$1 AND NOT(node_index=ANY($2::int[]))`, s.id, oldIndices); e != nil {
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
		if _, e = tx.Exec(ctx, `DELETE FROM node_explorer_snapshot_members WHERE snapshot_id=$1 AND NOT(node_index=ANY($2::int[]))`, s.id, oldIndices); e != nil {
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
 WHERE n.snapshot_id=$1 AND n.node_index=m.old_index AND n.identity<>$3`, s.id, oldIndices, id); e != nil {
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
