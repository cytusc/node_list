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
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

const DelayTarget = "https://www.gstatic.com/generate_204"
const LeaseTTL = 150 * time.Second

var ErrManagerBusy = errors.New("Mihomo capacity paused or busy")
var ErrConfigRejected = errors.New("node configuration rejected or unsupported")
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
		if k != "name" {
			clone[k] = v
		}
	}
	raw, _ := json.Marshal(clone)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func proxyName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "node-" + hex.EncodeToString(sum[:])
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
type DelayResult struct {
	Success    bool      `json:"success"`
	Delay      int       `json:"delay"`
	Generation string    `json:"generation"`
	CheckedAt  time.Time `json:"checkedAt"`
}
type ManagerStats struct {
	Generation   string `json:"generation"`
	Loaded       int    `json:"loaded"`
	Quarantined  int    `json:"quarantined"`
	Reloads      int    `json:"reloads"`
	ReloadMillis int64  `json:"reloadMillis"`
	FastRunning  int    `json:"fastRunning"`
	Healthy      bool   `json:"healthy"`
}

// Manager is the sole controller writer. Leases fence reloads and are never renewed.
// The owner must terminate it immediately upon losing the DB ownership session.
type Manager struct {
	Instance                      MihomoConfig
	mu                            sync.Mutex
	entries                       map[string]RegistryEntry
	rejected                      map[string]bool
	credentials                   map[string]string
	leases                        map[string]Lease
	generation, epoch             string
	sequence                      int
	reloading, healthy            bool
	reloads                       int
	reloadMillis                  int64
	fingerprint                   string
	lastPayload                   []byte
	fast, fastNormal, highWaiting int
	OnDelay                       func(context.Context, bool, bool)
}

