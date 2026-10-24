// Command dead-verify drains the dead-node pool through an independent Mihomo
// instance. The shared kernel's strict lane is a fixed eight leases and each
// strict probe costs two timeouts plus a control node, so a forty-thousand-node
// backlog would take days there. This verifier starts its own Mihomo, loads a
// wave of candidates as named proxies and lets the controller perform one real
// handshake per node — the only way to judge Cloudflare-fronted nodes, whose
// TCP port always accepts connections.
//
// It never deletes on its own authority: every verdict is handed to
// Repository.DeleteVerifiedDead, which re-runs the queue's full gate set
// (fresh proof, exact configuration digest, age window, running-work veto,
// newer-observation veto, registry and snapshot locks).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}

type options struct {
	limit    int
	workers  int
	deleters int
	timeout  time.Duration
	minAge   time.Duration
	recheck  time.Duration
	interval time.Duration
	once     bool
	dryRun   bool
}

func run() error {
	var o options
	flag.IntVar(&o.limit, "limit", envInt("DEAD_VERIFY_LIMIT", 400), "节点每批数量")
	flag.IntVar(&o.workers, "workers", envInt("DEAD_VERIFY_WORKERS", 96), "并行握手数")
	flag.IntVar(&o.deleters, "deleters", envInt("DEAD_VERIFY_DELETERS", 8), "并行删除事务数")
	flag.DurationVar(&o.timeout, "timeout", time.Duration(envInt("DEAD_VERIFY_TIMEOUT_SECONDS", 6))*time.Second, "单节点握手超时")
	flag.DurationVar(&o.minAge, "min-age", time.Duration(envInt("DEAD_VERIFY_MIN_AGE_SECONDS", 86400))*time.Second, "候选年龄窗口")
	flag.DurationVar(&o.recheck, "recheck", time.Duration(envInt("DEAD_VERIFY_RECHECK_SECONDS", 21600))*time.Second, "同一节点再次核验的间隔")
	flag.DurationVar(&o.interval, "interval", time.Duration(envInt("DEAD_VERIFY_INTERVAL_SECONDS", 5))*time.Second, "无候选时的等待")
	flag.BoolVar(&o.once, "once", false, "只跑一批")
	flag.BoolVar(&o.dryRun, "dry-run", false, "只报告判定，不删除")
	flag.Parse()

	service.LoadEnv(service.Root())
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
	repo := &service.Repository{Pool: a.Pool()}

	binary := service.Env("DEAD_VERIFY_MIHOMO", "mihomo")
	instance, shutdown, err := startMihomo(ctx, binary)
	if err != nil {
		return err
	}
	defer shutdown()
	log.Printf("dead-verify: mihomo ready controller=%s limit=%d workers=%d", instance.Controller, o.limit, o.workers)

	for {
		wave, err := repo.VerifiedCandidates(ctx, o.minAge, o.recheck, o.limit)
		if err != nil {
			return err
		}
		if len(wave) == 0 {
			if o.once {
				log.Printf("dead-verify: no candidates")
				return nil
			}
			heartbeat(ctx, repo, map[string]any{"running": true, "idle": true})
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(o.interval):
			}
			continue
		}
		stats, err := verifyWave(ctx, repo, instance, wave, o)
		if err != nil {
			log.Printf("dead-verify: wave failed: %v", err)
		} else {
			log.Printf("dead-verify: wave candidates=%d alive=%d dead=%d deleted=%d vetoed=%d", len(wave), stats.alive, stats.dead, stats.deleted, stats.vetoed)
		}
		heartbeat(ctx, repo, map[string]any{"running": true, "candidates": len(wave), "alive": stats.alive, "dead": stats.dead, "deleted": stats.deleted, "vetoed": stats.vetoed, "dryRun": o.dryRun})
		if o.once {
			return err
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(o.interval):
			}
		}
	}
}

type waveStats struct {
	alive, dead, deleted, vetoed int
}

