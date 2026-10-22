package service

import (
	"context"

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
