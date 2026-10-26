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

	"github.com/cytusc/node_list/backend/internal/liveness"
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
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	health := flag.Bool("health", false, "check independent liveness heartbeat")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// pgx reads standard PG* variables, including TLS configuration. No business
	// adapter or another role's schema migrations are invoked here.
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		return fmt.Errorf("invalid PostgreSQL configuration")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *health {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		var ok bool
		err = pool.QueryRow(check, `SELECT updated_at>now()-interval '30 seconds' FROM node_explorer_service_status WHERE service='node-liveness' AND coalesce(payload->>'error','')='' `).Scan(&ok)
		if err != nil || !ok {
			return fmt.Errorf("liveness heartbeat unavailable")
		}
		return nil
	}
	xw, err := number("LIVENESS_XRAY_WORKERS", 4, 1, 64)
	if err != nil {
		return err
	}
	mw, err := number("LIVENESS_MIHOMO_WORKERS", 4, 1, 64)
	if err != nil {
		return err
	}
	seconds, err := number("LIVENESS_TIMEOUT_SECONDS", 12, 1, 120)
	if err != nil {
		return err
	}
	interval, err := number("LIVENESS_INTERVAL_SECONDS", 1800, 10, 86400)
	if err != nil {
		return err
	}
	retry, err := number("LIVENESS_RETRY_SECONDS", 60, 10, 3600)
	if err != nil {
		return err
	}
	for _, bin := range []string{env("XRAY_BIN", "xray"), env("MIHOMO_BIN", "mihomo")} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("required kernel binary unavailable: %s", bin)
		}
	}
	db := &liveness.DB{Pool: pool}
	defer db.Close()
	acquire, stop := context.WithTimeout(ctx, 10*time.Second)
	err = db.Acquire(acquire)
	stop()
	if err != nil {
		return err
	}
	engine := func(name, bin string) liveness.Engine {
		return liveness.ProcessEngine{Name: name, Binary: bin, Target: liveness.Target, Expected: 204, Timeout: time.Duration(seconds) * time.Second}
	}
	runner := liveness.Runner{Store: db, Xray: engine("xray", env("XRAY_BIN", "xray")), Mihomo: engine("mihomo", env("MIHOMO_BIN", "mihomo")), XrayWorkers: xw, MihomoWorkers: mw, Interval: time.Duration(interval) * time.Second, Retry: time.Duration(retry) * time.Second, Control: liveness.DirectControl}
	return runner.Run(ctx)
}
