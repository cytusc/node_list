// Command github-patrol rechecks every visible node's reachability on a
// schedule, retires confirmed dead connections and revives ones that return.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/patrol"
	"github.com/cytusc/node_list/backend/internal/store"
	pgstore "github.com/cytusc/node_list/backend/internal/store/postgres"
	sqlitestore "github.com/cytusc/node_list/backend/internal/store/sqlite"
	"github.com/cytusc/node_list/backend/internal/transport"
)

// statusFile mirrors the JavaScript service so existing health checks and
// dashboards keep working.
type status struct {
	Enabled          bool                            `json:"enabled"`
	Running          bool                            `json:"running"`
	IntervalSeconds  int                             `json:"intervalSeconds"`
	Error            string                          `json:"error,omitempty"`
	StartedAt        string                          `json:"startedAt,omitempty"`
	CompletedAt      string                          `json:"completedAt,omitempty"`
	NextRunAt        string                          `json:"nextRunAt,omitempty"`
	LastResult       *patrol.Summary                 `json:"lastResult,omitempty"`
	LastRecovery     *patrol.RecoverySummary         `json:"lastRecovery,omitempty"`
	Progress         *patrol.Summary                 `json:"progress,omitempty"`
	RecoveryProgress *patrol.RecoverySummary         `json:"recoveryProgress,omitempty"`
	RecoveryState    map[string]patrol.RecoveryState `json:"recoveryState"`
	UpdatedAt        string                          `json:"updatedAt"`
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("github-patrol: %v", err)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "--health" {
		return healthCheck()
	}
	interval := time.Duration(intBetween("GITHUB_PATROL_INTERVAL_SECONDS", 1800, 60, 86400)) * time.Second
	concurrency := intBetween("GITHUB_PATROL_CONCURRENCY", 8, 1, 64)
	timeout := time.Duration(intBetween("GITHUB_PATROL_TIMEOUT_SECONDS", 12, 3, 60)) * time.Second
	retryDelay := time.Duration(intBetween("GITHUB_PATROL_RETRY_SECONDS", 10, 1, 300)) * time.Second

	controller := os.Getenv("GITHUB_PATROL_CONTROLLER")
	proxyURL := os.Getenv("GITHUB_PATROL_PROXY_URL")
	if controller == "" || proxyURL == "" {
		return fmt.Errorf("GitHub patrol requires a dedicated Mihomo controller and proxy URL")
	}
	if controller == os.Getenv("IPPURE_CONTROLLER") {
		return fmt.Errorf("GitHub patrol must not share the IPPure controller")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st *store.Store
	var adapter store.Adapter
	if os.Getenv("PGHOST") != "" {
		cfg, err := pgstore.ConfigFromEnv()
		if err != nil {
			return err
		}
		pgAdapter, err := pgstore.NewAdapter(ctx, cfg)
		if err != nil {
			return fmt.Errorf("PostgreSQL initialization failed: %w", err)
		}
		adapter = pgAdapter
		st = store.New(pgAdapter, nil)
	} else {
		sqliteAdapter, err := sqlitestore.New(envOr("DATABASE_PATH", "/data/history/nodes.sqlite"))
		if err != nil {
			return fmt.Errorf("sqlite initialization failed: %w", err)
		}
		adapter = sqliteAdapter
		st = store.New(sqliteAdapter, nil)
	}
	_ = adapter
	if err := st.Reload(ctx); err != nil {
		return fmt.Errorf("initial load failed: %w", err)
	}
	defer st.Close()

	controllerList := splitList(controller)
	proxyList := splitList(proxyURL)
	if len(controllerList) != len(proxyList) {
		return fmt.Errorf("GITHUB_PATROL_CONTROLLER and GITHUB_PATROL_PROXY_URL must list the same number of instances")
	}
	instances := make([]transport.MihomoConfig, 0, len(controllerList))
	for i := range controllerList {
		instances = append(instances, transport.MihomoConfig{
			Controller: controllerList[i],
			ProxyURL:   proxyList[i],
			Secret:     os.Getenv("GITHUB_PATROL_CONTROLLER_SECRET"),
		})
	}
	router := transport.NewRouter(transport.NewNativeEngine()).
		WithFallback(transport.NewMihomoEngine(instances, 15*time.Second))
	runner := patrol.NewPatrol(st, router, patrol.PatrolConfig{
		Concurrency: concurrency,
		Timeout:     timeout,
		RetryDelay:  retryDelay,
	})

	path := statusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	current := status{
		Enabled:         true,
		IntervalSeconds: int(interval.Seconds()),
		RecoveryState:   map[string]patrol.RecoveryState{},
	}
	if raw, err := os.ReadFile(path); err == nil {
		var previous status
		if err := json.Unmarshal(raw, &previous); err == nil && previous.RecoveryState != nil {
			current.RecoveryState = previous.RecoveryState
		}
	}
	writer := &statusWriter{path: path}
	persist := func() { writer.update(current) }
	persist()

	heartbeatDone := make(chan struct{})
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	go func() {
		defer close(heartbeatDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				writer.heartbeat()
			}
		}
	}()

	log.Printf("github-patrol: interval=%s concurrency=%d timeout=%s", interval, concurrency, timeout)
	for !isDone(ctx) {
		nextDelay := interval
		current.Running = true
		current.StartedAt = time.Now().UTC().Format(time.RFC3339)
		current.NextRunAt = ""
		current.Progress = nil
		current.Error = ""
		persist()
		summary, err := runner.RunOnce(ctx, func(progress patrol.Summary) {
			current.Progress = &progress
			persist()
		})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			current.Error = "巡检未完成：数据库、代理配置或网络异常，未确认的节点不做软删除"
			nextDelay = minDuration(interval, time.Minute)
			log.Printf("github-patrol: %s (%v)", current.Error, err)
		} else {
			current.LastResult = &summary
			recovery, err := runner.RecoverRetiredNodes(ctx, current.RecoveryState,
				time.Now(), 30*time.Minute, func(progress patrol.RecoverySummary) {
					current.RecoveryProgress = &progress
					persist()
				})
			if err != nil && ctx.Err() == nil {
				log.Printf("github-patrol: recovery failed: %v", err)
			} else {
				current.LastRecovery = &recovery
			}
			current.CompletedAt = time.Now().UTC().Format(time.RFC3339)
			log.Printf("github-patrol: complete %+v recovery %+v", summary, recovery)
		}
		current.Running = false
		current.NextRunAt = time.Now().Add(nextDelay).UTC().Format(time.RFC3339)
		persist()
		if !sleep(ctx, nextDelay) {
			break
		}
	}
	stop()
	<-heartbeatDone
	current.Running = false
	persist()
	return nil
}

func isDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// statusWriter owns a detached copy. The heartbeat never reads the mutable
// patrol state or its recovery map, and all atomic file replacements serialize.
type statusWriter struct {
	mu      sync.Mutex
	path    string
	current status
}

func (w *statusWriter) update(current status) {
	raw, err := json.Marshal(current)
	if err != nil {
		log.Printf("github-patrol: marshal status failed: %v", err)
		return
	}
	var detached status
	if err := json.Unmarshal(raw, &detached); err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.current = detached
	w.writeLocked()
}

func (w *statusWriter) heartbeat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeLocked()
}

func (w *statusWriter) writeLocked() {
	w.current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	writeStatus(w.path, w.current)
}

// writeStatus persists atomically; callers serialize writes through statusWriter.
func writeStatus(path string, payload status) {
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("github-patrol: marshal status failed: %v", err)
		return
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".patrol-status-*")
	if err != nil {
		log.Printf("github-patrol: status write failed: %v", err)
		return
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err = file.Write(raw); err != nil {
		file.Close()
		log.Printf("github-patrol: status write failed: %v", err)
		return
	}
	if err = file.Close(); err != nil {
		log.Printf("github-patrol: status close failed: %v", err)
		return
	}
	if err = os.Rename(tmp, path); err != nil {
		log.Printf("github-patrol: status rename failed: %v", err)
	}
}

func statusPath() string {
	if v := os.Getenv("GITHUB_PATROL_STATUS_FILE"); v != "" {
		return v
	}
	dir := os.Getenv("DATABASE_PATH")
	if dir == "" {
		dir = "/data/history/nodes.sqlite"
	}
	return filepath.Join(filepath.Dir(dir), "github-patrol-status.json")
}

// healthCheck mirrors the container health check: the service is healthy while
// it has written a status file recently.
func healthCheck() error {
	raw, err := os.ReadFile(statusPath())
	if err != nil {
		os.Exit(1)
	}
	var payload status
	if err := json.Unmarshal(raw, &payload); err != nil {
		os.Exit(1)
	}
	updated, err := time.Parse(time.RFC3339, payload.UpdatedAt)
	if err != nil || time.Since(updated) > 180*time.Second {
		os.Exit(1)
	}
	os.Exit(0)
	return nil
}

func intBetween(name string, fallback, min, max int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < min || parsed > max {
		log.Printf("github-patrol: %s must be between %d and %d, using %d", name, min, max, fallback)
		return fallback
	}
	return parsed
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
