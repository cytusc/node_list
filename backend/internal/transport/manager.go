package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

const LeaseTTL = 150 * time.Second

var ErrManagerBusy = errors.New("Mihomo capacity paused or busy")
var ErrConfigRejected = errors.New("node configuration rejected or unsupported")

type ConfigRejection struct{ Reason string }

func (e *ConfigRejection) Error() string {
	return ErrConfigRejected.Error() + ": " + ConfigRejectionCode(e)
}
func (e *ConfigRejection) Unwrap() error { return ErrConfigRejected }
func ConfigRejectionCode(err error) string {
	var e *ConfigRejection
	if errors.As(err, &e) {
		switch e.Reason {
		case "missing-credentials", "invalid-endpoint", "missing-reality-key", "invalid-ech", "unsupported-transport", "invalid-xhttp-extra", "unsupported-xhttp-option", "control-character", "invalid-text", "chained-proxy", "name-collision", "kernel-rejected":
			return e.Reason
		}
	}
	return "link-format"
}

var ErrConfigChanged = errors.New("node missing or configuration changed")

type RegistryEntry struct {
	Identity    string      `json:"identity"`
	Incarnation string      `json:"incarnation"`
	Node        domain.Node `json:"-"`
	Digest      string      `json:"digest"`
}

func ConfigDigest(n domain.Node) string {
	p := outboundConfig(n)
	clone := make(domain.ProxyConfig, len(p))
	for k, v := range p {
		if !domain.DecorativeConfigField(k) {
			clone[k] = v
		}
	}
	raw, _ := json.Marshal(clone)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// proxyNameHex bounds the kernel-facing proxy name for panel and API
// readability (REFACTOR_PLAN §6.4). The identity↔name mapping stays a pure
// function of the identity; Sync quarantines the astronomically unlikely
// truncated-name collision instead of letting two nodes share one route.
const proxyNameHex = 16

func proxyName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "node-" + hex.EncodeToString(sum[:])[:proxyNameHex]
}
func token() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type Lease struct {
	ID         string          `json:"id"`
	Generation string          `json:"generation"`
	Targets    []RegistryEntry `json:"targets"`
	URLs       []string        `json:"urls"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Kind       string          `json:"kind"`
}
type LeaseRequest struct {
	Targets []RegistryEntry `json:"targets"`
	Kind    string          `json:"kind"`
	High    bool            `json:"high"`
}
type ManagerStats struct {
	Generation        string `json:"generation"`
	Loaded            int    `json:"loaded"`
	Quarantined       int    `json:"quarantined"`
	Reloads           int    `json:"reloads"`
	ReloadMillis      int64  `json:"reloadMillis"`
	ReloadPauseMillis int64  `json:"reloadPauseMillis"`
	ReloadDrainMillis int64  `json:"reloadDrainMillis"`
	ConfigLoads       int    `json:"configLoads"`
	Healthy           bool   `json:"healthy"`
}

// Manager is the sole controller writer. Leases fence reloads and are never renewed.
// The owner must terminate it immediately upon losing the DB ownership session.
// reloadDrainLimit bounds how long a config swap waits for in-flight leases.
const reloadDrainLimit = 2 * time.Second

type Manager struct {
	healthMu sync.Mutex
	Instance MihomoConfig
	// ExitConcurrency and RiskConcurrency are configured before serving
	// requests. Risk queries are IPPure lookups that ride a tunnel; a dead
	// tunnel burns the whole slot for the query timeout, so the ceiling has to
	// be high enough to keep the refresh queue moving.
	ExitConcurrency                      int
	RiskConcurrency                      int
	mu                                   sync.Mutex
	entries                              map[string]RegistryEntry
	rejected                             map[string]string
	credentials                          map[string]string
	leases                               map[string]Lease
	generation, epoch                    string
	sequence                             int
	reloading, healthy                   bool
	reloads                              int
	reloadMillis                         int64
	reloadPauseMillis, reloadDrainMillis int64
	configLoads                          int
	fingerprint                          string
	lastPayload                          []byte
}

func NewManager(instance MihomoConfig) *Manager {
	return &Manager{Instance: instance, ExitConcurrency: 8, RiskConcurrency: 2, epoch: token(), entries: map[string]RegistryEntry{}, rejected: map[string]string{}, credentials: map[string]string{}, leases: map[string]Lease{}}
}
func (m *Manager) prune() {
	for id, l := range m.leases {
		if !time.Now().Before(l.ExpiresAt) {
			delete(m.leases, id)
		}
	}
}
func (m *Manager) Stats() ManagerStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ManagerStats{Generation: m.generation, Loaded: len(m.entries) - len(m.rejected), Quarantined: len(m.rejected), Reloads: m.reloads, ReloadMillis: m.reloadMillis, ReloadPauseMillis: m.reloadPauseMillis, ReloadDrainMillis: m.reloadDrainMillis, ConfigLoads: m.configLoads, Healthy: m.healthy && !m.reloading}
}

func (m *Manager) controller(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(m.Instance.Controller, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Instance.Secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, errors.New("Mihomo controller unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, err
}
func (m *Manager) push(ctx context.Context, payload []byte) (int, error) {
	m.mu.Lock()
	m.configLoads++
	m.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"payload": string(payload)})
	raw, status, err := m.controller(ctx, http.MethodPut, "/configs?force=true", body)
	if err != nil {
		return -1, err
	}
	if status >= 200 && status < 300 {
		return -1, nil
	}
	if status == 400 {
		var d struct{ Message string }
		_ = json.Unmarshal(raw, &d)
		match := proxyErrorIndex.FindStringSubmatch(d.Message)
		if len(match) == 2 {
			i, _ := strconv.Atoi(match[1])
			return i, &ProxyConfigError{Index: i}
		}
	}
	return -1, errors.New("Mihomo configuration load failed")
}

// validateBatchSize bounds how many changed connections one validation load
// carries. Every load restarts the kernel, so small batches multiply the
// outage: a 44k-node registry whose digests all changed needed 179 reloads at
// 256 per batch, roughly six minutes of kernel churn during which every probe
// stalled and hit its task deadline. Larger batches keep the isolation
// property (the failing index is still removed and retried) at 1/8 the churn.
const validateBatchSize = 2048

// Sync drains before updating and restores the last valid document on failure.
// Call serially, after the registry has been stable for the coalescing interval.
func (m *Manager) Sync(ctx context.Context, entries []RegistryEntry) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Identity < entries[j].Identity })
	for i := range entries {
		entries[i].Digest = ConfigDigest(entries[i].Node)
	}
	raw, _ := json.Marshal(entries)
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	m.mu.Lock()
	if m.fingerprint == fingerprint && m.healthy {
		m.mu.Unlock()
		return nil
	}
	previousEntries := m.entries
	previousRejected := m.rejected
	// A reload in progress already carries the newest state it can: letting a
	// second caller start another one just extends the window where the kernel
	// reports unhealthy and every periodic probe skips.
	if m.reloading {
		m.mu.Unlock()
		return nil
	}
	m.reloading = true
	m.mu.Unlock()
	pauseStarted := time.Now()
	defer func() {
		m.mu.Lock()
		m.reloadPauseMillis = time.Since(pauseStarted).Milliseconds()
		m.reloading = false
		m.mu.Unlock()
	}()
	// Drain in-flight probes, but never unbounded: IP-verification traffic holds
	// leases for tens of seconds, and waiting for all of it kept the kernel in
	// "reloading" long enough that every periodic sweep was skipped. Swapping the
	// config may fail a request that was already in flight; stalling the whole
	// probe pipeline is worse.
	drainDeadline := time.Now().Add(reloadDrainLimit)
	for {
		m.mu.Lock()
		m.prune()
		empty := len(m.leases) == 0
		m.mu.Unlock()
		if empty || time.Now().After(drainDeadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	m.mu.Lock()
	m.reloadDrainMillis = time.Since(pauseStarted).Milliseconds()
	m.mu.Unlock()
	base, err := url.Parse(m.Instance.ProxyURL)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(base.Port())
	if err != nil {
		return err
	}
	controller, _ := url.Parse(m.Instance.Controller)
	controllerPort := controller.Port()
	if controllerPort == "" {
		controllerPort = "9090"
	}
	config := mihomoConfig{MixedPort: port, AllowLan: true, BindAddress: "*", Mode: "rule", ExternalController: "0.0.0.0:" + controllerPort, Secret: m.Instance.Secret, Rules: []string{"MATCH,REJECT"}}
	rejected := map[string]string{}
	credentials := map[string]string{}
	valid := []RegistryEntry{}
	changed := []RegistryEntry{}
	for _, entry := range entries {
		old, known := previousEntries[entry.Identity]
		same := known && old.Digest == entry.Digest
		if same && previousRejected[entry.Identity] != "" {
			rejected[entry.Identity] = previousRejected[entry.Identity]
			continue
		}
		p := outboundConfig(entry.Node)
		// Original display names cannot safely resolve arbitrary cross-node chains.
		if p == nil || p["dialer-proxy"] != nil || p["detour"] != nil {
			reason := "link-format"
			if len(entry.Node.RawConfig) == 0 && entry.Node.RawLink != "" {
				_, err := domain.ShareLinkProxy(entry.Node.RawLink, "probe")
				var detail *domain.ShareLinkError
				if errors.As(err, &detail) {
					reason = detail.Code
				}
			}
			if p != nil {
				reason = "chained-proxy"
			}
			rejected[entry.Identity] = reason
			continue
		}
		valid = append(valid, entry)
		if !same {
			changed = append(changed, entry)
		}
	}
	// Truncated display names must stay injective (§6.4): a 64-bit collision
	// is astronomically unlikely, but one is quarantined with a log line so a
	// node can never route through another node's proxy.
	seen := map[string]bool{}
	deduped := make([]RegistryEntry, 0, len(valid))
	for _, entry := range valid {
		if seen[proxyName(entry.Identity)] {
			rejected[entry.Identity] = "name-collision"
			log.Printf("mihomo: truncated proxy name collision quarantined")
			continue
		}
		seen[proxyName(entry.Identity)] = true
		deduped = append(deduped, entry)
	}
	valid = deduped
	start := time.Now()
	// Bound bad-config isolation to new/changed connection digests. Unchanged
	// configs were already installed successfully, including known rejections.
	// A small update needs only the final install; large imports validate in batches.
	// No lease is admitted during these loads.
	if len(changed) > validateBatchSize {
		for offset := 0; offset < len(changed); offset += validateBatchSize {
			end := offset + validateBatchSize
			if end > len(changed) {
				end = len(changed)
			}
			batch := append([]RegistryEntry(nil), changed[offset:end]...)
			for {
				check := config
				check.Proxies = nil
				check.Authentication = []string{"disabled:" + token()}
				batchProxies := make([]domain.ProxyConfig, 0, len(batch))
				for _, entry := range batch {
					batchProxies = append(batchProxies, outboundConfig(entry.Node))
				}
				check.Proxies = applyECHDNS(&check, batchProxies)
				for i, entry := range batch {
					p := check.Proxies[i]
					p["name"] = proxyName(entry.Identity)
				}
				document, e := marshalMihomoConfig(check)
				if e != nil {
					return e
				}
				index, e := m.push(ctx, document)
				if e == nil {
					break
				}
				if index < 0 || index >= len(batch) {
					break
				} // Final install handles infrastructure errors and rollback.
				rejected[batch[index].Identity] = "kernel-rejected"
				batch = append(batch[:index], batch[index+1:]...)
			}
		}
		filtered := valid[:0]
		for _, entry := range valid {
			if rejected[entry.Identity] == "" {
				filtered = append(filtered, entry)
			}
		}
		valid = filtered
	}
	var payload []byte
	for {
		config.Proxies = nil
		proxyConfigs := make([]domain.ProxyConfig, 0, len(valid))
		config.Authentication = nil
		config.Rules = nil
		for _, entry := range valid {
			name := proxyName(entry.Identity)
			password := token()
			credentials[entry.Identity] = password
			proxyConfigs = append(proxyConfigs, outboundConfig(entry.Node))
			config.Authentication = append(config.Authentication, name+":"+password)
			config.Rules = append(config.Rules, "IN-USER,"+name+","+name)
		}
		config.Proxies = applyECHDNS(&config, proxyConfigs)
		for i, entry := range valid {
			config.Proxies[i]["name"] = proxyName(entry.Identity)
		}
		if len(config.Authentication) == 0 {
			config.Authentication = []string{"disabled:" + token()}
		}
		config.Rules = append(config.Rules, "MATCH,REJECT")
		payload, err = marshalMihomoConfig(config)
		if err != nil {
			return err
		}
		index, e := m.push(ctx, payload)
		if e == nil {
			break
		}
		if index >= 0 && index < len(valid) {
			rejected[valid[index].Identity] = "kernel-rejected"
			valid = append(valid[:index], valid[index+1:]...)
			continue
		}
		m.mu.Lock()
		previous := append([]byte(nil), m.lastPayload...)
		m.healthy = false
		m.mu.Unlock()
		if len(previous) > 0 {
			rollback, stop := context.WithTimeout(context.Background(), 15*time.Second)
			_, rollbackErr := m.push(rollback, previous)
			stop()
			if rollbackErr == nil {
				m.mu.Lock()
				m.healthy = true
				m.mu.Unlock()
			}
		}
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sequence++
	m.generation = fmt.Sprintf("%s:%d", m.epoch, m.sequence)
	m.entries = map[string]RegistryEntry{}
	for _, entry := range entries {
		m.entries[entry.Identity] = entry
	}
	m.rejected = rejected
	m.credentials = credentials
	m.fingerprint = fingerprint
	m.lastPayload = payload
	m.healthy = true
	m.reloads++
	m.reloadMillis = time.Since(start).Milliseconds()
	return nil
}
func (m *Manager) Acquire(req LeaseRequest) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	if m.reloading {
		return Lease{}, ErrManagerBusy
	}
	if !m.healthy {
		return Lease{}, errors.New("manager unavailable")
	}
	limit := map[string]int{"exit": 8, "risk": 2}[req.Kind]
	if req.Kind == "exit" && m.ExitConcurrency > 0 {
		limit = m.ExitConcurrency
	}
	if req.Kind == "risk" && m.RiskConcurrency > 0 {
		limit = m.RiskConcurrency
	}
	if limit == 0 || len(req.Targets) == 0 || len(req.Targets) > 2 {
		return Lease{}, errors.New("invalid lease request")
	}
	count := 0
	for _, l := range m.leases {
		if l.Kind == req.Kind {
			count++
		}
	}
	if count >= limit {
		return Lease{}, ErrManagerBusy
	}
	l := Lease{ID: token(), Generation: m.generation, ExpiresAt: time.Now().Add(LeaseTTL), Kind: req.Kind}
	for _, target := range req.Targets {
		entry, ok := m.entries[target.Identity]
		if !ok || entry.Digest != target.Digest || (target.Incarnation != "" && target.Incarnation != entry.Incarnation) {
			return Lease{}, ErrConfigChanged
		}
		if reason := m.rejected[target.Identity]; reason != "" {
			return Lease{}, &ConfigRejection{Reason: reason}
		}
		entry.Node = domain.Node{}
		l.Targets = append(l.Targets, entry)
		u, _ := url.Parse(m.Instance.ProxyURL)
		u.User = url.UserPassword(proxyName(target.Identity), m.credentials[target.Identity])
		l.URLs = append(l.URLs, u.String())
	}
	m.leases[l.ID] = l
	return l, nil
}
func (m *Manager) Release(id string) { m.mu.Lock(); delete(m.leases, id); m.mu.Unlock() }

// invalidate fences a confirmed kernel failure against a concurrent reload.
// Reasons are fixed codes: controller errors may contain node credentials.
func (m *Manager) invalidate(generation, reason string, status int) {
	m.mu.Lock()
	changed := m.generation == generation && m.healthy && !m.reloading
	if changed {
		m.healthy = false
		m.leases = map[string]Lease{}
	}
	m.mu.Unlock()
	if changed {
		log.Printf("mihomo-manager: kernel unhealthy reason=%s status=%d generation=%s", reason, status, generation)
	}
}

// Check confirms transient controller errors with a second independent probe.
// Missing registered proxies prove state loss immediately. Cancellation of the
// caller remains a single request failure, never evidence about global health.
func (m *Manager) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Avoid many failing proof validators simultaneously probing the controller.
	for !m.healthMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer m.healthMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	generation := m.generation
	path := "/version"
	for id := range m.entries {
		if m.rejected[id] == "" {
			path = "/proxies/" + url.PathEscape(proxyName(id))
			break
		}
	}
	healthy := m.healthy
	m.mu.Unlock()
	if !healthy {
		return errors.New("kernel not ready")
	}
	for attempt, budget := range []time.Duration{2 * time.Second, 3 * time.Second} {
		check, cancel := context.WithTimeout(ctx, budget)
		_, status, err := m.controller(check, http.MethodGet, path, nil)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && status == 200 {
			return nil
		}
		if status == http.StatusNotFound || attempt == 1 {
			reason := "health_probe_failed"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "health_probe_timeout"
			}
			m.invalidate(generation, reason, status)
			return errors.New("kernel unavailable or restarted")
		}
	}
	return nil
}

// Kernel restarts lose the in-memory registry. Outages invalidate all results.
func (m *Manager) Validate(ctx context.Context, id string) error {
	return m.validate(ctx, id, nil)
}

// inspect reuses the validated proxy response while the caller owns its lease.
func (m *Manager) validate(ctx context.Context, id string, inspect func([]byte)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.prune()
	l, ok := m.leases[id]
	valid := ok && m.healthy && l.Generation == m.generation
	m.mu.Unlock()
	if !valid {
		return errors.New("lease expired or generation changed")
	}
	for _, entry := range l.Targets {
		raw, status, err := m.controller(ctx, http.MethodGet, "/proxies/"+url.PathEscape(proxyName(entry.Identity)), nil)
		if err != nil || status != 200 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if status == http.StatusNotFound {
				// A missing registered proxy is direct evidence of kernel state loss.
				m.invalidate(l.Generation, "registered_proxy_missing", status)
			} else {
				// A transient request failure rejects this proof, but only a fresh
				// health probe may invalidate other callers' leases.
				_ = m.Check(ctx)
			}
			return errors.New("Mihomo infrastructure unavailable")
		}
		if inspect != nil {
			inspect(raw)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	_, ok = m.leases[id]
	if !ok || !m.healthy || m.generation != l.Generation {
		return errors.New("lease expired")
	}
	return nil
}
func (m *Manager) Handler(secret string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		s := m.Stats()
		if !s.Healthy {
			w.WriteHeader(503)
		}
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("POST /v1/leases", func(w http.ResponseWriter, r *http.Request) {
		var req LeaseRequest
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		l, err := m.Acquire(req)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		_ = json.NewEncoder(w).Encode(l)
	})
	mux.HandleFunc("POST /v1/leases/{id}/validate", func(w http.ResponseWriter, r *http.Request) {
		if m.Validate(r.Context(), r.PathValue("id")) != nil {
			http.Error(w, "invalid lease", 409)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /v1/leases/{id}", func(w http.ResponseWriter, r *http.Request) { m.Release(r.PathValue("id")); w.WriteHeader(204) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret == "" || r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
	})
}

// Only fixed diagnostic codes cross the API; controller errors can contain
// proxy credentials and must never be returned to workers or logs.
func writeManagerError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "manager_unavailable"
	switch {
	case errors.Is(err, ErrManagerBusy):
		status, code = http.StatusTooManyRequests, "manager_busy"
	case errors.Is(err, ErrConfigRejected):
		status, code = http.StatusUnprocessableEntity, "node_config_rejected"
	case errors.Is(err, ErrConfigChanged):
		status, code = http.StatusConflict, "node_config_changed"
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "reason": ConfigRejectionCode(err)})
}
