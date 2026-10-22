package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

func (r *Runner) fast(ctx context.Context, j Job, high bool) error {
	if r.Config.Manager == nil {
		return r.live(ctx, j, high)
	}
	t, err := r.Repo.probeTarget(ctx, j.Payload)
	if err != nil {
		return err
	}
	if t == nil || (j.Payload.Action != "cleanup" && !matchesRetirement(t, j.Payload)) {
		return r.Repo.Finish(ctx, j, nil)
	}
	j.Payload.Incarnation = t.Incarnation
	result, err := r.Config.Manager.Delay(ctx, t.Node, t.Incarnation, high)
	if err != nil {
		if errors.Is(err, transport.ErrManagerBusy) {
			return err
		}
		// Keep the latest infrastructure observation separate from strict proofs.
		raw, _ := json.Marshal(map[string]any{"checkedAt": time.Now().UTC(), "abnormal": true})
		_, _ = r.Repo.Pool.Exec(ctx, `INSERT INTO node_explorer_fast_results(identity,checked_at,payload)
   SELECT identity,clock_timestamp(),$3 FROM node_explorer_node_registry WHERE identity=$1 AND incarnation=$2
   ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, t.Identity, t.Incarnation, raw)
		return err
	} // Infrastructure is retried, never a node failure.
	// Fence the success-to-proof transaction against reload, expiry and replacement.
	var paths map[string]*transport.Outbound
	if result.Success {
		bounded, release, leased, e := r.proofFence(ctx, j.Payload)
		if e != nil {
			return e
		}
		defer release()
		ctx, paths = bounded, leased
	}
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		ok, e := guard(ctx, tx, j.Payload)
		if e != nil || !ok {
			return e
		}
		if e = r.Config.Manager.Generation(ctx, result.Generation); e != nil {
			return e
		}
		raw, _ := json.Marshal(result)
		if _, e = tx.Exec(ctx, `INSERT INTO node_explorer_fast_results(identity,checked_at,payload) VALUES($1,$2,$3)
  ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload
  WHERE node_explorer_fast_results.checked_at<=EXCLUDED.checked_at`, t.Identity, result.CheckedAt, raw); e != nil {
			return e
		}
		if e = writeProbe(ctx, tx, j.Payload.Session, KindFast, t.Identity, result); e != nil {
			return e
		}
		if result.Success {
			if _, e = tx.Exec(ctx, `UPDATE node_explorer_work SET payload=payload||jsonb_build_object('capacityAt',clock_timestamp()) WHERE id=$1 AND generation=$2 AND lease_token=$3 AND NOT(payload ? 'capacityAt')`, j.ID, j.Generation, j.Token); e != nil {
				return e
			}
			if e = validatePaths(ctx, tx, paths); e != nil {
				return e
			}
			v := domain.ConnectivityState{Reachable: true, CheckedAt: result.CheckedAt.UTC().Format(time.RFC3339Nano), KernelGeneration: result.Generation, ProbeTarget: transport.DelayTarget}
			return r.saveConnectivity(ctx, tx, j, t, v)
		}
		// A retired fast failure ends this assessment; deletion always requires strict proof.
		if !result.Success && t.Retired != nil && j.Payload.Action != "cleanup" {
			return nil
		}
		key := j.Payload.Session
		if key == "" {
			key = "fast:" + j.Ref()
		}
		return once(ctx, tx, KindLive, key, j.Payload, j.Priority)
	})
}

// A proof keeps its three-minute freshness budget. Before any proof exists,
// bound the whole wait too: legacy done-without-result dependencies otherwise
// hold an assessment slot forever.
const assessmentQueueTimeout = 10 * time.Minute

func (r *Runner) assessmentTimeout(ctx context.Context, session string, started time.Time) (string, error) {
	if r.Config.Manager == nil {
		if time.Since(started) > 3*time.Minute {
			return "timed-out", nil
		}
		return "", nil
	}
	var stamp *time.Time
	err := r.Repo.Pool.QueryRow(ctx, `SELECT min((payload->>'capacityAt')::timestamptz) FROM node_explorer_work
 WHERE payload->>'session'=$1 AND kind IN ($2,$3) AND payload ? 'capacityAt'`, session, KindLive, KindFast).Scan(&stamp)
	if err != nil {
		return "", err
	}
	if stamp != nil && time.Since(*stamp) > 3*time.Minute {
		return "timed-out", nil
	}
	if stamp == nil && !started.IsZero() && time.Since(started) > assessmentQueueTimeout {
		return "capacity-timeout", nil
	}
	return "", nil
}

func (r *Runner) assessmentExpired(ctx context.Context, session string, started time.Time) (bool, error) {
	reason, err := r.assessmentTimeout(ctx, session, started)
	// Cleanup retains its proof deadline; capacity waiting only triggers a
	// recovery retry, never a cleanup failure.
	return reason == "timed-out", err
}

func validatePaths(ctx context.Context, tx pgx.Tx, paths map[string]*transport.Outbound) error {
	ids := make([]string, 0, len(paths))
	for id := range paths {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		path := paths[id]
		if path == nil || path.Validate == nil {
			continue
		}
		if err := path.Validate(ctx); err != nil {
			return err
		}
		var raw []byte
		var incarnation string
		err := tx.QueryRow(ctx, `SELECT node,incarnation FROM node_explorer_node_registry WHERE identity=$1 FOR UPDATE`, id).Scan(&raw, &incarnation)
		if err != nil {
			return err
		}
		var entry transport.RegistryEntry
		if err = json.Unmarshal(raw, &entry.Node); err != nil {
			return err
		}
		if incarnation != path.Incarnation || transport.ConfigDigest(entry.Node) != path.Digest {
			return ErrLeaseLost
		}
	}
	return nil
}

// Leave a second for cancellation to reach PostgreSQL before a lease expires.
func pathContext(ctx context.Context, paths map[string]*transport.Outbound) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(2 * time.Minute)
	for _, p := range paths {
		if p != nil && !p.ExpiresAt.IsZero() && p.ExpiresAt.Add(-time.Second).Before(deadline) {
			deadline = p.ExpiresAt.Add(-time.Second)
		}
	}
	return context.WithDeadline(ctx, deadline)
}

// Hold a non-network lease until the state transaction commits, fencing reloads.
func (r *Runner) proofFence(ctx context.Context, p Payload) (context.Context, func(), map[string]*transport.Outbound, error) {
	if r.Config.Manager == nil {
		return ctx, func() {}, nil, nil
	}
	t, err := r.Repo.probeTarget(ctx, p)
	if err != nil {
		return ctx, nil, nil, err
	}
	if t == nil {
		return ctx, func() {}, nil, nil
	}
	engine := &transport.ManagedEngine{Manager: r.Config.Manager, Kind: "proof"}
	out, release, err := engine.Reserve(ctx, []domain.Node{t.Node})
	if err != nil {
		return ctx, nil, nil, err
	}
	paths := map[string]*transport.Outbound{t.Identity: out[0]}
	bounded, cancel := pathContext(ctx, paths)
	return bounded, func() { cancel(); release() }, paths, nil
}
