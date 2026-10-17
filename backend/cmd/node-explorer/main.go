// Command node-explorer serves current database facts through the HTTP API.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/api"
	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"log"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// ttlCache memoizes a dashboard read for ttl and collapses concurrent misses
// into a single query. Failures are never cached, so a transient fault clears
// on the next request instead of persisting for the whole window. Staleness
// stays below the frontend poll interval by configuration.
type ttlCache[T any] struct {
	mu   sync.Mutex
	ttl  time.Duration
	load func(context.Context) (T, error)
	at   time.Time
	val  T
}

func (c *ttlCache[T]) Get(ctx context.Context) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < c.ttl {
		return c.val, nil
	}
	val, err := c.load(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	c.val, c.at = val, time.Now()
	return val, nil
}

// servicesSource adapts the cached board to the api.WriteServices contract.
type servicesSource struct{ cache *ttlCache[[]service.Status] }

func (s servicesSource) Statuses(ctx context.Context) ([]service.Status, error) {
	return s.cache.Get(ctx)
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
	collectorCache := &ttlCache[api.CollectorStatus]{
		ttl: service.Seconds("API_COLLECTOR_CACHE_SECONDS", 5),
		load: func(ctx context.Context) (api.CollectorStatus, error) {
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			s, e := repo.Status(check, "ip-service")
			if e != nil {
				return api.CollectorStatus{Enabled: true, Error: "服务状态暂不可用"}, e
			}
			return api.CollectorStatus{Enabled: true, Running: s.Healthy, Queued: int(s.Queued), LastRun: s.UpdatedAt.Format(time.RFC3339), Error: s.Error}, nil
		},
	}
	collector := func() api.CollectorStatus {
		s, err := collectorCache.Get(ctx)
		if err != nil {
			return api.CollectorStatus{Enabled: true, Error: "服务状态暂不可用"}
		}
		return s
	}
	servicesCache := &ttlCache[[]service.Status]{
		ttl: service.Seconds("API_SERVICES_CACHE_SECONDS", 10),
		load: func(ctx context.Context) ([]service.Status, error) {
			return repo.Statuses(ctx)
		},
	}
	server := api.New(api.ServerConfig{Store: st, Live: postgres.NewLiveReader(a.Pool()), Collector: collector, DistDir: filepath.Join(root, "dist"), HasPostgres: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, req *http.Request) {
		api.WriteServices(w, req, servicesSource{servicesCache})
	})
	mux.Handle("/", server.Handler())
	return api.Serve(ctx, mux, service.Env("HOST", "127.0.0.1")+":"+service.Env("API_PORT", "3001"))
}