func NewManager(instance MihomoConfig) *Manager {
	return &Manager{Instance: instance, epoch: token(), entries: map[string]RegistryEntry{}, rejected: map[string]bool{}, credentials: map[string]string{}, leases: map[string]Lease{}}
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
	return ManagerStats{m.generation, len(m.entries) - len(m.rejected), len(m.rejected), m.reloads, m.reloadMillis, m.fast, m.healthy && !m.reloading}
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
		return nil, 0, errors.New("Mihomo controller unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, err
}
func (m *Manager) push(ctx context.Context, payload []byte) (int, error) {
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
	m.reloading = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.reloading = false; m.mu.Unlock() }()
	for {
		m.mu.Lock()
		m.prune()
		empty := len(m.leases) == 0
		m.mu.Unlock()
		if empty {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
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
	rejected := map[string]bool{}
	credentials := map[string]string{}
	valid := []RegistryEntry{}
	for _, entry := range entries {
		p := outboundConfig(entry.Node)
		// Original display names cannot safely resolve arbitrary cross-node chains.
		if p == nil || p["dialer-proxy"] != nil || p["detour"] != nil {
			rejected[entry.Identity] = true
			continue
		}
		valid = append(valid, entry)
	}
	start := time.Now()
	// Bound bad-config isolation cost: retry only a small batch, then install the
	// complete valid registry once. No lease is admitted during these loads.
	if len(valid) > 256 {
		filtered := make([]RegistryEntry, 0, len(valid))
		for offset := 0; offset < len(valid); offset += 256 {
			end := offset + 256
			if end > len(valid) {
				end = len(valid)
			}
			batch := append([]RegistryEntry(nil), valid[offset:end]...)
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
				rejected[batch[index].Identity] = true
				batch = append(batch[:index], batch[index+1:]...)
			}
			filtered = append(filtered, batch...)
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
			rejected[valid[index].Identity] = true
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
	limit := map[string]int{"strict": 8, "exit": 8, "risk": 2, "delay": 32, "proof": 128}[req.Kind]
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
		if m.rejected[target.Identity] {
			return Lease{}, ErrConfigRejected
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

// Check detects a restart even when there are no worker requests in flight.
func (m *Manager) Check(ctx context.Context) error {
	m.mu.Lock()
	generation := m.generation
	path := "/version"
	for id := range m.entries {
		if !m.rejected[id] {
			path = "/proxies/" + url.PathEscape(proxyName(id))
			break
		}
	}
	healthy := m.healthy
	m.mu.Unlock()
	if !healthy {
		return errors.New("kernel not ready")
	}
	_, status, err := m.controller(ctx, http.MethodGet, path, nil)
	if err != nil || status != 200 {
		m.mu.Lock()
		if m.generation == generation {
			m.healthy = false
			m.leases = map[string]Lease{}
		}
		m.mu.Unlock()
		return errors.New("kernel unavailable or restarted")
	}
	return nil
}

// Kernel restarts lose the in-memory registry. Outages invalidate all results.
func (m *Manager) Validate(ctx context.Context, id string) error {
	m.mu.Lock()
	m.prune()
	l, ok := m.leases[id]
	valid := ok && m.healthy && l.Generation == m.generation
	m.mu.Unlock()
	if !valid {
		return errors.New("lease expired or generation changed")
	}
	for _, entry := range l.Targets {
		_, status, err := m.controller(ctx, http.MethodGet, "/proxies/"+url.PathEscape(proxyName(entry.Identity)), nil)
		if err != nil || status != 200 {
			m.mu.Lock()
			m.healthy = false
			m.leases = map[string]Lease{}
			m.mu.Unlock()
			return errors.New("Mihomo infrastructure unavailable")
		}
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
func (m *Manager) Delay(ctx context.Context, req LeaseRequest) (result DelayResult, err error) {
	if len(req.Targets) != 1 {
		return result, errors.New("one delay target required")
	}
	// Idle reserved capacity may be borrowed. Waiting residential calls get the next completion.
	waitingHigh := req.High
	m.mu.Lock()
	if waitingHigh {
		m.highWaiting++
	}
	m.mu.Unlock()
	defer func() {
		if waitingHigh {
			m.mu.Lock()
			m.highWaiting--
			m.mu.Unlock()
		}
	}()
	for {
		m.mu.Lock()
		allowed := m.fast < 32 && (req.High || m.highWaiting == 0 || m.fastNormal < 24)
		if allowed {
			if waitingHigh {
				m.highWaiting--
				waitingHigh = false
			}
			m.fast++
			if !req.High {
				m.fastNormal++
			}
		}
		m.mu.Unlock()
		if allowed {
			break
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(time.Millisecond * 5):
		}
	}
	defer func() {
		m.mu.Lock()
		m.fast--
		if !req.High {
			m.fastNormal--
		}
		m.mu.Unlock()
		if m.OnDelay != nil && !errors.Is(err, ErrManagerBusy) {
			m.OnDelay(context.WithoutCancel(ctx), result.Success, err != nil)
		}
	}()
	req.Kind = "delay"
	l, e := m.Acquire(req)
	if e != nil {
		return result, e
	}
	defer m.Release(l.ID)
	// Mihomo's DNS/dial setup can exceed its requested 2s delay timeout.
	// Allow its explicit 503/504 response to arrive; a transport timeout remains
	// an infrastructure error, never evidence that a node is dead.
	probe, stop := context.WithTimeout(ctx, 12*time.Second)
	defer stop()
	path := "/proxies/" + url.PathEscape(proxyName(req.Targets[0].Identity)) + "/delay?timeout=2000&expected=204&url=" + url.QueryEscape(DelayTarget)
	raw, status, e := m.controller(probe, http.MethodGet, path, nil)
	if e != nil {
		return result, e
	}
	if e = m.Validate(ctx, l.ID); e != nil {
		return result, e
	}
	result.Generation = l.Generation
	result.CheckedAt = time.Now().UTC()
	if status == 200 {
		var d struct {
			Delay int `json:"delay"`
		}
		if json.Unmarshal(raw, &d) != nil {
			return result, errors.New("invalid controller delay response")
		}
		result.Success = true
		result.Delay = d.Delay
	} else if status != 503 && status != 504 {
		return result, errors.New("Mihomo delay infrastructure error")
	}
	return result, nil
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
	mux.HandleFunc("POST /v1/delay", func(w http.ResponseWriter, r *http.Request) {
		var req LeaseRequest
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil || len(req.Targets) != 1 {
			http.Error(w, "invalid request", 400)
			return
		}
		v, err := m.Delay(r.Context(), req)
		if err != nil {
			writeManagerError(w, err)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	})
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
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
