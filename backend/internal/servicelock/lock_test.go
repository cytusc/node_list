package servicelock

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
)

func startLockedService(t *testing.T, r *pgxpool.Pool, name string) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, r, name, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()
	t.Cleanup(cancel)
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("start service: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("service did not start")
	}
	return cancel, done
}

func awaitService(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("service did not stop")
		return nil
	}
}

func TestServiceLockRejectsEveryDuplicateBeforeStartingWork(t *testing.T) {
	f := setup(t)
	// Use an independent pool to model another process/container.
	pool, err := pgxpool.NewWithConfig(context.Background(), f.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	other := pool
	for _, role := range []string{"source-ingest", "test-service"} {
		t.Run(role, func(t *testing.T) {
			cancel, done := startLockedService(t, f, role)
			err := Run(context.Background(), other, role, func(context.Context) error {
				t.Fatal("duplicate started work")
				return nil
			})
			if !errors.Is(err, ErrServiceRunning) {
				t.Fatalf("duplicate was not rejected before work: %v", err)
			}
			cancel()
			if err := awaitService(t, done); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := Run(context.Background(), other, role, func(context.Context) error { called = true; return nil }); err != nil || !called {
				t.Fatalf("could not take over after exit: %v", err)
			}
		})
	}
}

func TestDifferentServicesCanRunTogetherAndErrorsReleaseLocks(t *testing.T) {
	f := setup(t)
	cancel, done := startLockedService(t, f, "connectivity")
	sentinel := errors.New("worker failed")
	for i := 0; i < 2; i++ {
		if err := Run(context.Background(), f, "ip-service", func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatal("other role blocked or failed callback leaked lock", err)
		}
	}
	cancel()
	if err := awaitService(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestLostServiceLockCancelsWorkAndAllowsTakeover(t *testing.T) {
	f := setup(t)
	name := fmt.Sprintf("loss-test-%d", time.Now().UnixNano())
	_, done := startLockedService(t, f, name)
	var killed bool
	err := f.QueryRow(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name=$1`, "node-explorer/service/"+name).Scan(&killed)
	if err != nil || !killed {
		t.Fatalf("terminate ownership session: %v", err)
	}
	if err := awaitService(t, done); !errors.Is(err, ErrServiceLockLost) {
		t.Fatalf("lost lock did not stop worker: %v", err)
	}
	if err := Run(context.Background(), f, name, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("crashed session left stale lock: %v", err)
	}
}

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := testutil.Postgres(t)
	pool, err := pgxpool.New(context.Background(), cfg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
