// Command node-explorer serves the API and read projection only.
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
	if err = st.Reload(ctx); err != nil {
		return err
	}
	p, err := postgres.NewProjectorForAdapter(ctx, a, st, postgres.ProjectorConfig{})
	if err != nil {
		return err
	}
	st.SetRefresh(p.Refresh)
	if err = st.Refresh(ctx); err != nil {
		return err
	}
	repo := &service.Repository{Pool: a.Pool()}
	if err := repo.Heartbeat(ctx, "api", ""); err != nil {
		return err
	}
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				message := ""
				if err := st.Refresh(ctx); err != nil {
					log.Print("API projection refresh failed")
					message = "当前节点投影暂时不可用"
				}
				_ = repo.Heartbeat(ctx, "api", message)
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
		all, e := repo.Statuses(check)
		if e != nil {
			return api.CollectorStatus{Enabled: true, Error: "服务状态暂不可用"}
		}
		for _, s := range all {
			if s.Service == "ip-service" {
				return api.CollectorStatus{Enabled: true, Running: s.Healthy, Queued: int(s.Queued), LastRun: s.UpdatedAt.Format(time.RFC3339), Error: s.Error}
			}
		}
		return api.CollectorStatus{Enabled: false}
	}
	server := api.New(api.ServerConfig{Store: st, Projector: p, Collector: collector, DistDir: filepath.Join(root, "dist"), HasPostgres: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, req *http.Request) { api.WriteServices(w, req, repo) })
	mux.Handle("/", server.Handler())
	return api.Serve(ctx, mux, service.Env("HOST", "127.0.0.1")+":"+service.Env("API_PORT", "3001"))
}
