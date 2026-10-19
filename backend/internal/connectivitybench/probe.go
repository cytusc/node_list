// Package connectivitybench is a read-only experiment. It does not import the
// service runner, queue, repository, lifecycle, or database migration packages.
package connectivitybench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
)

type Verdict struct {
	Reachable     bool      `json:"reachable"`
	ConfirmedDead bool      `json:"confirmed_dead"`
	Error         string    `json:"error,omitempty"`
	Attempts      int       `json:"attempts"`
	Trace         []Attempt `json:"trace,omitempty"`
	ReferenceID   string    `json:"reference_id,omitempty"`
}

type Attempt struct {
	Phase       string    `json:"phase"`
	At          time.Time `json:"at"`
	ElapsedMS   float64   `json:"elapsed_ms"`
	Outcome     string    `json:"outcome"`
	ReferenceID string    `json:"reference_id,omitempty"`
}

type Reference struct {
	ID   string
	Path *transport.Outbound
}

type Policy struct {
	Timeout, RetryDelay time.Duration
	Probe               func(context.Context, *http.Client) (bool, error)
	ControlProbe        func(context.Context, *http.Client, string, string) (bool, error)
}

func ProductionPolicy() Policy {
	return Policy{Timeout: 12 * time.Second, RetryDelay: 10 * time.Second, Probe: diagnosticHTTP}
}

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

// Same request contract as transport.ProbeHTTP, with a sanitized status error
// so benchmark disagreements can distinguish rate limits from tunnel errors.
func diagnosticHTTP(ctx context.Context, c *http.Client) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "node-explorer-github-patrol")
	response, err := c.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); response.Body.Close() }()
	if response.StatusCode != 200 {
		return false, httpStatusError(response.StatusCode)
	}
	return true, nil
}

// Evaluate deliberately follows service.live: GET/200, two attempts, then a
// fresh direct AND independent reference check. Preparation failures and
// cancellation never constitute evidence that a node is dead.
func (p Policy) Evaluate(ctx context.Context, target, reference *transport.Outbound) Verdict {
	return p.EvaluateReferences(ctx, target, []Reference{{Path: reference}})
}

// A recent reference is a candidate, not a guarantee of current availability.
// Try up to three independently selected paths, preserving every failed check.
// All paths belong to the same immutable wave and each request remains fresh.
func (p Policy) EvaluateReferences(ctx context.Context, target *transport.Outbound, references []Reference) Verdict {
	v := Verdict{}
	if ctx.Err() != nil {
		v.Error = "cancelled"
		return v
	}
	if target == nil || target.Error != nil || target.Client == nil {
		v.Error = "unsupported"
		return v
	}
	check := func(c *http.Client, phase, refID string) bool {
		started := time.Now()
		probeCtx, cancel := context.WithTimeout(ctx, p.Timeout)
		defer cancel()
		c.CloseIdleConnections()
		probe := p.Probe
		if p.ControlProbe != nil && (phase == "host-control" || phase == "reference-control") {
			probe = func(ctx context.Context, c *http.Client) (bool, error) { return p.ControlProbe(ctx, c, phase, refID) }
		}
		ok, err := probe(probeCtx, c)
		outcome := "http-non-200"
		if ok && err == nil {
			outcome = "http-200"
		} else if err != nil {
			outcome = "transport-error"
			var status httpStatusError
			var dns *net.DNSError
			var op *net.OpError
			var timeout net.Error
			if errors.As(err, &status) {
				outcome = fmt.Sprintf("http-%d", int(status))
			} else if errors.Is(err, context.Canceled) {
				outcome = "cancelled"
			} else if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
				outcome = "timeout"
			} else if errors.As(err, &dns) {
				outcome = "dns-error"
			} else if errors.As(err, &op) {
				outcome = "connection-error"
			}
		}
		v.Trace = append(v.Trace, Attempt{Phase: phase, At: started.UTC(), ElapsedMS: float64(time.Since(started).Microseconds()) / 1000, Outcome: outcome, ReferenceID: refID})
		return err == nil && ok
	}
	v.Attempts = 1
	if check(target.Client, "target-first", "") {
		v.Reachable = true
		return v
	}
	timer := time.NewTimer(p.RetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		v.Error = "cancelled"
		return v
	case <-timer.C:
	}
	v.Attempts = 2
	if check(target.Client, "target-retry", "") {
		v.Reachable = true
		return v
	}
	if ctx.Err() != nil {
		v.Error = "cancelled"
		return v
	}
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer direct.CloseIdleConnections()
	var valid []Reference
	for _, ref := range references {
		if ref.Path != nil && ref.Path.Error == nil && ref.Path.Client != nil {
			valid = append(valid, ref)
		}
	}
	if len(valid) > 0 && check(direct, "host-control", "") {
		for _, ref := range valid[:min(3, len(valid))] {
			if ctx.Err() != nil {
				break
			}
			v.ReferenceID = ref.ID
			if check(ref.Path.Client, "reference-control", ref.ID) {
				v.ConfirmedDead = true
				break
			}
		}
	}
	v.Error = "insufficient-controls"
	if v.ConfirmedDead {
		v.Error = "two-failures"
	}
	if ctx.Err() != nil {
		v.ConfirmedDead = false
		v.Error = "cancelled"
	}
	return v
}
