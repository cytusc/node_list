package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrServiceRunning = errors.New("service already running")
var ErrServiceLockLost = errors.New("service ownership connection lost")

// withServiceLock uses a dedicated database session: file/PID locks cannot
// exclude a second container, and pooled sessions must never retain this lock.
// The callback must stop and join its workers when its context is cancelled.
func (r *Repository) withServiceLock(parent context.Context, name string, run func(context.Context) error) error {
	cfg := r.Pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "node-explorer/service/" + name
	connect, stop := context.WithTimeout(parent, 5*time.Second)
	conn, err := pgx.ConnectConfig(connect, cfg)
	stop()
	if err != nil {
		return fmt.Errorf("service %s: connect ownership session: %w", name, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = conn.Close(cleanup) // Close releases every session lock, even on errors.
	}()
	acquire, stop := context.WithTimeout(parent, 3*time.Second)
	var locked bool
	err = conn.QueryRow(acquire, `SELECT pg_try_advisory_lock(hashtext(current_schema()),hashtext($1))`, "node-explorer-service:"+name).Scan(&locked)
	stop()
	if err != nil {
		return fmt.Errorf("service %s: acquire ownership: %w", name, err)
	}
	if !locked {
		return fmt.Errorf("%w: %s", ErrServiceRunning, name)
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	monitor := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				monitor <- nil
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 2*time.Second)
				err := conn.Ping(check)
				stop()
				if err != nil {
					if ctx.Err() != nil {
						monitor <- nil
						return
					}
					cancel()
					monitor <- fmt.Errorf("%w: %s", ErrServiceLockLost, name)
					return
				}
			}
		}
	}()
	err = run(ctx)
	cancel()
	if lost := <-monitor; lost != nil && parent.Err() == nil {
		return lost
	}
	return err
}

// WithServiceLock owns a service until the callback and all its workers stop.
func (r *Repository) WithServiceLock(ctx context.Context, name string, run func(context.Context) error) error {
	return r.withServiceLock(ctx, name, run)
}
