// Package transport exposes node traffic through the engine best suited to each
// protocol, and gives every engine its own concurrency budget.
//
// Running every protocol through one proxy kernel forces the slowest engine to
// set the pace for all of them. Protocols the standard library already speaks
// are dialled natively: no configuration reload, no listener port, no shared
// kernel state. Everything else is delegated to Mihomo, which remains the only
// engine that implements VLESS, VMess, Trojan, Shadowsocks and Hysteria.
package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// Outbound is one node's isolated traffic path.
type Outbound struct {
	// Client issues HTTP requests through the node.
	Client *http.Client
	// Label identifies the path in diagnostics without leaking credentials.
	Label string
}

// Engine prepares isolated traffic paths for a set of nodes.
type Engine interface {
	// Name identifies the engine in logs and diagnostics.
	Name() string
	// Supports reports whether the engine can carry this protocol.
	Supports(protocol string) bool
	// Reserve prepares paths for the given nodes and returns them keyed by
	// node index. The returned release function must be called when the wave
	// finishes.
	Reserve(ctx context.Context, nodes []domain.Node) (map[int]*Outbound, func(), error)
}

// Router picks the engine for a protocol. Adding a kernel is a matter of
// implementing Engine and registering it; nothing else changes.
type Router struct {
	engines  []Engine
	fallback Engine
	byProto  map[string]Engine
}

// NewRouter builds a router over the given engines, in priority order.
func NewRouter(engines ...Engine) *Router {
	r := &Router{engines: engines, byProto: make(map[string]Engine, 16)}
	for _, e := range engines {
		for _, proto := range knownProtocols {
			if _, taken := r.byProto[proto]; !taken && e.Supports(proto) {
				r.byProto[proto] = e
			}
		}
	}
	return r
}

// WithFallback sets the engine used for protocols no registered engine claims.
func (r *Router) WithFallback(e Engine) *Router {
	r.fallback = e
	return r
}

// knownProtocols is the union of protocol names seen in subscriptions.
var knownProtocols = []string{
	"http", "https", "socks5", "socks",
	"vless", "vmess", "trojan", "ss", "ssr",
	"hysteria", "hysteria2", "tuic", "anytls", "wireguard",
}

// EngineFor resolves the engine that should carry a node.
func (r *Router) EngineFor(node domain.Node) (Engine, bool) {
	protocol := strings.ToLower(node.Type)
	if protocol == "" && len(node.RawConfig) > 0 {
		if v, ok := node.RawConfig["type"].(string); ok {
			protocol = strings.ToLower(v)
		}
	}
	if e, ok := r.byProto[protocol]; ok {
		return e, true
	}
	if r.fallback != nil {
		return r.fallback, true
	}
	return nil, false
}

// Group splits a wave by engine so each engine reloads at most once and only
// when it actually has work.
func (r *Router) Group(nodes []domain.Node) map[Engine][]int {
	out := make(map[Engine][]int, len(r.engines))
	for i, node := range nodes {
		e, ok := r.EngineFor(node)
		if !ok {
			continue
		}
		out[e] = append(out[e], i)
	}
	return out
}

// NativeEngine dials HTTP/HTTPS and SOCKS5 nodes with the standard library.
// It owns no external state, so reservation is allocation only: no reload, no
// listener, no port exhaustion, and concurrency is bounded solely by the caller.
type NativeEngine struct{}

// NewNativeEngine builds the native engine.
func NewNativeEngine() *NativeEngine { return &NativeEngine{} }

// Name implements Engine.
func (e *NativeEngine) Name() string { return "native" }

// Supports implements Engine. Mihomo also handles these, but the native path is
// strictly cheaper because it needs no configuration reload.
func (e *NativeEngine) Supports(protocol string) bool {
	switch strings.ToLower(protocol) {
	case "http", "https", "socks5", "socks":
		return true
	}
	return false
}

