// Command node-explorer serves the results API, imports detection rounds and
// runs the IPPure collector.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/api"
	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/qilan"
	"github.com/cytusc/node_list/backend/internal/source"
	"github.com/cytusc/node_list/backend/internal/store"
	pgstore "github.com/cytusc/node_list/backend/internal/store/postgres"
	sqlitestore "github.com/cytusc/node_list/backend/internal/store/sqlite"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("node-explorer: %v", err)
	}
}

type config struct {
	subDir       string
	resultsFile  string
	databasePath string
	distDir      string
	addr         string

	ippureInstances []transport.MihomoConfig
	ippureSecret    string
	ippureOrigin    string
	concurrency     int
	collectInterval time.Duration

	qilanSession     string
	qilanSessionFile string
	qilanUserAgent   string
	qilanInterval    time.Duration
	feishuWebhook    string

	refreshInterval time.Duration
	pg              *pgstore.Config
}

func loadConfig(root string) (config, error) {
	cfg := config{
		subDir:          envOr("SUBS_CHECK_DIR", filepath.Join(root, "data", "sub")),
		databasePath:    envOr("DATABASE_PATH", filepath.Join(root, "data", "nodes.sqlite")),
		distDir:         filepath.Join(root, "dist"),
		addr:            envOr("HOST", "127.0.0.1") + ":" + envOr("API_PORT", "3001"),
		ippureOrigin:    envOr("IPPURE_API_ORIGIN", "https://api.123169.xyz"),
		concurrency:     envInt("IPPURE_CONCURRENCY", 3),
		collectInterval: time.Duration(envInt("IPPURE_INTERVAL_SECONDS", 30)) * time.Second,
		qilanSession:    os.Getenv("QILAN_SESSION"),
		qilanUserAgent:  envOr("QILAN_USER_AGENT", "Mozilla/5.0"),
		qilanInterval:   time.Duration(envInt("QILAN_REFRESH_INTERVAL_SECONDS", 900)) * time.Second,
		feishuWebhook:   os.Getenv("FEISHU_WEBHOOK_URL"),
		refreshInterval: time.Duration(envInt("PROJECTION_REFRESH_SECONDS", 10)) * time.Second,
	}
	if cfg.concurrency < 1 || cfg.concurrency > 64 {
		return cfg, fmt.Errorf("IPPURE_CONCURRENCY must be between 1 and 64")
	}
	if cfg.collectInterval <= 0 || cfg.refreshInterval <= 0 {
		return cfg, fmt.Errorf("collector and projection intervals must be positive")
	}
	if cfg.qilanInterval < time.Minute {
		cfg.qilanInterval = time.Minute
	}
	cfg.resultsFile = os.Getenv("RESULTS_FILE")
	if cfg.resultsFile == "" {
		cfg.resultsFile = filepath.Join(cfg.subDir, "config", "cache", "config", "results.json")
	}
	cfg.qilanSessionFile = envOr("QILAN_SESSION_FILE",
		"/mnt/d/code/qilan/qilan-scraper/session.txt")
	cfg.ippureSecret = os.Getenv("IPPURE_CONTROLLER_SECRET")

	// Multiple Mihomo instances let the next wave reload an idle instance while
	// the current wave's probes are still finishing, which removes the reload
	// from the critical path. A single instance keeps the previous behaviour.
	controllers := splitList(os.Getenv("IPPURE_CONTROLLER"))
	proxyURLs := splitList(os.Getenv("IPPURE_PROXY_URL"))
	if len(controllers) != len(proxyURLs) {
		return cfg, fmt.Errorf("IPPURE_CONTROLLER and IPPURE_PROXY_URL must list the same number of instances")
	}
	for i := range controllers {
		cfg.ippureInstances = append(cfg.ippureInstances, transport.MihomoConfig{
			Controller: controllers[i],
			ProxyURL:   proxyURLs[i],
			Secret:     cfg.ippureSecret,
		})
	}
	if os.Getenv("PGHOST") != "" {
		pgCfg, err := pgstore.ConfigFromEnv()
		if err != nil {
			return cfg, err
		}
		cfg.pg = &pgCfg
	}
	return cfg, nil
}

// projectRoot locates the directory holding dist/ and .env. The container
// working directory comes first, then its parent so `go -C backend run` resolves
// the repository root, then the container image layout as a last resort.
func projectRoot() string {
	candidates := make([]string, 0, 3)
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd, filepath.Dir(wd))
	}
	candidates = append(candidates, "/app")
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "dist")); err == nil {
			return dir
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return "."
}

