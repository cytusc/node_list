package service

import (
	"context"
	"time"
)

// Provider quotas survive restarts and are shared across worker replicas.
func (r *Repository) BeforeProvider(ctx context.Context, provider string) error {
	tx, e := r.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `INSERT INTO node_explorer_provider_limits(provider,next_at) VALUES($1,now()) ON CONFLICT DO NOTHING`, provider); e != nil {
		return e
	}
	var next time.Time
	if e = tx.QueryRow(ctx, `SELECT next_at FROM node_explorer_provider_limits WHERE provider=$1 FOR UPDATE`, provider).Scan(&next); e != nil {
		return e
	}
	if time.Until(next) > 2*time.Second {
		return waiting{time.Until(next)}
	}
	if _, e = tx.Exec(ctx, `UPDATE node_explorer_provider_limits SET next_at=greatest(now(),next_at)+interval '1500 milliseconds' WHERE provider=$1`, provider); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	if wait := time.Until(next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (r *Repository) AfterProvider(ctx context.Context, provider string, next time.Time) error {
	_, e := r.Pool.Exec(ctx, `INSERT INTO node_explorer_provider_limits(provider,next_at) VALUES($1,$2) ON CONFLICT(provider) DO UPDATE SET next_at=greatest(node_explorer_provider_limits.next_at,EXCLUDED.next_at)`, provider, next)
	return e
}