// Reserve implements Engine.
func (e *NativeEngine) Reserve(ctx context.Context, nodes []domain.Node) (map[int]*Outbound, func(), error) {
	out := make(map[int]*Outbound, len(nodes))
	for i, node := range nodes {
		config := outboundConfig(node)
		client, err := nativeClient(config)
		if err != nil {
			continue
		}
		out[i] = &Outbound{Client: client, Label: "native:" + node.Type}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("native engine cannot carry any node in this wave")
	}
	return out, func() {
		for _, o := range out {
			o.Client.CloseIdleConnections()
		}
	}, nil
}

// outboundConfig resolves the configuration describing a node's endpoint.
// Share links are converted locally; credentials never leave the detector.
func outboundConfig(node domain.Node) domain.ProxyConfig {
	if len(node.RawConfig) > 0 {
		return node.RawConfig
	}
	if node.RawLink != "" {
		if config, err := domain.ShareLinkProxy(node.RawLink, node.Name); err == nil {
			return config
		}
	}
	return domain.ProxyConfig{
		"name":   node.Name,
		"type":   node.Type,
		"server": node.Server,
		"port":   atoi(node.Port),
	}
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return n
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// nativeClient builds an HTTP client whose traffic goes through one node.
func nativeClient(config domain.ProxyConfig) (*http.Client, error) {
	proxyURL, err := outboundURL(config)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		DialContext:         (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 8 * time.Second,
		// Exit-IP endpoints are plain HTTP or public TLS; the node's own
		// certificate policy (skip-cert-verify) does not apply to the
		// destination, only to the node tunnel, so verification stays on.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		// SOCKS5 needs a dialer that speaks the upstream handshake.
		DialTLSContext: nil,
	}
	if strings.EqualFold(fmt.Sprint(config["type"]), "socks5") || strings.EqualFold(fmt.Sprint(config["type"]), "socks") {
		socksURL, err := url.Parse(proxyURL.String())
		if err != nil {
			return nil, err
		}
		var auth *proxy.Auth
		if u := socksURL.User; u != nil {
			password, _ := u.Password()
			auth = &proxy.Auth{User: u.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", socksURL.Host, auth, &net.Dialer{Timeout: 8 * time.Second})
		if err != nil {
			return nil, err
		}
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   0, // per-request deadlines are set by the caller
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// outboundURL renders the proxy URL for an HTTP or SOCKS5 node.
func outboundURL(config domain.ProxyConfig) (*url.URL, error) {
	nodeType := strings.ToLower(fmt.Sprint(config["type"]))
	host := fmt.Sprint(config["server"])
	port := 0
	switch v := config["port"].(type) {
	case int:
		port = v
	case float64:
		port = int(v)
	case string:
		port = atoi(v)
	}
	if host == "" || port <= 0 {
		return nil, fmt.Errorf("incomplete endpoint for %s", nodeType)
	}
	scheme := "http"
	if nodeType == "socks5" || nodeType == "socks" {
		scheme = "socks5"
	} else if truthy(config["tls"]) || truthy(config["https"]) {
		scheme = "https"
	}
	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, fmt.Sprint(port))}
	if username := fmt.Sprint(config["username"]); username != "" && username != "<nil>" {
		password := ""
		if v, ok := config["password"].(string); ok {
			password = v
		}
		if password != "" {
			u.User = url.UserPassword(username, password)
		} else {
			u.User = url.User(username)
		}
	}
	return u, nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

// MihomoEngine loads batches into one or more Mihomo instances.
//
// Instances are used round-robin: when a second controller is configured the
// next wave reloads the idle instance while the previous wave's probes are
// still finishing, which removes the reload from the critical path entirely.
type MihomoEngine struct {
	configs    []MihomoConfig
	secret     string
	apiTimeout time.Duration

	mu       sync.Mutex
	next     int
	current  []domain.ProxyConfig
	reloadAt time.Time
	reloads  int64
	skipped  int64
}

// MihomoConfig is one Mihomo instance: its controller and its proxy entry.
type MihomoConfig struct {
	Controller string
	ProxyURL   string
	Secret     string
}

// NewMihomoEngine builds an engine over the given instances.
func NewMihomoEngine(instances []MihomoConfig, timeout time.Duration) *MihomoEngine {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &MihomoEngine{configs: instances, apiTimeout: timeout}
}

// Name implements Engine.
func (e *MihomoEngine) Name() string { return "mihomo" }

// Supports implements Engine: everything the standard library cannot dial.
func (e *MihomoEngine) Supports(protocol string) bool {
	switch strings.ToLower(protocol) {
	case "http", "https", "socks5", "socks":
		return false
	}
	return true
}

// Stats reports reload pressure so the scheduler can be tuned.
func (e *MihomoEngine) Stats() (reloads, skipped int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reloads, e.skipped
}

// Reserve loads the wave's proxies into one instance and returns one HTTP
// client per node bound to its own listener port.
func (e *MihomoEngine) Reserve(ctx context.Context, nodes []domain.Node) (map[int]*Outbound, func(), error) {
	if len(e.configs) == 0 {
		return nil, nil, fmt.Errorf("no Mihomo instance configured")
	}
	proxies := make([]domain.ProxyConfig, 0, len(nodes))
	indexes := make([]int, 0, len(nodes))
	for i, node := range nodes {
		config := outboundConfig(node)
		if config == nil {
			continue
		}
		proxies = append(proxies, config)
		indexes = append(indexes, i)
	}
	if len(proxies) == 0 {
		return nil, nil, fmt.Errorf("mihomo engine cannot carry any node in this wave")
	}

	e.mu.Lock()
	slot := e.next % len(e.configs)
	e.next++
	instance := e.configs[slot]
	same := sameProxySet(e.current, proxies)
	if same {
		e.skipped++
	}
	e.mu.Unlock()

	// An unchanged configuration needs no reload: Mihomo already serves it.
	// Waves that only replace a few nodes still reload, because dropping a
	// listener requires a configuration change.
	if !same {
		urls, err := ConfigureProxyBatch(ctx, instance, proxies)
		if err != nil {
			return nil, nil, err
		}
		e.mu.Lock()
		e.current = proxies
		e.reloadAt = time.Now()
		e.reloads++
		e.mu.Unlock()
		_ = urls
	}
	urls, err := listenerURLs(instance.ProxyURL, len(proxies))
	if err != nil {
		return nil, nil, err
	}
	out := make(map[int]*Outbound, len(proxies))
	for slot, proxy := range proxies {
		_ = proxy
		u := urls[slot]
		out[indexes[slot]] = &Outbound{Client: listenerClient(u), Label: u}
	}
	return out, func() {
		for _, o := range out {
			o.Client.CloseIdleConnections()
		}
	}, nil
}

func listenerClient(target string) *http.Client {
	u, err := url.Parse(target)
	if err != nil {
		u = &url.URL{Scheme: "http"}
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(u),
			DialContext:         (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 8 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// sameProxySet compares the requested proxies with the loaded configuration.
func sameProxySet(current, next []domain.ProxyConfig) bool {
	if len(current) != len(next) {
		return false
	}
	left := make([]string, len(current))
	right := make([]string, len(next))
	for i, p := range current {
		left[i] = proxyFingerprint(p)
	}
	for i, p := range next {
		right[i] = proxyFingerprint(p)
	}
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// proxyFingerprint hashes a proxy definition. Credentials stay in-process: the
// value is only used to decide whether a reload is needed.
func proxyFingerprint(p domain.ProxyConfig) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(fmt.Sprint(p[k]))
		b.WriteString(";")
	}
	return b.String()
}

// ProbeHTTP reports whether a node's tunnel can reach a plain HTTPS host.
// It is the reachability control used by the scheduled patrol: a node that
// cannot reach the control while the host itself can is genuinely dead.
func ProbeHTTP(ctx context.Context, client *http.Client) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "node-explorer-github-patrol")
	response, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
	}()
	return response.StatusCode == 200, nil
}
