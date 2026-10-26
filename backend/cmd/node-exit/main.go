// node-exit has no Redis, manager or attribute-service dependency.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/exitprobe"
	"github.com/cytusc/node_list/backend/internal/runtimeenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

func number(key string, def, low, high int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < low || n > high {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return n, nil
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	health := flag.Bool("health", false, "check independent exit heartbeat")
	flag.Parse()
	runtimeenv.LoadEnv(runtimeenv.Root())
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		return fmt.Errorf("invalid PostgreSQL configuration")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("PostgreSQL pool initialization failed")
	}
	defer pool.Close()
	db := &exitprobe.DB{Pool: pool}
	defer db.Close()
	if *health {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return db.Healthy(check)
	}
	workers, err := number("EXIT_WORKERS", 4, 1, 32)
	if err != nil {
		return err
	}
	timeout, err := number("EXIT_TIMEOUT_SECONDS", 25, 5, 120)
	if err != nil {
		return err
	}
	interval, err := number("EXIT_INTERVAL_SECONDS", 1800, 10, 86400)
	if err != nil {
		return err
	}
	retry, err := number("EXIT_RETRY_SECONDS", 300, 10, 86400)
	if err != nil {
		return err
	}
	binary := runtimeenv.Env("MIHOMO_BIN", "mihomo")
	if _, err = exec.LookPath(binary); err != nil {
		return fmt.Errorf("Mihomo binary unavailable")
	}
	acquire, stop := context.WithTimeout(ctx, 10*time.Second)
	err = db.Acquire(acquire)
	stop()
	if err != nil {
		return fmt.Errorf("exit ownership or schema initialization failed")
	}
	duration := time.Duration(timeout) * time.Second
	runner := exitprobe.Runner{Store: db, Prober: exitprobe.ProcessProber{Binary: binary, Timeout: duration}, Workers: workers, Timeout: duration, Interval: time.Duration(interval) * time.Second, Retry: time.Duration(retry) * time.Second}
	if err := runner.Run(ctx); err != nil {
		return fmt.Errorf("exit service stopped after a database failure")
	}
	return nil
}
