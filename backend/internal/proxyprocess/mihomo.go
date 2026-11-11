package proxyprocess

import (
	"context"
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

// Mihomo starts an authenticated loopback proxy with exactly one outbound.
// The returned cleanup kills and joins the child before removing private files;
// the done channel lets callers distinguish a kernel exit from a node failure.
func Mihomo(ctx context.Context, binary string, p Proxy) (*http.Transport, func(), <-chan struct{}, error) {
	dir, err := os.MkdirTemp("/tmp", "nl-")
	if err != nil {
		return nil, nil, nil, errors.New("private_directory_failed")
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanup()
		return nil, nil, nil, errors.New("listener_failed")
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
	copied["name"] = "probe"
	checkListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cleanup()
		return nil, nil, nil, errors.New("listener_failed")
	}
	checkPort := checkListener.Addr().(*net.TCPAddr).Port
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+token {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	})}
	served := make(chan struct{})
	go func() { _ = server.Serve(checkListener); close(served) }()
	previousCleanup := cleanup
	cleanup = func() { _ = server.Close(); <-served; previousCleanup() }
	// This single DIRECT rule is exclusively for local tunnel readiness.
	rule := "AND,((IP-CIDR,127.0.0.1/32),(DST-PORT," + strconv.Itoa(checkPort) + ")),DIRECT"
	cfg := Proxy{"mixed-port": port, "bind-address": "127.0.0.1", "allow-lan": false, "mode": "rule", "log-level": "silent", "ipv6": true, "authentication": []string{"probe:" + token}, "proxies": []any{copied}, "rules": []string{rule, "MATCH,probe"}}
	if resolver := text(p, "__ech-resolver"); resolver != "" {
		dns := Proxy{"enable": true, "nameserver": []string{"system"}, "proxy-server-nameserver": []string{resolver}}
		if ech, ok := p["ech-opts"].(map[string]any); ok && text(ech, "query-server-name") != "" {
			dns["proxy-server-nameserver-policy"] = Proxy{text(ech, "query-server-name"): []string{resolver}}
		}
		cfg["dns"] = dns
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		cleanup()
		return nil, nil, nil, errors.New("invalid_config")
	}
	file := filepath.Join(dir, "config.yaml")
	if os.WriteFile(file, b, 0600) != nil {
		cleanup()
		return nil, nil, nil, errors.New("config_write_failed")
	}
	cmd := exec.CommandContext(ctx, binary, "-d", dir, "-f", file)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	if cmd.Start() != nil {
		cleanup()
		return nil, nil, nil, errors.New("kernel_start_failed")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	cleanFiles := cleanup
	proxy := http.ProxyURL(&url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), User: url.UserPassword("probe", token)})
	tr := &http.Transport{Proxy: proxy, DisableKeepAlives: true}
	cleanup = func() { tr.CloseIdleConnections(); _ = cmd.Process.Kill(); <-done; cleanFiles() }
	readyClient := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		check, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		req, _ := http.NewRequestWithContext(check, http.MethodGet, "http://"+checkListener.Addr().String()+"/"+token, nil)
		res, e := readyClient.Do(req)
		ready := e == nil && res.StatusCode == 204
		if res != nil {
			_ = res.Body.Close()
		}
		stop()
		if ready {
			return tr, cleanup, done, nil
		}
		select {
		case <-ctx.Done():
			cleanup()
			return nil, nil, nil, errors.New("kernel_start_timeout")
		case <-done:
			cleanup()
			return nil, nil, nil, errors.New("kernel_rejected")
		case <-ticker.C:
		}
	}
}
