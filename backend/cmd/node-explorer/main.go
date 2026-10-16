// Command node-explorer serves current database facts through the HTTP API.
package main

import (
	"context"
	"github.com/cytusc/node_list/backend/internal/api"
	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"log"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	root := service.Root()
	service.LoadEnv(root)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := postgres.ConfigFromEnv()
	if err != nil {
		return err
	}
	a, err := postgres.NewAdapter(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()
	st := store.New(a, nil)
	repo := &service.Repository{Pool: a.Pool()}
	if err := repo.Heartbeat(ctx, "api", ""); err != nil {
		return err
	}
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = repo.Heartbeat(ctx, "api", "")
				var warning string
				if err := a.Pool().QueryRow(ctx, `SELECT coalesce(payload->>'sourceWarning','') FROM node_explorer_service_status WHERE service='source-ingest'`).Scan(&warning); err == nil {
					st.SetSourceWarning(warning)
				}
			}
		}
	}()
	collector := func() api.CollectorStatus {
		check, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		s, e := repo.Status(check, "ip-service")
		if e != nil {
			return api.CollectorStatus{Enabled: true, Error: "服务状态暂不可用"}
		}
		return api.CollectorStatus{Enabled: true, Running: s.Healthy, Queued: int(s.Queued), LastRun: s.UpdatedAt.Format(time.RFC3339), Error: s.Error}
	}
	server := api.New(api.ServerConfig{Store: st, Live: postgres.NewLiveReader(a.Pool()), Collector: collector, DistDir: filepath.Join(root, "dist"), HasPostgres: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, req *http.Request) { api.WriteServices(w, req, repo) })
	mux.Handle("/", server.Handler())
	return api.Serve(ctx, mux, service.Env("HOST", "127.0.0.1")+":"+service.Env("API_PORT", "3001"))
}
