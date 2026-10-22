package service

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/semaphore"
)

func (r *Runner) configureAdmission() {
	// Bound complete assessments, including their queued dependencies. Consumer
	// count alone does not bound the number of sessions awaiting probes.
	r.Repo.NormalRecoveryConcurrency = r.laneCount(KindNormalRecovery, false)
	if r.Config.Manager != nil && r.Config.Role == "exit-provision" {
		capacity := r.Config.ExitConcurrency
		if capacity <= 0 {
			capacity = 8
		}
		r.exitSlots = semaphore.NewWeighted(int64(capacity))
	}
}

func managerCapacityKind(kind string) string {
	switch kind {
	case KindExit:
		return "exit"
	case KindLive:
		return "strict"
	case KindRisk:
		return "risk"
	case KindFast:
		return "delay"
	default:
		return ""
	}
}

// Admission precedes both Redis Pop and the durable claim. Weighted's FIFO
// waiters share free slots between priority lanes without starving either one.
// Empty lanes return slots immediately, before the consumer's idle sleep.
func (r *Runner) nextJob(ctx context.Context, kind string, high bool) (*Job, func(), error) {
	release := func() {}
	wait := func() error {
		if r.Config.Manager == nil {
			return ctx.Err()
		}
		if err := r.Config.Manager.WaitReady(ctx); err != nil {
			return err
		}
		return r.Config.Manager.WaitCapacity(ctx, managerCapacityKind(kind))
	}
	if err := wait(); err != nil {
		return nil, nil, err
	}
	if kind == KindExit && r.exitSlots != nil {
		if err := r.exitSlots.Acquire(ctx, 1); err != nil {
			return nil, nil, err
		}
		release = func() { r.exitSlots.Release(1) }
	}
	// A busy response may have arrived while this consumer waited for a slot.
	if err := wait(); err != nil {
		release()
		return nil, nil, err
	}
	// Give short-lived recovery proofs bounded priority over periodic exit
	// maintenance. Every fourth opportunity stays with the ordinary queue.
	if kind == KindExit && r.exitSlots != nil && r.Repo.Pool != nil {
		j, err := r.claimRecoveryExit(ctx, high)
		if err != nil {
			release()
			return nil, nil, err
		}
		if j != nil {
			return j, release, nil
		}
	}
	ref, err := r.Queue.Pop(ctx, kind, high)
	if err != nil || ref == "" {
		release()
		return nil, nil, err
	}
	j, err := r.Repo.Claim(ctx, kind, ref, high)
	if err != nil || j == nil {
		release()
		return nil, nil, err
	}
	return j, release, nil
}

// Selection is serialized per priority lane only until Claim returns, never
// across network probes. Claim still enforces generation, lease and node fences.
type recoveryExitAdmission struct {
	mu    sync.Mutex
	turns uint64
}

func (r *Runner) claimRecoveryExit(ctx context.Context, high bool) (*Job, error) {
	lane := 0
	if high {
		lane = 1
	}
	a := &r.recoveryExits[lane]
	a.mu.Lock()
	defer a.mu.Unlock()
	a.turns++
	if a.turns%4 == 0 {
		return nil, nil
	}
	var ref string
	err := r.Repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work
 WHERE kind='exit' AND state='queued' AND payload->>'session' LIKE 'recovery:%'
 AND (priority>0)=$1 AND due_at<=now() ORDER BY due_at,id LIMIT 1`, high).Scan(&ref)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Durable claims make any duplicate Redis reference harmless. Periodic
	// work continues through Redis; urgent work need not wait behind its batch.
	return r.Repo.Claim(ctx, KindExit, ref, high)
}
