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
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/taskqueue"
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
	case "connectivity":
		return []string{KindLive}
	case "ip":
		return []string{KindExit, KindAttributes, KindRisk}
	case "residential":
		return []string{KindRecovery}
	case "normal-recovery":
		return []string{KindNormalRecovery}
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
	if n, ok := r.Config.LaneConcurrency[kind]; ok && n > 0 {
		return n
	}
	if high {
		return r.Config.HighConcurrency
	}
	return r.Config.NormalConcurrency
}

func (r *Runner) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	if r.Config.Role == "ip" || r.Config.Role == "connectivity" {
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
	prefix := "CONNECTIVITY"
	if r.Config.Role == "ip" {
		prefix = "IP_SERVICE"
	}
	if r.Config.Role == "ip" || r.Config.Role == "connectivity" {
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

func ServiceName(role string) string {
	switch role {
	case "source":
		return "source-ingest"
	case "ip":
		return "ip-service"
	case "residential":
		return "residential-recovery"
	case "lifecycle":
		return "node-lifecycle"
	default:
		return role
	}
}

type waiting struct{ delay time.Duration }

func (w waiting) Error() string { return "awaiting dependency" }

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
		if errors.As(err, &pending) {
			delay = pending.delay
			message = "等待前置检测结果"
		} else {
			log.Printf("%s: task %s failed; retrying", kind, ref)
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		_ = r.Repo.Retry(cleanup, *j, delay, message)
		stop()
	}
}

func (r *Runner) handle(ctx context.Context, j Job, high bool) error {
	switch j.Kind {
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
