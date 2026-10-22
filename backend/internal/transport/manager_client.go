package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// ManagerClient has no controller credentials and never writes kernel config.
type ManagerClient struct {
	URL, Token string
	mu         sync.Mutex
	retryAt    time.Time
	capacity   map[string]capacityCooldown
}

// All lanes in a worker share the outage cooldown. Wait before claiming work
// as well as before HTTP calls so a manager outage cannot churn the full queue.
func (c *ManagerClient) WaitReady(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		delay := time.Until(c.retryAt)
		c.mu.Unlock()
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Busy is scoped to a pool: saturated exit probes must not pause proof
// validation, risk probes, or lease release. The map is protected by mu.
type capacityCooldown struct {
	retryAt  time.Time
	failures int
}

func (c *ManagerClient) WaitCapacity(ctx context.Context, kind string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		delay := time.Until(c.capacity[kind].retryAt)
		c.mu.Unlock()
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *ManagerClient) pauseCapacity(kind string) {
	if kind == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity == nil {
		c.capacity = make(map[string]capacityCooldown)
	}
	state := c.capacity[kind]
	// Responses already in flight belong to the same failed wave. They must
	// not compound the delay or keep pushing its deadline into the future.
	if time.Now().Before(state.retryAt) {
		return
	}
	state.failures = min(state.failures+1, 5)
	ceiling := min(2*time.Second<<(state.failures-1), 30*time.Second)
	delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)))
	state.retryAt = time.Now().Add(delay)
	c.capacity[kind] = state
}

func (c *ManagerClient) capacitySuccess(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.capacity[kind]
	state.failures = 0
	if c.capacity != nil {
		c.capacity[kind] = state
	}
}

func (c *ManagerClient) pause() {
	c.mu.Lock()
	c.retryAt = time.Now().Add(2 * time.Second)
	c.mu.Unlock()
}

var ErrManagerUnavailable = errors.New("Mihomo manager unavailable")

type ManagerHTTPError struct{ Status int }

func (e *ManagerHTTPError) Error() string {
	return fmt.Sprintf("Mihomo manager HTTP %d; retry required", e.Status)
}

func (c *ManagerClient) call(ctx context.Context, method, path string, in, out any) error {
	// Release must remain available during outages so reload can drain leases.
	if method != http.MethodDelete {
		if err := c.WaitReady(ctx); err != nil {
			return err
		}
	}
	// Only admission calls wait on busy; existing leases must be able to
	// validate and finish so that capacity becomes available.
	capacityKind := ""
	if method == http.MethodPost {
		if path == "/v1/delay" {
			capacityKind = "delay"
		}
		if path == "/v1/leases" {
			if lease, ok := in.(LeaseRequest); ok {
				capacityKind = lease.Kind
			}
		}
	}
	if capacityKind != "" {
		if err := c.WaitCapacity(ctx, capacityKind); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.pause()
		return ErrManagerUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		c.pause()
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		c.pauseCapacity(capacityKind)
		return ErrManagerBusy
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var v struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
		if resp.StatusCode == http.StatusUnprocessableEntity && v.Code == "node_config_rejected" {
			return &ConfigRejection{Reason: ConfigRejectionCode(&ConfigRejection{Reason: v.Reason})}
		}
		if resp.StatusCode == http.StatusConflict && v.Code == "node_config_changed" {
			return ErrConfigChanged
		}
		return &ManagerHTTPError{Status: resp.StatusCode}
	}
	if capacityKind != "" {
		c.capacitySuccess(capacityKind)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}
func (c *ManagerClient) Delay(ctx context.Context, n domain.Node, incarnation string, high bool) (DelayResult, error) {
	var v DelayResult
	err := c.call(ctx, http.MethodPost, "/v1/delay", LeaseRequest{Targets: []RegistryEntry{{Identity: domain.AggregateIdentity(n), Incarnation: incarnation, Digest: ConfigDigest(n)}}, High: high}, &v)
	return v, err
}
func (c *ManagerClient) Generation(ctx context.Context, generation string) error {
	var s ManagerStats
	if err := c.call(ctx, http.MethodGet, "/v1/health", nil, &s); err != nil {
		return err
	}
	if generation == "" || generation != s.Generation {
		return errors.New("kernel generation changed")
	}
	return nil
}

type ManagedEngine struct {
	Manager *ManagerClient
	Kind    string
	High    bool
}

func (e *ManagedEngine) Name() string         { return "managed-mihomo" }
func (e *ManagedEngine) Supports(string) bool { return true }
func (e *ManagedEngine) Reserve(ctx context.Context, nodes []domain.Node) (map[int]*Outbound, func(), error) {
	req := LeaseRequest{Kind: e.Kind, High: e.High}
	for _, n := range nodes {
		req.Targets = append(req.Targets, RegistryEntry{Identity: domain.AggregateIdentity(n), Digest: ConfigDigest(n)})
	}
	var l Lease
	if err := e.Manager.call(ctx, http.MethodPost, "/v1/leases", req, &l); err != nil {
		return nil, nil, err
	}
	out := map[int]*Outbound{}
	if len(l.URLs) != len(nodes) || len(l.Targets) != len(nodes) {
		return nil, nil, errors.New("invalid lease response")
	}
	for i, u := range l.URLs {
		client := listenerClient(u)
		client.Transport = &leaseTransport{base: client.Transport, expiry: l.ExpiresAt}
		out[i] = &Outbound{Client: client, Label: "managed-mihomo", Generation: l.Generation, ExpiresAt: l.ExpiresAt, Incarnation: l.Targets[i].Incarnation, Digest: l.Targets[i].Digest, Validate: func(ctx context.Context) error {
			return e.Manager.call(ctx, http.MethodPost, "/v1/leases/"+l.ID+"/validate", nil, nil)
		}}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, o := range out {
				o.Client.CloseIdleConnections()
			}
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_ = e.Manager.call(cleanup, http.MethodDelete, "/v1/leases/"+l.ID, nil, nil)
		})
	}
	return out, release, nil
}

// Bind every request (including redirects) to the immutable lease deadline.
type leaseTransport struct {
	base   http.RoundTripper
	expiry time.Time
}

func (t *leaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithDeadline(req.Context(), t.expiry)
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &leaseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}
func (t *leaseTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type leaseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *leaseBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
