package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/taskqueue"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Runner struct {
	Repo           *Repository
	Queue          taskqueue.Queue
	Config         Config
	Classification *ipcheck.Client
	asnCache       *collector.ASNCache
}

func (r *Runner) kinds() []string {
	switch r.Config.Role {
	case "exit-provision":
		return []string{KindExit}
	case "exit-attributes":
		return []string{KindAttributes, KindRisk}
	// The recovery role absorbs the former residential, normal-recovery and
	// lifecycle workers (REFACTOR_PLAN §2). Strict sessions re-use fast/live
	// and exit probes as dependencies, so the role consumes those kinds too.
	case "recovery":
		return []string{KindRecovery, KindNormalRecovery, KindFast, KindLive, KindLifecycle}
	case "node-cleanup":
		return []string{KindCleanup}
	case "lifecycle":
		return []string{KindLifecycle}
	}
	return nil
}

// laneCount returns the consumer count for one kind and priority lane. Slow
// lanes carry an explicit per-kind count (Config.LaneConcurrency); probe lanes
// use the shared lane concurrency.
func (r *Runner) laneCount(kind string, high bool) int {
	if kind == KindFast {
		if high {
			return 8
		}
		return 32
	}
	if n, ok := r.Config.LaneConcurrency[kind]; ok && n > 0 {
		return n
	}
	if high {
		return r.Config.HighConcurrency
	}
	return r.Config.NormalConcurrency
}

func (r *Runner) Run(ctx context.Context) error {
	if len(r.kinds()) == 0 {
		return fmt.Errorf("unknown worker role")
	}
	return r.Repo.withServiceLock(ctx, ServiceName(r.Config.Role), r.run)
}