func run() error {
	root := projectRoot()
	loadDotEnv(filepath.Join(root, ".env"))
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var adapter store.Adapter
	var projector *pgstore.Projector
	var pgAdapter *pgstore.Adapter
	if cfg.pg != nil {
		pgAdapter, err = pgstore.NewAdapter(ctx, *cfg.pg)
		if err != nil {
			return fmt.Errorf("PostgreSQL initialization failed: %w", err)
		}
		adapter = pgAdapter
		log.Printf("node-explorer: using PostgreSQL %s@%s:%d/%s",
			cfg.pg.User, cfg.pg.Host, cfg.pg.Port, cfg.pg.Database)
	} else {
		sqliteAdapter, err := sqlitestore.New(cfg.databasePath)
		if err != nil {
			return fmt.Errorf("sqlite initialization failed: %w", err)
		}
		adapter = sqliteAdapter
		log.Printf("node-explorer: using SQLite %s", cfg.databasePath)
	}
	st := store.New(adapter, nil)
	if err := st.Reload(ctx); err != nil {
		return fmt.Errorf("initial load failed: %w", err)
	}
	if cfg.pg != nil {
		projector, err = pgstore.NewProjectorForAdapter(ctx, pgAdapter, st, pgstore.ProjectorConfig{})
		if err != nil {
			return fmt.Errorf("projection initialization failed: %w", err)
		}
		st.SetRefresh(projector.Refresh)
		if err := projector.Refresh(ctx); err != nil {
			log.Printf("node-explorer: initial projection refresh failed: %v", err)
		}
	}

	ingestor := store.NewIngestor(st)
	linksFile := filepath.Join(cfg.subDir, "output", "base64.txt")
	clashFile := filepath.Join(cfg.subDir, "output", "all.yaml")

	mihomo := transport.NewMihomoEngine(cfg.ippureInstances, 15*time.Second)
	router := transport.NewRouter(transport.NewNativeEngine(), mihomo).WithFallback(mihomo)

	var scheduler *collector.Scheduler
	collectorStatus := func() api.CollectorStatus {
		if scheduler == nil {
			return api.CollectorStatus{Enabled: false}
		}
		status := scheduler.Status()
		return api.CollectorStatus{
			Enabled:     status.Enabled,
			Running:     status.Running,
			Concurrency: status.Concurrency,
			Transport:   status.Transport,
			Queued:      status.Queued,
			Probed:      status.Probed,
			LastRun:     status.LastRun,
			Error:       status.Error,
		}
	}
	if len(cfg.ippureInstances) > 0 {
		scheduler, err = collector.NewScheduler(st, router, collector.SchedulerConfig{
			Concurrency: cfg.concurrency,
			Interval:    cfg.collectInterval,
			APIOrigin:   cfg.ippureOrigin,
		})
		if err != nil {
			return err
		}
		go runCollector(ctx, scheduler, cfg.collectInterval)
	}

	// Ingestion is event driven. The watcher replaces the five-second poll the
	// JavaScript service used, which rebuilt the projection on every tick.
	ingest := func(ctx context.Context) error {
		result, err := ingestor.ImportFile(ctx, cfg.resultsFile, clashFile, linksFile)
		if err != nil {
			// A results file that has not been published yet is the normal
			// state before subs-check finishes its first round, not a read
			// failure. The distinction is visible in the results payload, so it
			// has to match the old service.
			st.SetSourceWarning(store.SourceWarningFor(err))
			return err
		}
		st.SetSourceWarning(store.SourceWarningNone)
		if !result.Skipped {
			log.Printf("node-explorer: imported detection round (%d nodes)", resultNodes(st))
		}
		return st.Refresh(ctx)
	}
	watcher := source.NewWatcher([]string{cfg.resultsFile, clashFile, linksFile},
		source.WatcherConfig{Quiet: 5 * time.Second, Sweep: 2 * time.Minute, Ingest: ingest})
	// Run one round before serving. The JavaScript service synchronised on
	// every request, so its very first response already carried the real source
	// status; without this the API would report "no warning" until the watcher
	// or the poll fallback fired, which is a visible difference. Doing it once at
	// startup keeps that guarantee without restoring the per-request cost.
	if err := ingest(ctx); err != nil {
		log.Printf("node-explorer: initial ingest did not complete: %v", err)
	}
	if err := watcher.Start(ctx); err != nil {
		log.Printf("node-explorer: file watcher unavailable, falling back to polling: %v", err)
		go pollIngest(ctx, watcher, cfg)
	}

	if cfg.qilanSession != "" || fileExists(cfg.qilanSessionFile) {
		qilanSource := qilan.NewSource(st, ingestor, qilan.SourceConfig{
			Session:     cfg.qilanSession,
			SessionFile: cfg.qilanSessionFile,
			UserAgent:   cfg.qilanUserAgent,
			Interval:    cfg.qilanInterval,
			Webhook:     cfg.feishuWebhook,
		})
		qilanSource.Start(ctx)
		defer qilanSource.Close()
	}

	go refreshLoop(ctx, st, cfg.refreshInterval)

	server := api.New(api.ServerConfig{
		Store:       st,
		Projector:   projector,
		Collector:   collectorStatus,
		DistDir:     cfg.distDir,
		HasPostgres: cfg.pg != nil,
	})
	log.Printf("Node Explorer: http://%s\nResults file: %s", cfg.addr, cfg.resultsFile)
	if err := api.Serve(ctx, server.Handler(), cfg.addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return st.Close()
}

// refreshLoop coalesces projection rebuilds. Every collector write bumps the
// source revision, so rebuilding on notification would mean rebuilding
// continuously; a fixed floor bounds the cost while keeping results fresh.
func refreshLoop(ctx context.Context, st *store.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.Refresh(ctx); err != nil {
				log.Printf("node-explorer: projection refresh failed: %v", err)
			}
		}
	}
}

func runCollector(ctx context.Context, scheduler *collector.Scheduler, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := scheduler.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("node-explorer: collection round failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollIngest is the fallback when inotify is unavailable (some bind mounts do
// not propagate events).
func pollIngest(ctx context.Context, watcher *source.Watcher, cfg config) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			watcherTrigger(watcher, ctx)
		}
	}
}

func watcherTrigger(w *source.Watcher, ctx context.Context) {
	// Reuse the debounce path so polling and watching share one code path.
	w.Trigger(ctx)
}

func resultNodes(st *store.Store) int {
	if snapshot := st.Aggregate(false); snapshot != nil {
		return len(snapshot.Nodes)
	}
	return 0
}

func loadDotEnv(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return parsed
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
