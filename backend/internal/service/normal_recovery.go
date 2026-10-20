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
	return 20 * time.Minute
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

// retryIntervalFor decides how long a completed assessment must wait before its
// next attempt. The first repeat waits one base interval; every further
// recorded failure multiplies it by six up to a daily ceiling, so a node whose
// proofs keep failing cannot exhaust the lane (previously every failure
// re-queued unconditionally after one interval). Deterministic rejections stay
// on their own staged cadence because parser verdicts do not change between
// attempts, and the dead-letter verdict is terminal: the scheduler filters it
// out entirely instead of trusting an interval. normal_recovery.go and the
// prefetch UPDATE below enforce that contract on persisted state.
func retryIntervalFor(result string, attempts int, base time.Duration) time.Duration {
	if result == "config-rejected-dead" {
		return time.Duration(1 << 62) // terminal result; scheduling is gated elsewhere
	}
	if result == "config-rejected" {
		return max(base, 6*time.Hour)
	}
	tiered := base
	for tier := 1; tier < attempts; tier++ {
		next := tiered * 6
		if next/6 != tiered || next > 24*time.Hour {
			return 24 * time.Hour
		}
		tiered = next
	}
	return tiered
}

// Residential transient failures remain with the fast service. Nonhistorical
// nodes and explicit nonresidential/hosting outcomes use this isolated queue.
// A still-running legacy assessment is allowed to finish before handoff.
func (r *Runner) scheduleNormalRecovery(ctx context.Context) error {
	// Shortening the interval must also advance untouched persisted jobs. Never
	// replace a running assessment or its session/generation proofs. Jobs past
	// their first attempt carry a tiered deadline that may legitimately exceed
	// one interval, so only fresh failures are eligible for an early advance.
	if _, err := r.Repo.Pool.Exec(ctx, `UPDATE node_explorer_work SET
      due_at=greatest(now(),coalesce((payload->>'checkedAt')::timestamptz,(payload->>'retiredAt')::timestamptz)+$1::interval),
      generation=generation+1
      WHERE kind='normal-recovery' AND state='queued' AND NOT(payload ? 'startedAt')
      AND coalesce(payload->>'recoveryResult','') NOT IN ('config-rejected','config-rejected-dead')
      AND coalesce((payload->>'recoveryAttempts')::int,0)<=1
      AND due_at>now()+$1::interval`, r.normalRecoveryInterval().String()); err != nil {
		return err
	}

	rows, err := r.Repo.Pool.Query(ctx, `SELECT n.identity,t.retired_at,t.reason,w.id,w.generation,w.state,w.payload
 FROM node_explorer_node_registry n JOIN node_explorer_retired_nodes t USING(identity)
 LEFT JOIN node_explorer_work fast ON fast.kind='recovery' AND fast.key=n.identity||':'||t.retired_at
 LEFT JOIN node_explorer_work w ON w.kind='normal-recovery' AND w.key=n.identity||':'||t.retired_at
 WHERE n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)
 AND (t.reason=$1 OR t.reason LIKE 'IPPure:%')
 AND (fast.id IS NULL OR fast.state='done')
 AND (NOT n.historical_residential OR fast.payload->>'recoveryResult' IN ('not-residential','skipped-datacenter-asn'))
 AND (w.id IS NULL OR (w.state='done' AND w.payload->>'recoveryResult'<>'config-rejected-dead')) ORDER BY t.retired_at,n.identity LIMIT 256`, store.RetireRestoreReason)
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
			if c.payload.RecoveryResult == "config-rejected-dead" {
				// Terminal deterministic rejection: never re-queue. A revived
				// registry entry gets a fresh work key via new generation.
				continue
			}
			// Keep the result/history and start fresh session proofs at the next
			// interval. Repeated failures back off instead of re-queueing every
			// interval unconditionally.
			dueAt := stamp.Add(retryIntervalFor(c.payload.RecoveryResult, c.payload.RecoveryAttempts, interval))
			_, err = r.Repo.Pool.Exec(ctx, `UPDATE node_explorer_work w SET state='queued',generation=generation+1,
    due_at=$4,payload=payload-'startedAt',lease_token=NULL,lease_until=NULL,attempts=0,error=''
    WHERE id=$1 AND generation=$2 AND state='done' AND payload->>'checkedAt'=$3
    AND EXISTS(SELECT 1 FROM node_explorer_retired_nodes t JOIN node_explorer_node_registry n USING(identity)
    WHERE t.identity=w.payload->>'identity' AND t.retired_at=w.payload->>'retiredAt' AND t.reason=w.payload->>'reason'
    AND n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id))`, *c.id, *c.generation, c.payload.CheckedAt, dueAt)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
