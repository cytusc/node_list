// Package liveness is the sole owner of node availability. It has no dependency
// on task queues, the shared controller, exit checks or node lifecycle workers.
package liveness

import (
	"fmt"
	"strconv"

	"strings"

	"github.com/cytusc/node_list/backend/internal/proxyprocess"
)

type Proxy = map[string]any

func text(p Proxy, key string) string {
	if v, ok := p[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}
func flag(p Proxy, key string) bool    { b, _ := p[key].(bool); return b }
func object(p Proxy, key string) Proxy { v, _ := p[key].(map[string]any); return v }

// Decode adapts the common stored node format.
func Decode(raw []byte) (Proxy, error) { return proxyprocess.Decode(raw) }

// XrayOutbound is deliberately conservative: an unmodelled connection option
// goes to Mihomo, never gets silently dropped. Version: Xray 26.10.21.
//
// Options Xray models natively are mapped here instead of forcing a
// Mihomo fallback: HTTP/H2 transports, HTTPUpgrade + early-data,
// XHTTP extensions (headers, no-grpc-header, padding, xmux reuse),
// skip-cert-verify (allowInsecure), TCP fast open (sockopt) and
// VMess alterId. Falling back for those only made the verdict depend
// on which engine happened to run, not on whether the node works.
func XrayOutbound(p Proxy) (Proxy, string) {
	protocol := text(p, "type")
	allowed := map[string]bool{}
	for _, k := range strings.Fields("name type server port udp tfo tcp-fast-open tls servername sni alpn client-fingerprint fingerprint skip-cert-verify network ws-opts grpc-opts http-opts h2-opts xhttp-opts reality-opts") {
		allowed[k] = true
	}
	specific := map[string]string{"vless": "uuid flow", "vmess": "uuid cipher alterId", "trojan": "password", "ss": "cipher password", "http": "username password", "socks5": "username password"}
	keys, ok := specific[protocol]
	if !ok {
		return nil, "protocol"
	}
	for _, k := range strings.Fields(keys) {
		allowed[k] = true
	}
	for k := range p {
		if !allowed[k] {
			return nil, "option:" + k
		}
	}
	for _, k := range []string{"udp", "tls", "skip-cert-verify", "tfo", "tcp-fast-open"} {
		if v, ok := p[k]; ok {
			if _, ok = v.(bool); !ok {
				return nil, "option_type:" + k
			}
		}
	}
	for key, network := range map[string]string{"ws-opts": "ws", "grpc-opts": "grpc", "http-opts": "http", "h2-opts": "h2", "xhttp-opts": "xhttp"} {
		if v, ok := p[key]; ok {
			if _, valid := v.(map[string]any); !valid || text(p, "network") != network {
				return nil, "transport_options"
			}
		}
	}
	if v, ok := p["reality-opts"]; ok {
		if _, valid := v.(map[string]any); !valid {
			return nil, "reality_options"
		}
		if protocol != "vless" {
			return nil, "reality_protocol"
		}
	}
	// skip-cert-verify, TCP fast open and VMess alterId are all modelled
	// by Xray (tlsSettings.allowInsecure, streamSettings.sockopt,
	// users[].alterId), so they no longer force a Mihomo fallback.
	// A relay that forwards correctly with a wrong certificate is
	// usable with skip-verify; testing it under Xray with the same
	// flag is the honest verdict, not a stricter one.
	hasTLS := flag(p, "tls") || protocol == "trojan"
	if !hasTLS && len(object(p, "reality-opts")) == 0 {
		for _, key := range []string{"servername", "sni", "alpn", "client-fingerprint"} {
			if _, ok := p[key]; ok {
				return nil, "tls_options_without_tls"
			}
		}
	}
	if v, ok := p["reality-opts"]; ok {
		if len(v.(map[string]any)) == 0 {
			return nil, "empty_reality"
		}
		if _, ok := p["alpn"]; ok {
			return nil, "reality_alpn"
		}
	}
	stream := Proxy{"network": "raw"}
	network := text(p, "network")
	switch network {
	case "", "tcp":
	case "ws":
		opts := object(p, "ws-opts")
		for k := range opts {
			if k != "path" && k != "headers" && k != "v2ray-http-upgrade" && k != "max-early-data" && k != "early-data-header-name" {
				return nil, "ws_options"
			}
		}
		if flag(opts, "v2ray-http-upgrade") {
			// Mihomo encodes HTTPUpgrade as ws + v2ray-http-upgrade;
			// Xray has a dedicated httpupgrade network.
			stream["network"] = "httpupgrade"
			settings := Proxy{"path": opts["path"]}
			if host := wsHost(opts); host != "" {
				settings["host"] = host
			}
			stream["httpupgradeSettings"] = settings
		} else {
			stream["network"] = "ws"
			settings := Proxy{"path": opts["path"]}
			if headers, ok := opts["headers"]; ok {
				settings["headers"] = headers
			}
			if v, ok := opts["max-early-data"]; ok {
				settings["maxEarlyData"] = v
			}
			if v, ok := opts["early-data-header-name"]; ok {
				settings["earlyDataHeaderName"] = v
			}
			stream["wsSettings"] = settings
		}
	case "grpc":
		opts := object(p, "grpc-opts")
		for k := range opts {
			if k != "grpc-service-name" {
				return nil, "grpc_options"
			}
		}
		stream["network"] = "grpc"
		stream["grpcSettings"] = Proxy{"serviceName": opts["grpc-service-name"]}
	case "http":
		opts := object(p, "http-opts")
		for k := range opts {
			if k != "path" && k != "headers" {
				return nil, "http_options"
			}
		}
		stream["network"] = "http"
		settings := Proxy{}
		if v := firstString(opts["path"]); v != "" {
			settings["path"] = v
		}
		if host := httpHost(opts); host != nil {
			settings["host"] = host
		}
		stream["httpSettings"] = settings
	case "h2":
		opts := object(p, "h2-opts")
		for k := range opts {
			if k != "path" && k != "host" {
				return nil, "h2_options"
			}
		}
		stream["network"] = "h2"
		settings := Proxy{"path": opts["path"]}
		if v, ok := opts["host"]; ok {
			settings["host"] = v
		}
		stream["h2Settings"] = settings
	case "xhttp":
		opts := object(p, "xhttp-opts")
		for k := range opts {
			switch k {
			case "path", "host", "mode", "headers", "no-grpc-header", "x-padding-bytes", "reuse-settings", "session-key", "session-placement", "sc-max-each-post-bytes", "sc-min-posts-interval-ms":
			default:
				return nil, "xhttp_options"
			}
		}
		stream["network"] = "xhttp"
		stream["xhttpSettings"] = xhttpSettings(opts)
	default:
		return nil, "transport"
	}
	if flag(p, "tfo") || flag(p, "tcp-fast-open") {
		stream["sockopt"] = Proxy{"tcpFastOpen": true}
	}
	sni := text(p, "servername")
	if sni == "" {
		sni = text(p, "sni")
	}
	reality := object(p, "reality-opts")
	tls := flag(p, "tls") || protocol == "trojan"
	if len(reality) > 0 {
		for k := range reality {
			if k != "public-key" && k != "short-id" {
				return nil, "reality_options"
			}
		}
		if network == "ws" || text(p, "client-fingerprint") == "" {
			return nil, "reality_transport"
		}
		stream["security"] = "reality"
		stream["realitySettings"] = Proxy{"serverName": sni, "fingerprint": p["client-fingerprint"], "publicKey": reality["public-key"], "shortId": reality["short-id"]}
	} else if tls {
		stream["security"] = "tls"
		settings := Proxy{"serverName": sni}
		for _, k := range []string{"alpn", "client-fingerprint"} {
			if v, ok := p[k]; ok {
				name := k
				if k == "client-fingerprint" {
					name = "fingerprint"
				}
				settings[name] = v
			}
		}
		if flag(p, "skip-cert-verify") {
			settings["allowInsecure"] = true
		}
		if v := text(p, "fingerprint"); v != "" {
			settings["pinnedPeerCertificateSha256"] = v
		}
		stream["tlsSettings"] = settings
	}
	out := Proxy{"protocol": protocol, "streamSettings": stream}
	endpoint := Proxy{"address": p["server"], "port": p["port"]}
	switch protocol {
	case "vless", "vmess":
		if text(p, "uuid") == "" {
			return nil, "missing_uuid"
		}
		if protocol == "vless" && !tls && len(reality) == 0 {
			return nil, "plaintext_vless"
		}
		user := Proxy{"id": p["uuid"]}
		if protocol == "vless" {
			user["encryption"] = "none"
			if f := text(p, "flow"); f != "" {
				user["flow"] = f
			}
		} else {
			user["security"] = "auto"
			if s := text(p, "cipher"); s != "" {
				user["security"] = s
			}
			if v, ok := p["alterId"]; ok {
				user["alterId"] = v
			}
		}
		endpoint["users"] = []any{user}
		out["settings"] = Proxy{"vnext": []any{endpoint}}
	case "trojan":
		if text(p, "password") == "" {
			return nil, "missing_password"
		}
		endpoint["password"] = p["password"]
		out["settings"] = Proxy{"servers": []any{endpoint}}
	case "ss":
		cipher := text(p, "cipher")
		switch cipher {
		case "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "xchacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
			// Legacy stream ciphers Xray still dials; rejecting them
			// only hid working nodes behind a Mihomo fallback.
			"aes-128-cfb", "aes-256-cfb", "chacha20", "chacha20-ietf":
		default:
			return nil, "ss_cipher"
		}
		if text(p, "password") == "" {
			return nil, "missing_password"
		}
		if tls || network != "" && network != "tcp" {
			return nil, "ss_transport"
		}
		out["protocol"] = "shadowsocks"
		endpoint["method"] = cipher
		endpoint["password"] = p["password"]
		out["settings"] = Proxy{"servers": []any{endpoint}}
	case "http", "socks5":
		if network != "" && network != "tcp" || len(reality) > 0 {
			return nil, "proxy_transport"
		}
		if text(p, "username") != "" || text(p, "password") != "" {
			endpoint["users"] = []any{Proxy{"user": text(p, "username"), "pass": text(p, "password")}}
		}
		if protocol == "socks5" {
			out["protocol"] = "socks"
		}
		out["settings"] = Proxy{"servers": []any{endpoint}}
	default:
		return nil, "protocol"
	}
	// Options valid for another protocol must never be silently reinterpreted.
	if text(p, "security") != "" || protocol != "vmess" && text(p, "alterId") != "" || protocol != "vless" && text(p, "flow") != "" {
		return nil, "protocol_options"
	}
	return out, ""
}

// firstString takes the first element of a string/list value, or "".
func firstString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		if len(t) > 0 {
			return t[0]
		}
	case []any:
		if len(t) > 0 {
			return fmt.Sprint(t[0])
		}
	}
	return ""
}

