package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/ratelimit"
	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/taskqueue"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	role := flag.String("role", "", "source, exit-provision, exit-attributes, recovery, node-cleanup, mihomo-manager")
	health := flag.Bool("health", false, "check persistent service heartbeat")
	flag.Parse()
	service.LoadEnv(service.Root())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := postgres.ConfigFromEnv()
	if err != nil {
		return err
	}
	if *health {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		a, err := postgres.Connect(check, cfg)
		if err != nil {
			return err
		}
		defer a.Close()
		return (&service.Repository{Pool: a.Pool()}).Healthy(check, service.ServiceName(*role))
	}
	a, err := postgres.NewAdapter(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	repo := &service.Repository{Pool: a.Pool()}
	if *role == "source" {
		st := store.New(a, nil)
		return repo.RunSource(ctx, st)
	}
	if *role == "mihomo-manager" {
		return repo.RunMihomoManager(ctx)
	}
	c, err := service.ConfigFromEnv(*role)
	if err != nil {
		return err
	}
	q, err := taskqueue.New(ctx, os.Getenv("REDIS_URL"), os.Getenv("REDIS_NAMESPACE"))
	if err != nil {
		return err
	}
	defer q.Close()
	// Provider pacing is shared through the same Redis connection, so every
	// replica draws from one GCRA clock instead of one row lock per call.
	repo.Limiter = ratelimit.New(q.Client())
	return (&service.Runner{Repo: repo, Queue: q, Config: c}).Run(ctx)
}