func (r *Runner) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Repo.NormalRecoveryConcurrency = r.laneCount(KindNormalRecovery, false)
	if r.Config.Manager != nil {
		r.Repo.NormalRecoveryConcurrency = 128
	}
	kinds := r.kinds()
	if len(kinds) == 0 {
		return fmt.Errorf("unknown worker role")
	}
	if r.Classification == nil {
		r.Classification = ipcheck.NewClient(os.Getenv("IPINFO_TOKEN"), os.Getenv("IP_API_PRO_KEY"), os.Getenv("IPREGISTRY_API_KEY"), os.Getenv("ABUSEIPDB_API_KEY"))
	}
	if r.asnCache == nil {
		r.asnCache = collector.NewASNCacheWithTTL(6 * time.Hour)
	}
	r.Classification.BeforeProvider = r.Repo.BeforeProvider
	r.Classification.AfterProvider = r.Repo.AfterProvider
	// One session holds cross-process controller ownership for both lanes.
	var conn *pgxpool.Conn
	var err error
	if r.Config.Manager == nil && isExitRole(r.Config.Role) {
		if r.Repo.Pool.Config().MaxConns < 2 {
			return fmt.Errorf("probe services require PGPOOL_MAX >= 2")
		}
		conn, err = r.Repo.Pool.Acquire(ctx)
		if err != nil {
			return err
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = conn.Conn().Close(cleanup)
			conn.Release()
		}()
	}
	prefix := ""
	if isExitRole(r.Config.Role) {
		prefix = "IP_SERVICE"
	}
	if r.Config.Manager == nil && isExitRole(r.Config.Role) {
		for _, lane := range []string{"HIGH", "NORMAL"} {
			for _, controller := range strings.Split(os.Getenv(prefix+"_"+lane+"_CONTROLLER"), ",") {
				if controller == "" {
					continue
				}
				var locked bool
				if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext(current_schema()),hashtext($1))`, strings.TrimRight(strings.TrimSpace(controller), "/")).Scan(&locked); err != nil {
					return err
				}
				if !locked {
					return fmt.Errorf("Mihomo controller already owned by another service")
				}
			}
		}
	}
	var wg sync.WaitGroup
	for _, kind := range kinds {
		for _, high := range []bool{true, false} {
			if (kind == KindNormalRecovery || kind == KindCleanup) && high {
				continue
			}
			count := r.laneCount(kind, high)
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(kind string, high bool) { defer wg.Done(); r.consume(ctx, kind, high) }(kind, high)
			}
		}
	}
	defer func() { cancel(); wg.Wait() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastSchedule := time.Time{}
	lastHeartbeat := time.Time{}
	lastMessage := ""
	for {
		message := ""
		if conn != nil {
			check, stop := context.WithTimeout(ctx, 3*time.Second)
			err := conn.Ping(check)
			stop()
			if err != nil {
				return fmt.Errorf("controller ownership connection lost")
			}
		}
		if time.Since(lastSchedule) >= r.Config.ScheduleInterval {
			if err := r.schedule(ctx); err != nil {
				message = "任务调度暂时失败"
			}
			lastSchedule = time.Now()
		}
		if err := r.Repo.Publish(ctx, r.Queue, kinds); err != nil {
			message = "Redis 任务投递暂时失败，保留持久化待投递记录"
		}
		if time.Since(lastHeartbeat) >= 10*time.Second || message != lastMessage {
			if err := r.Repo.Heartbeat(ctx, ServiceName(r.Config.Role), message); err != nil {
				if ctx.Err() == nil {
					log.Printf("%s: heartbeat failed", r.Config.Role)
				}
			} else {
				lastHeartbeat, lastMessage = time.Now(), message
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// isExitRole reports the roles split out of the former single ip-service worker.
func isExitRole(role string) bool {
	return role == "exit-provision" || role == "exit-attributes"
}

func ServiceName(role string) string {
	switch role {
	case "source":
		return "source-ingest"
	default:
		return role
	}
}

type waiting struct{ delay time.Duration }

func (w waiting) Error() string { return "awaiting dependency" }

func configRejectedPattern() string {
	return fmt.Sprintf("node_config_rejected:share-link-v%d:%%", domain.ShareLinkVersion)
}

func configRejectedMessage() string {
	return fmt.Sprintf("node_config_rejected:share-link-v%d: 节点配置损坏或检测器暂不支持，暂停频繁重试", domain.ShareLinkVersion)
}

// Poll coordinators less often as they wait, without spending their execution
// deadline on queue time. Keep the cap below proof freshness windows.
func dependencyWait(attempts int) waiting {
	return waiting{time.Duration(min(max(attempts, 1), 15)) * time.Second}
}

func taskFailureReason(err error) string {
	for _, known := range []error{transport.ErrConfigRejected, transport.ErrConfigChanged, transport.ErrManagerUnavailable, context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	var manager *transport.ManagerHTTPError
	if errors.As(err, &manager) {
		return manager.Error()
	}
	var database *pgconn.PgError
	if errors.As(err, &database) {
		return "PostgreSQL SQLSTATE " + database.Code
	}
	// Arbitrary error text may include share links, SQL values or proxy URLs.
	return fmt.Sprintf("failure type %T", err)
}

func (r *Runner) consume(ctx context.Context, kind string, high bool) {
	idleDelay := 250 * time.Millisecond
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		ref, err := r.Queue.Pop(ctx, kind, high)
		if err != nil || ref == "" {
			timer.Reset(idleDelay)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			idleDelay = min(2*time.Second, idleDelay*2)
			continue
		}
		idleDelay = 250 * time.Millisecond
		j, err := r.Repo.Claim(ctx, kind, ref, high)
		if err != nil || j == nil {
			continue
		}
		taskCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = r.handle(taskCtx, *j, high)
		cancel()
		if err == nil || errors.Is(err, ErrLeaseLost) {
			continue
		}
		delay := time.Duration(min(j.Attempts, 30)) * 10 * time.Second
		message := "检测暂时失败，等待重试"
		var pending waiting
		if errors.Is(err, transport.ErrConfigRejected) {
			delay = 6 * time.Hour
			message = configRejectedMessage()
			log.Printf("%s: task %s: %s; retry in %s", kind, ref, taskFailureReason(err), delay)
		} else if errors.Is(err, transport.ErrManagerBusy) {
			delay = time.Second
			message = "等待内核执行容量"
		} else if errors.As(err, &pending) {
			delay = pending.delay
			message = "等待前置检测结果"
		} else {
			message += ": " + taskFailureReason(err)
			log.Printf("%s: task %s failed: %s; retry in %s", kind, ref, taskFailureReason(err), delay)
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		if retryErr := r.Repo.Retry(cleanup, *j, delay, message); retryErr != nil {
			log.Printf("%s: task %s retry persistence failed: %s", kind, ref, taskFailureReason(retryErr))
		}
		stop()
	}
}

func (r *Runner) handle(ctx context.Context, j Job, high bool) error {
	switch j.Kind {
	case KindFast:
		return r.fast(ctx, j, high)
	case KindLive:
		return r.live(ctx, j, high)
	case KindExit:
		return r.exit(ctx, j, high)
	case KindAttributes:
		return r.attributes(ctx, j)
	case KindRisk:
		return r.risk(ctx, j, high)
	case KindRecovery, KindNormalRecovery:
		return r.recovery(ctx, j)
	case KindCleanup:
		return r.cleanup(ctx, j)
	case KindLifecycle:
		return r.lifecycle(ctx, j)
	}
	return r.Repo.Finish(ctx, j, nil)
}

func (r *Repository) Heartbeat(ctx context.Context, name, message string) error {
	b, _ := json.Marshal(map[string]any{"error": message, "running": true})
	_, err := r.Pool.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES($1,now(),$2)
	ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, name, b)
	return err
}

func (r *Repository) Healthy(ctx context.Context, name string) error {
	var ok bool
	err := r.Pool.QueryRow(ctx, `SELECT updated_at>now()-interval '90 seconds' AND coalesce(payload->>'error','')='' FROM node_explorer_service_status WHERE service=$1`, name).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service heartbeat stale or degraded")
	}
	return nil
}
