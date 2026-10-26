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
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Result struct {
	Status    string    `json:"status"`
	Engine    string    `json:"engine"`
	Reachable bool      `json:"reachable"`
	CheckedAt time.Time `json:"checkedAt"`
	LatencyMS int64     `json:"latencyMs"`
	Reason    string    `json:"reason,omitempty"`
	Fallback  string    `json:"fallback,omitempty"`
}

func (r Result) Valid() bool { return r.Status == "reachable" || r.Status == "unreachable" }

type Engine interface {
	Probe(context.Context, Proxy) Result
}
type ProcessEngine struct {
	Name, Binary, Target string
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
		// A random per-process authenticated loopback proxy. No controller or shared
		// kernel, and no mutable route can be held by a slow node.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return r
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		token := filepath.Base(dir)
		copied := Proxy{}
		for k, v := range p {
			if k != "__ech-resolver" {
				copied[k] = v
			}
		}
		// Mihomo opens its listener while its tunnel is still suspended. A TCP
		// connect alone can therefore produce false negatives. Route a private
		// loopback readiness request through the tunnel before probing the node.
		checkListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return r
		}
		checkPort := checkListener.Addr().(*net.TCPAddr).Port
		checkServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/"+token {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})}
		checkDone := make(chan struct{})
		go func() { _ = checkServer.Serve(checkListener); close(checkDone) }()
		defer func() { _ = checkServer.Close(); <-checkDone }()
		checkRule := "AND,((IP-CIDR,127.0.0.1/32),(DST-PORT," + strconv.Itoa(checkPort) + ")),DIRECT"
		cfg := Proxy{"mixed-port": port, "bind-address": "127.0.0.1", "allow-lan": false, "mode": "rule", "log-level": "silent", "ipv6": true, "authentication": []string{"probe:" + token}, "proxies": []any{copied}, "rules": []string{checkRule, "MATCH,probe"}}
		if resolver := text(p, "__ech-resolver"); resolver != "" {
			dns := Proxy{"enable": true, "nameserver": []string{"system"}, "proxy-server-nameserver": []string{resolver}}
			if name := text(object(p, "ech-opts"), "query-server-name"); name != "" {
				dns["proxy-server-nameserver-policy"] = Proxy{name: []string{resolver}}
			}
			cfg["dns"] = dns
		}
		b, err := yaml.Marshal(cfg)
		if err != nil {
			return r
		}
		file := filepath.Join(dir, "config.yaml")
		if os.WriteFile(file, b, 0600) != nil {
			return r
		}
		cmd = exec.CommandContext(ctx, e.Binary, "-d", dir, "-f", file)
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		proxy := http.ProxyURL(&url.URL{Scheme: "http", Host: address, User: url.UserPassword("probe", token)})
		tr = &http.Transport{Proxy: proxy, DisableKeepAlives: true}
		ready = func() bool {
			check, stop := context.WithTimeout(ctx, 100*time.Millisecond)
			defer stop()
			_, err := request(check, &http.Transport{Proxy: proxy, DisableKeepAlives: true}, "http://"+checkListener.Addr().String()+"/"+token, http.StatusNoContent)
			return err == nil
		}
	}
	if e.RootCAs != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: e.RootCAs, MinVersion: tls.VersionTLS12}
	}
	// Discard all child output: proxy configurations and errors can carry secrets.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if cmd.Start() != nil {
		return r
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
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
	latency, err := request(ctx, tr, e.Target, e.Expected)
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
