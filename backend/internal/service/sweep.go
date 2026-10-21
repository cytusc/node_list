package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

// The sweeper absorbs the old per-node fast/live scheduling (REFACTOR_PLAN §3):
// one round covers the whole registry through the shared kernel's delay
// endpoint, and the round commit is the single durable write.

type sweepObservation struct {
	entry transport.RegistryEntry
	delay transport.DelayResult
}

// The sweeper shares the manager's controller lease; each probe borrows it
// through m.Delay with bounded concurrency. Run entries without a snapshot are
// skipped the same way schedulable nodes were.
func (r *Repository) sweepEntries(ctx context.Context) ([]transport.RegistryEntry, error) {
	rows, err := r.Pool.Query(ctx, `SELECT n.identity,n.incarnation,n.node FROM node_explorer_node_registry n
 WHERE n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id) ORDER BY n.identity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []transport.RegistryEntry{}
	for rows.Next() {
		var entry transport.RegistryEntry
		var raw []byte
		if err = rows.Scan(&entry.Identity, &entry.Incarnation, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &entry.Node); err != nil {
			return nil, err
		}
		entry.Digest = transport.ConfigDigest(entry.Node)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// sweepAll probes every registered entry concurrently; results only accumulate
// in memory. A single infra failure retries whole-next round, and the node
// counts the delayed round as a failure (§3.1 step 2-3).
func (r *Repository) sweepAll(ctx context.Context, m *transport.Manager, entries []transport.RegistryEntry, concurrency int) []sweepObservation {
	if concurrency < 1 {
		concurrency = 32
	}
	results := make([]sweepObservation, len(entries))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range entries {
		wg.Add(1)
		go func(i int, entry transport.RegistryEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			select {
			case <-ctx.Done():
				return
			default:
			}
			probe, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			result, err := m.Delay(probe, transport.LeaseRequest{Targets: []transport.RegistryEntry{entry}})
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				result.Success = false
				if result.CheckedAt.IsZero() {
					result.CheckedAt = time.Now().UTC()
				}
			}
			results[i] = sweepObservation{entry: entry, delay: result}
		}(i, entries[i])
	}
	wg.Wait()
	return results
}

// sweepCommit persists one round in one transaction: kernel probe results,
// connectivity failure accumulation, retirement on the third confirmed loss
// (guard review retained), and the round's recovery markers for retired nodes
// that passed this round (§3.1 steps 4-6).
func (r *Repository) sweepCommit(ctx context.Context, m *transport.Manager, results []sweepObservation) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	passed := make([]string, 0, len(results))
	for _, res := range results {
		if res.entry.Identity == "" {
			continue
		}
		var raw []byte
		var previous domain.ConnectivityState
		err = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_connectivity WHERE identity=$1 FOR UPDATE`, res.entry.Identity).Scan(&raw)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if len(raw) > 0 && json.Unmarshal(raw, &previous) != nil {
			previous = domain.ConnectivityState{}
		}
		if res.delay.CheckedAt.IsZero() {
			res.delay.CheckedAt = time.Now().UTC()
		}
		probe, e := json.Marshal(res.delay)
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_fast_results(identity,checked_at,payload) VALUES($1,$2,$3)
		 ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload
		 WHERE node_explorer_fast_results.checked_at<=EXCLUDED.checked_at`, res.entry.Identity, res.delay.CheckedAt.UTC(), probe); err != nil {
			return err
		}
		current := accumulateFailures(previous, domain.ConnectivityState{Reachable: res.delay.Success, ConfirmedDead: !res.delay.Success,
			CheckedAt: res.delay.CheckedAt.UTC().Format(time.RFC3339Nano), ProbeTarget: transport.DelayTarget})
		state, e := json.Marshal(current)
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,$2,$3)
		 ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload
		 WHERE node_explorer_connectivity.checked_at<=EXCLUDED.checked_at`, res.entry.Identity, res.delay.CheckedAt.UTC(), state); err != nil {
			return err
		}
		if res.delay.Success {
			passed = append(passed, res.entry.Identity)
			continue
		}
		if current.ConfirmedDead && current.FailedRounds >= retirementFailedRounds {
			if e = r.sweepRetire(ctx, tx, m, res, current); e != nil {
				return e
			}
		}
	}
	if len(passed) == 0 {
		return tx.Commit(ctx)
	}
	// Step 6: a retired node that passed the round re-promotes one strict
	// recovery marker (idempotent once), same conditions as the former
	// residential promotion scan.
	ids := make([]map[string]string, 0, len(passed))
	for _, id := range passed {
		ids = append(ids, map[string]string{"identity": id})
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT t.identity,t.retired_at,t.reason FROM node_explorer_retired_nodes t
	 JOIN node_explorer_node_registry n ON n.identity=t.identity
	 JOIN jsonb_to_recordset($1::jsonb) AS x(identity text) ON x.identity=t.identity
	 WHERE n.available AND n.historical_residential
	 AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)
	 AND (t.reason=$2 OR t.reason LIKE 'IPPure:%')
	 AND NOT EXISTS(SELECT 1 FROM node_explorer_work w WHERE w.kind=$3 AND w.key=t.identity||':'||t.retired_at AND w.state<>'done')
	 AND NOT EXISTS(SELECT 1 FROM node_explorer_work w WHERE w.kind='normal-recovery' AND w.key=t.identity||':'||t.retired_at AND w.state<>'done')`, b, store.RetireRestoreReason, KindRecovery)
	if err != nil {
		return err
	}
	type marker struct{ identity, retiredAt, reason string }
	markers := []marker{}
	for rows.Next() {
		var v marker
		if err = rows.Scan(&v.identity, &v.retiredAt, &v.reason); err != nil {
			rows.Close()
			return err
		}
		markers = append(markers, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, v := range markers {
		p := Payload{Identity: v.identity, RetiredAt: v.retiredAt, Reason: v.reason}
		if err = once(ctx, tx, KindRecovery, v.identity+":"+v.retiredAt, p, 1); err != nil {
			return err
		}
	}
	// Step 7: nodes without a reachable exit stay queued for exit work; nodes
	// with an IP keep their attribute/risk top-ups owned by the exit service.
	rows, err = tx.Query(ctx, `SELECT n.identity,e.ip,a.payload FROM node_explorer_node_registry n
	 JOIN jsonb_to_recordset($1::jsonb) AS x(identity text) ON x.identity=n.identity
	 LEFT JOIN node_explorer_exits e ON e.identity=n.identity
	 LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
	 WHERE n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`, b)
	if err != nil {
		return err
	}
	type route struct {
		identity string
		ip       *string
		attr     []byte
	}
	var routes []route
	for rows.Next() {
		var v route
		if err = rows.Scan(&v.identity, &v.ip, &v.attr); err != nil {
			rows.Close()
			return err
		}
		routes = append(routes, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	now := time.Now()
	for _, v := range routes {
		if v.ip == nil || *v.ip == "" {
			if err = once(ctx, tx, KindExit, v.identity, Payload{Identity: v.identity}, 0); err != nil {
				return err
			}
			continue
		}
		var attr domain.IPPureResult
		if len(v.attr) > 0 && json.Unmarshal(v.attr, &attr) != nil {
			attr = domain.IPPureResult{}
		}
		if attr.Classification == nil || attr.Classification.State == "" {
			if err = once(ctx, tx, KindAttributes, *v.ip, Payload{IP: *v.ip, Identity: v.identity}, 0); err != nil {
				return err
			}
		}
		if missingTrafficRatio(attr, now) {
			p := Payload{IP: *v.ip, Identity: v.identity}
			if err = once(ctx, tx, KindRisk, *v.ip, p, 0); err != nil {
				return err
			}
			if err = scheduleCompletedIP(ctx, tx, KindRisk, *v.ip, p, 0); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("mihomo-manager: sweep round committed %d observations (%d passed)", len(results), len(passed))
	return nil
}

// sweepRetire promotes a third consecutive confirmed loss to retirement inside
// the round transaction. The registry row lock and the lifecycle event keep the
// audit trail identical to the queue-driven retire flow.
func (r *Repository) sweepRetire(ctx context.Context, tx pgx.Tx, m *transport.Manager, res sweepObservation, current domain.ConnectivityState) error {
	// A kernel reload between the probe and the commit invalidates the round's
	// evidence; the next round re-arbitrates.
	if err := m.Generation(ctx, res.delay.Generation); err != nil {
		return nil
	}
	p := Payload{Identity: res.entry.Identity, Incarnation: res.entry.Incarnation, CheckedAt: current.CheckedAt}
	ok, err := guard(ctx, tx, p)
	if err != nil || !ok {
		return err
	}
	dryRun := Env("LIFECYCLE_DRY_RUN", "false") == "true"
	if !dryRun {
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_retired_nodes(identity,retired_at,reason) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			res.entry.Identity, time.Now().UTC().Format(time.RFC3339Nano), store.RetireRestoreReason); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_lifecycle_events(identity,action,dry_run,reason) VALUES($1,$2,$3,$4)`,
		res.entry.Identity, "retire", dryRun, store.RetireRestoreReason)
	return err
}
