// node-source collects and persists nodes without starting any other service.
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/database"
	"github.com/cytusc/node_list/backend/internal/ingestion"
	"github.com/cytusc/node_list/backend/internal/runtimeenv"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	health := flag.Bool("health", false, "check source ingestion heartbeat")
	flag.Parse()
	runtimeenv.LoadEnv(runtimeenv.Root())
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg, err := database.ConfigFromEnv()
	if err != nil {
		return err
	}
	// Schema provisioning belongs to deployment; collection must not run other
	// services' data migrations on startup.
	connect, stop := context.WithTimeout(ctx, 5*time.Second)
	adapter, err := database.Connect(connect, cfg)
	stop()
	if err != nil {
		return err
	}
	defer adapter.Close()
	repo := &ingestion.Repository{Pool: adapter}
	if *health {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return repo.Healthy(check)
	}
	return repo.RunSource(ctx)
}
