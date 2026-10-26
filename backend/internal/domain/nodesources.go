package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// detailPaths lists the upstream summary columns extracted from a Clash proxy.
var detailPaths = []struct {
	key  string
	path []string
}{
	{"cipher", []string{"cipher"}},
	{"flow", []string{"flow"}},
	{"client-fingerprint", []string{"client-fingerprint"}},
	{"alpn", []string{"alpn"}},
	{"skip-cert-verify", []string{"skip-cert-verify"}},
	{"ws-path", []string{"ws-opts", "path"}},
	{"ws-host", []string{"ws-opts", "headers", "Host"}},
	{"grpc-service-name", []string{"grpc-opts", "grpc-service-name"}},
	{"h2-path", []string{"h2-opts", "path"}},
	{"http-path", []string{"http-opts", "path"}},
	{"plugin", []string{"plugin"}},
	{"obfs", []string{"obfs"}},
	{"protocol", []string{"protocol"}},
	{"up", []string{"up"}},
	{"down", []string{"down"}},
	{"ports", []string{"ports"}},
	{"congestion-controller", []string{"congestion-controller"}},
	{"udp-relay-mode", []string{"udp-relay-mode"}},
	{"version", []string{"version"}},
}

func dig(value ProxyConfig, path []string) any {
	// ProxyConfig is a named map type; it does not satisfy a map[string]any
	// type assertion until converted. Nested YAML maps already use that type.
	current := any(map[string]any(value))
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func formatDetail(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s := formatDetail(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconvFloat(t)
	case int:
		return fmt.Sprintf("%d", t)
	case string:
		return strings.TrimSpace(t)
	default:
		b, _ := json.Marshal(t)
		return strings.TrimSpace(string(b))
	}
}

func strconvFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", f), "0"), ".")
}

// SummarizeProxy mirrors the upstream results summary so a reused display name
// never supplies another endpoint's credentials. Arrays use the upstream
// comma-space format, which keeps the identity stable across versions.
func SummarizeProxy(proxy ProxyConfig) Node {
	nodeType := str(proxy["type"])
	quic := nodeType == "hysteria" || nodeType == "hysteria2" || nodeType == "tuic"
	realityOpts, _ := proxy["reality-opts"].(map[string]any)
	reality := len(realityOpts) > 0
	tls := nodeType == "trojan" || nodeType == "anytls" || quic || truthy(proxy["tls"]) || reality
	sni := ""
	if tls {
		if v := str(proxy["servername"]); v != "" {
			sni = v
		} else {
			sni = str(proxy["sni"])
		}
	}
	network := str(proxy["network"])
	if network == "" {
		switch {
		case quic:
			network = "quic"
		case nodeType == "wireguard":
			network = "udp"
		default:
			network = "tcp"
		}
	}
	details := make([]Detail, 0, len(detailPaths))
	for _, entry := range detailPaths {
		v := dig(proxy, entry.path)
		formatted := formatDetail(v)
		if v == nil || formatted == "" {
			continue
		}
		details = append(details, Detail{K: entry.key, V: formatted})
	}
	return Node{
		Name:    str(proxy["name"]),
		Type:    nodeType,
		Server:  str(proxy["server"]),
		Port:    fmt.Sprintf("%v", proxy["port"]),
		TLS:     tls,
		Reality: reality,
		SNI:     sni,
		UDP:     quic || truthy(proxy["udp"]),
		Network: network,
		Details: details,
	}
}