func verifyWave(ctx context.Context, repo *service.Repository, instance transport.MihomoConfig, wave []service.VerifiedCandidate, o options) (waveStats, error) {
	var stats waveStats
	controls, err := repo.VerificationControls(ctx, 3)
	if err != nil {
		return stats, err
	}
	if len(controls) == 0 {
		return stats, errors.New("no egress control: refusing to judge a wave")
	}
	nodes := make([]domain.Node, 0, len(controls)+len(wave))
	nodes = append(nodes, controls...)
	for _, c := range wave {
		nodes = append(nodes, c.Node)
	}
	names, picked, err := transport.LoadVerificationProxies(ctx, instance, nodes)
	if err != nil {
		return stats, err
	}
	if len(names) != len(picked) || len(names) <= len(controls) {
		return stats, fmt.Errorf("wave did not load: names=%d picked=%d controls=%d", len(names), len(picked), len(controls))
	}

	verdict := make([]int32, len(names)) // 0 unknown, 1 alive, 2 dead
	jobs := make(chan int, len(names))
	var wg sync.WaitGroup
	for w := 0; w < o.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				alive, err := transport.ProxyDelay(ctx, instance, names[i], o.timeout)
				switch {
				case err != nil:
					// Controller trouble says nothing about the node.
					atomic.StoreInt32(&verdict[i], 0)
				case alive:
					atomic.StoreInt32(&verdict[i], 1)
				default:
					atomic.StoreInt32(&verdict[i], 2)
				}
			}
		}()
	}
	for i := range names {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	// The control must answer; otherwise the whole wave is untrustworthy.
	controlAlive := false
	for i := range controls {
		if atomic.LoadInt32(&verdict[i]) == 1 {
			controlAlive = true
			break
		}
	}
	if !controlAlive {
		return stats, errors.New("egress control failed: wave discarded")
	}

	sem := make(chan struct{}, o.deleters)
	var deleted, vetoed, dead, alive atomic.Int32
	var delWG sync.WaitGroup
	for i := len(controls); i < len(names); i++ {
		switch atomic.LoadInt32(&verdict[i]) {
		case 1:
			alive.Add(1)
			continue
		case 2:
			dead.Add(1)
		default:
			continue
		}
		if o.dryRun {
			continue
		}
		c := wave[picked[i]-len(controls)]
		delWG.Add(1)
		go func(c service.VerifiedCandidate) {
			defer delWG.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Reachable: false, ConfirmedDead: true, ProbeTarget: "batch-verify"}
			done, err := repo.DeleteVerifiedDead(ctx, c.Identity, c.Incarnation, c.Digest, proof)
			if err != nil {
				log.Printf("dead-verify: %s delete: %v", c.Identity, err)
				return
			}
			if done {
				deleted.Add(1)
			} else {
				vetoed.Add(1)
			}
		}(c)
	}
	delWG.Wait()
	stats = waveStats{alive: int(alive.Load()), dead: int(dead.Load()), deleted: int(deleted.Load()), vetoed: int(vetoed.Load())}
	return stats, nil
}

// startMihomo runs an independent Mihomo for verification only. It shares no
// controller, no capacity lane and no config with the production kernel.
func startMihomo(ctx context.Context, binary string) (transport.MihomoConfig, func(), error) {
	dir, err := os.MkdirTemp("", "dead-verify-")
	if err != nil {
		return transport.MihomoConfig{}, nil, err
	}
	secret := fmt.Sprintf("dead-verify-%d", time.Now().UnixNano())
	controller := "127.0.0.1:19090"
	config := strings.Join([]string{
		"mixed-port: 17890",
		"allow-lan: false",
		"bind-address: 127.0.0.1",
		"mode: rule",
		"log-level: warning",
		"external-controller: " + controller,
		"secret: " + secret,
		"proxies: []",
		"rules:",
		"  - MATCH,DIRECT",
		"",
	}, "\n")
	path := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(path, []byte(config), 0o600); err != nil {
		return transport.MihomoConfig{}, nil, err
	}
	cmd := exec.Command(binary, "-d", dir, "-f", path)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err = cmd.Start(); err != nil {
		return transport.MihomoConfig{}, nil, fmt.Errorf("start mihomo: %w", err)
	}
	instance := transport.MihomoConfig{Controller: "http://" + controller, Secret: secret}
	shutdown := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = os.RemoveAll(dir)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err = transport.ProxyCount(ctx, instance); err == nil {
			return instance, shutdown, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			shutdown()
			return transport.MihomoConfig{}, nil, errors.New("mihomo did not become ready")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func heartbeat(ctx context.Context, repo *service.Repository, fields map[string]any) {
	payload, err := json.Marshal(fields)
	if err != nil {
		return
	}
	_, err = repo.Pool.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('dead-verify',now(),$1)
 ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, payload)
	if err != nil {
		log.Printf("dead-verify: heartbeat: %v", err)
	}
}
