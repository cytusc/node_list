// node-attributes fills missing metadata using only PostgreSQL and provider HTTP APIs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/attributes"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
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
	health := flag.Bool("health", false, "check independent attributes heartbeat")
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
	db := &attributes.DB{Pool: pool}
	defer db.Close()
	if *health {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return db.Healthy(check)
	}
	workers, err := number("ATTR_WORKERS", 3, 1, 32)
	if err != nil {
		return err
	}
	timeout, err := number("ATTR_TIMEOUT_SECONDS", 45, 5, 120)
	if err != nil {
		return err
	}
	retry, err := number("ATTR_RETRY_SECONDS", 1800, 10, 86400)
	if err != nil {
		return err
	}
	acquire, stop := context.WithTimeout(ctx, 10*time.Second)
	err = db.Acquire(acquire)
	stop()
	if err != nil {
		return fmt.Errorf("attributes ownership or schema initialization failed")
	}
	duration := time.Duration(timeout) * time.Second
	classification := ipcheck.NewClient(os.Getenv("IPINFO_TOKEN"), os.Getenv("IP_API_PRO_KEY"), os.Getenv("IPREGISTRY_API_KEY"), os.Getenv("ABUSEIPDB_API_KEY"))
	classification.Providers = ipcheck.ProvidersFromEnv()
	if raw := os.Getenv("IP_PROVIDERS"); raw != "" && classification.Providers == nil {
		return fmt.Errorf("IP_PROVIDERS contains no supported provider")
	}
	// Classification failures only affect in-memory pacing, not persisted data.
	classification.BeforeProvider = db.BeforeProvider
	client := attributes.NewClient(classification, runtimeenv.Env("IPPURE_API_ORIGIN", "https://api.123169.xyz"))
	client.BeforeProvider, client.AfterProvider = db.BeforeProvider, db.AfterProvider
	runner := attributes.Runner{Store: db, Lookup: client, Workers: workers, Timeout: duration, Retry: time.Duration(retry) * time.Second}
	if err := runner.Run(ctx); err != nil {
		return fmt.Errorf("attributes service stopped after a database failure")
	}
	return nil
}
