package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// ManagerClient has no controller credentials and never writes kernel config.
type ManagerClient struct{ URL, Token string }

var ErrManagerUnavailable = errors.New("Mihomo manager unavailable")

type ManagerHTTPError struct{ Status int }

func (e *ManagerHTTPError) Error() string {
	return fmt.Sprintf("Mihomo manager HTTP %d; retry required", e.Status)
}

func (c *ManagerClient) call(ctx context.Context, method, path string, in, out any) error {
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
		return ErrManagerUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrManagerBusy
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var v struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
		if resp.StatusCode == http.StatusUnprocessableEntity && v.Code == "node_config_rejected" {
			return ErrConfigRejected
		}
		if resp.StatusCode == http.StatusConflict && v.Code == "node_config_changed" {
			return ErrConfigChanged
		}
		return &ManagerHTTPError{Status: resp.StatusCode}
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