// wsHost extracts the Host header from ws-opts headers, if present.
func wsHost(opts Proxy) string {
	if headers, ok := opts["headers"].(map[string]any); ok {
		return text(headers, "Host")
	}
	return ""
}

// httpHost extracts the Host header list from http-opts headers.
func httpHost(opts Proxy) any {
	if headers, ok := opts["headers"].(map[string]any); ok {
		if v, ok := headers["Host"]; ok {
			return v
		}
	}
	return nil
}

func toIntValue(v any) any {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n
		}
		return t
	}
	return v
}

// xhttpSettings translates Mihomo kebab-case xhttp-opts into Xray
// camelCase xhttpSettings. Unknown keys were already rejected above,
// so this is a pure rename, never a silent drop.
func xhttpSettings(opts Proxy) Proxy {
	out := Proxy{}
	for k, v := range opts {
		switch k {
		case "path", "host", "mode", "headers":
			out[k] = v
		case "no-grpc-header":
			out["noGRPCHeader"] = v
		case "x-padding-bytes":
			out["xPaddingBytes"] = toIntValue(v)
		case "sc-max-each-post-bytes":
			out["scMaxEachPostBytes"] = toIntValue(v)
		case "sc-min-posts-interval-ms":
			out["scMinPostsIntervalMs"] = toIntValue(v)
		case "session-key":
			out["sessionKey"] = v
		case "session-placement":
			out["sessionPlacement"] = v
		case "reuse-settings":
			out["xmux"] = xhttpReuseSettings(v)
		}
	}
	return out
}

func xhttpReuseSettings(v any) Proxy {
	aliases := map[string]string{
		"max-concurrency": "maxConcurrency", "max-connections": "maxConnections",
		"c-max-reuse-times": "cMaxReuseTimes", "h-max-request-times": "hMaxRequestTimes",
		"h-max-reusable-secs": "hMaxReusableSecs", "h-keep-alive-period": "hKeepAlivePeriod",
	}
	out := Proxy{}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			if target, ok := aliases[k]; ok {
				out[target] = toIntValue(val)
			}
		}
	}
	return out
}
