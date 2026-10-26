package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

// RunMihomoManager owns a dedicated session lock for the controller, including
// while reloads drain requests. Losing that session cancels the HTTP server.
// Shared kernel ownership is limited to exit and risk requests.
func (r *Repository) RunMihomoManager(parent context.Context) error {
	return r.withServiceLock(parent, "mihomo-manager", func(ctx context.Context) error {
		return r.runMihomoManager(ctx)
	})
}

func (r *Repository) runMihomoManager(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	secret := os.Getenv("MIHOMO_CONTROLLER_SECRET")
	apiToken := os.Getenv("MIHOMO_MANAGER_TOKEN")
	if secret == "" || apiToken == "" {
		return fmt.Errorf("manager and controller secrets are required")
	}
	instance := transport.MihomoConfig{Controller: Env("MIHOMO_CONTROLLER", "http://shared-mihomo:9090"), ProxyURL: Env("MIHOMO_PROXY_URL", "http://shared-mihomo:7890"), Secret: secret}
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = conn.Conn().Close(cleanup)
		conn.Release()
	}()
	var locked bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext(current_schema()),hashtext($1))`, instance.Controller).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("Mihomo controller already owned")
	}
	m := transport.NewManager(instance)
	m.ExitConcurrency = Number("MIHOMO_EXIT_CONCURRENCY", 8)
	m.RiskConcurrency = Number("MIHOMO_RISK_CONCURRENCY", 2)
	server := &http.Server{Addr: Env("MIHOMO_MANAGER_LISTEN", ":9080"), Handler: m.Handler(apiToken), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ListenAndServe() }()
	defer server.Close()
	// Only this goroutine touches the ownership connection.
	ownerErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	defer func() { cancel(); wg.Wait() }()
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 2*time.Second)
				e := conn.Ping(check)
				stop()
				if e != nil {
					cancel()
					_ = server.Close()
					ownerErr <- fmt.Errorf("controller ownership lost")
					return
				}
			}
		}
	}()
	// Heartbeats continue while Sync drains up to the lease maximum.
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s := m.Stats()
				message := ""
				if !s.Healthy {
					message = "内核未就绪或正在重载"
				}
				payload, _ := json.Marshal(map[string]any{"error": message, "running": true, "kernel": s, "exitCapacity": m.ExitConcurrency})
				_, _ = r.Pool.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('mihomo-manager',now(),$1) ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, payload)
			}
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// A round holds the process mutex through commit; pending changes are
	// coalesced here and reloaded only after that round releases the kernel.
	var pending []transport.RegistryEntry
	lastFingerprint := ""
	changedAt := time.Time{}
	nextAttempt := time.Time{}
	nextRegistryPoll := time.Time{}
	registryPoll := Seconds("MIHOMO_REGISTRY_POLL_SECONDS", 30)
	for {
		if m.Stats().Healthy {
			_ = m.Check(ctx)
		}
		if !time.Now().Before(nextRegistryPoll) {
			entries, e := r.kernelEntries(ctx)
			if e != nil {
				return e
			}
			raw, _ := json.Marshal(entries)
			fingerprint := string(raw)
			if fingerprint != lastFingerprint {
				pending = entries
				lastFingerprint = fingerprint
				changedAt = time.Now()
			}
			nextRegistryPoll = time.Now().Add(registryPoll)
		}

		if time.Now().After(nextAttempt) && ((!changedAt.IsZero() && time.Since(changedAt) >= 5*time.Second) || !m.Stats().Healthy) {
			reloadKernel := func() error {
				reload, stop := context.WithTimeout(ctx, transport.LeaseTTL+30*time.Second)
				defer stop()
				return m.Sync(reload, pending)
			}
			if err := reloadKernel(); err == nil {
				changedAt = time.Time{}
			} else {
				nextAttempt = time.Now().Add(5 * time.Second)
			}

		}

		select {
		case <-ctx.Done():
			select {
			case e := <-ownerErr:
				return e
			default:
				return nil
			}
		case e := <-serverErr:
			return e
		case <-ticker.C:
		}
	}
}
func (r *Repository) kernelEntries(ctx context.Context) ([]transport.RegistryEntry, error) {
	rows, err := r.Pool.Query(ctx, `SELECT identity,incarnation,node FROM node_explorer_node_registry ORDER BY identity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []transport.RegistryEntry{}
	for rows.Next() {
		var entry transport.RegistryEntry
		var raw []byte
		if err = rows.Scan(&entry.Identity, &entry.Incarnation, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &entry.Node); err != nil {
			return nil, err
		}
		entry.Digest = transport.ConfigDigest(entry.Node)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
