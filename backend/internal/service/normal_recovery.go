package service

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"time"

	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

func (r *Runner) normalRecoveryInterval() time.Duration {
	if r.Config.NormalRecoveryInterval > 0 {
		return r.Config.NormalRecoveryInterval
	}
	return 6 * time.Hour
}

// Spread an old backlog over five minutes, persisted once per retirement. Fresh
// retirements wait at least a full low-frequency interval before first admission.
func normalRecoveryFirstDue(identity, retiredAt string, now time.Time, interval time.Duration) time.Time {
	due := now
	if retired, err := time.Parse(time.RFC3339Nano, retiredAt); err == nil && retired.Add(interval).After(due) {
		due = retired.Add(interval)
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(identity + ":" + retiredAt))
	spread := min(5*time.Minute, interval)
	return due.Add(time.Duration(hash.Sum64() % uint64(spread)))
}

// Residential transient failures remain with the fast service. Nonhistorical
// nodes and explicit nonresidential/hosting outcomes use this isolated queue.
// A still-running legacy assessment is allowed to finish before handoff.
func (r *Runner) scheduleNormalRecovery(ctx context.Context) error {
	rows, err := r.Repo.Pool.Query(ctx, `SELECT n.identity,t.retired_at,t.reason,w.id,w.generation,w.state,w.payload
 FROM node_explorer_node_registry n JOIN node_explorer_retired_nodes t USING(identity)
 LEFT JOIN node_explorer_work fast ON fast.kind='recovery' AND fast.key=n.identity||':'||t.retired_at
 LEFT JOIN node_explorer_work w ON w.kind='normal-recovery' AND w.key=n.identity||':'||t.retired_at
 WHERE n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)
 AND (t.reason=$1 OR t.reason LIKE 'IPPure:%')
 AND (fast.id IS NULL OR fast.state='done')
 AND (NOT n.historical_residential OR fast.payload->>'recoveryResult' IN ('not-residential','skipped-datacenter-asn'))
 AND (w.id IS NULL OR w.state='done') ORDER BY t.retired_at,n.identity LIMIT 256`, store.RetireRestoreReason)
	if err != nil {
		return err
	}
	type candidate struct {
		identity, retiredAt, reason string
		id, generation              *int64
		state                       *string
		payload                     Payload
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var raw []byte
		if err = rows.Scan(&c.identity, &c.retiredAt, &c.reason, &c.id, &c.generation, &c.state, &raw); err != nil {
			rows.Close()
			return err
		}
		if len(raw) > 0 {
			if err = json.Unmarshal(raw, &c.payload); err != nil {
				rows.Close()
				return err
			}
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	now := time.Now()
	interval := r.normalRecoveryInterval()
	for _, c := range candidates {
		if c.id == nil {
			p := Payload{Identity: c.identity, RetiredAt: c.retiredAt, Reason: c.reason, RecoveryMode: "normal"}
			err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
				return onceDue(ctx, tx, KindNormalRecovery, c.identity+":"+c.retiredAt, p, normalRecoveryFirstDue(c.identity, c.retiredAt, now, interval), 0)
			})
		} else {
			stamp, e := time.Parse(time.RFC3339Nano, c.payload.CheckedAt)
			if e != nil {
				continue
			}
			// Keep the result/history and start fresh session proofs at the next interval.
			_, err = r.Repo.Pool.Exec(ctx, `UPDATE node_explorer_work w SET state='queued',generation=generation+1,
    due_at=$4,payload=payload-'startedAt',lease_token=NULL,lease_until=NULL,attempts=0,error=''
    WHERE id=$1 AND generation=$2 AND state='done' AND payload->>'checkedAt'=$3
    AND EXISTS(SELECT 1 FROM node_explorer_retired_nodes t JOIN node_explorer_node_registry n USING(identity)
    WHERE t.identity=w.payload->>'identity' AND t.retired_at=w.payload->>'retiredAt' AND t.reason=w.payload->>'reason'
    AND n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id))`, *c.id, *c.generation, c.payload.CheckedAt, stamp.Add(interval))
		}
		if err != nil {
			return err
		}
	}
	return nil
}
