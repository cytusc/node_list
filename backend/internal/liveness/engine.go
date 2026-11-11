package liveness

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cytusc/node_list/backend/internal/proxyprocess"
)

type Result struct {
	Status      string          `json:"status"`
	Engine      string          `json:"engine"`
	Reachable   bool            `json:"reachable"`
	CheckedAt   time.Time       `json:"checkedAt"`
	LatencyMS   int64           `json:"latencyMs"`
	Reason      string          `json:"reason,omitempty"`
	Fallback    string          `json:"fallback,omitempty"`
	ProbeTarget string          `json:"probeTarget,omitempty"`
	Targets     map[string]bool `json:"targets,omitempty"`
	// FailureStreak counts consecutive "unreachable" verdicts for this
	// configuration; the store uses it to back off dead addresses.
	FailureStreak int `json:"failureStreak,omitempty"`
	ErrorStreak   int `json:"errorStreak,omitempty"`
}

func (r Result) Valid() bool { return r.Status == "reachable" || r.Status == "unreachable" }

type Engine interface {
	Probe(context.Context, Proxy) Result
}
type ProcessEngine struct {
	Name, Binary, Target string
	BackupTarget         string
	Expected             int
	Timeout              time.Duration
	RootCAs              *x509.CertPool
}

func request(ctx context.Context, tr *http.Transport, target string, expected int) (int64, error) {
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != expected {
		return 0, errors.New("unexpected_status")
	}
	// Bound both bytes and time; matching headers alone are not a completed probe.
	n, err := io.Copy(io.Discard, io.LimitReader(res.Body, 4097))
	if err != nil {
		return 0, err
	}
	if n > 4096 {
		return 0, errors.New("response_too_large")
	}
	return max(1, time.Since(started).Milliseconds()), nil
}

// Reserve time for each remaining target so a blocked primary cannot exhaust
// the entire probe deadline. Both requests use the same private proxy.
func requestTargets(ctx context.Context, tr *http.Transport, targets []string, expected int) (int64, string, map[string]bool, error) {
	results := make(map[string]bool, len(targets))
	var last error
	for i, target := range targets {
		check := ctx
		cancel := func() {}
		if deadline, ok := ctx.Deadline(); ok {
			check, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(targets)-i))
		}
		latency, err := request(check, tr, target, expected)
		cancel()
		results[target] = err == nil
		if err == nil {
			return latency, target, results, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return 0, "", results, last
}

func (e ProcessEngine) Probe(parent context.Context, p Proxy) Result {
	r := Result{Engine: e.Name, Status: "infrastructure", Reason: "startup_failed", CheckedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(parent, e.Timeout)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "nl-")
	if err != nil {
		return r
	}
	defer os.RemoveAll(dir)
	var cmd *exec.Cmd
	var tr *http.Transport
	var ready func() bool
	var done <-chan struct{}
	if e.Name == "xray" {
		out, reason := XrayOutbound(p)
		if reason != "" {
			r.Status = "unsupported"
			r.Reason = reason
			return r
		}
		socket := filepath.Join(dir, "p.sock")
		config := Proxy{"log": Proxy{"loglevel": "none"}, "inbounds": []any{Proxy{"listen": socket, "protocol": "http", "settings": Proxy{}}}, "outbounds": []any{out}}
		b, _ := json.Marshal(config)
		cmd = exec.CommandContext(ctx, e.Binary, "run", "-config", "stdin:")
		cmd.Stdin = bytes.NewReader(b)
		tr = &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "probe.local"}), DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
		ready = func() bool { _, err := os.Stat(socket); return err == nil }
	} else {
		var closeProcess func()
		tr, closeProcess, done, err = proxyprocess.Mihomo(ctx, e.Binary, p)
		if err != nil {
			r.Reason = err.Error()
			if r.Reason == "kernel_rejected" {
				r.Status = "config_error"
			}
			return r
		}
		defer closeProcess()
	}

	if e.RootCAs != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: e.RootCAs, MinVersion: tls.VersionTLS12}
	}
	if cmd != nil {
		// Discard all child output: proxy configurations and errors can carry secrets.
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.WaitDelay = time.Second
		if cmd.Start() != nil {
			return r
		}
		exited := make(chan struct{})
		done = exited
		go func() { _ = cmd.Wait(); close(exited) }()
		defer func() { _ = cmd.Process.Kill(); <-done }()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !ready() {
			select {
			case <-ctx.Done():
				r.Reason = "startup_timeout"
				return r
			case <-done:
				r.Status = "config_error"
				r.Reason = "kernel_rejected"
				return r
			case <-ticker.C:
			}
		}
	}

	targets := []string{e.Target}
	if e.BackupTarget != "" {
		targets = append(targets, e.BackupTarget)
	}
	latency, target, results, err := requestTargets(ctx, tr, targets, e.Expected)
	if parent.Err() != nil {
		r.Status = "cancelled"
		r.Reason = "cancelled"
		return r
	}
	select {
	case <-done:
		r.Reason = "kernel_exited"
		return r
	default:
	}
	r.CheckedAt = time.Now().UTC()
	r.LatencyMS = latency
	r.ProbeTarget = target
	r.Targets = results
	if err == nil {
		r.Status = "reachable"
		r.Reachable = true
		r.Reason = ""
	} else {
		r.Status = "unreachable"
		r.Reason = "target_failed"
	}
	return r
}
