// Package liveness is the sole owner of node availability. It has no dependency
// on task queues, the shared controller, exit checks or node lifecycle workers.
package liveness

import (
	"fmt"

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
func XrayOutbound(p Proxy) (Proxy, string) {
	protocol := text(p, "type")
	allowed := map[string]bool{}
	for _, k := range strings.Fields("name type server port udp tfo tcp-fast-open tls servername sni alpn client-fingerprint skip-cert-verify network ws-opts grpc-opts xhttp-opts reality-opts") {
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
	for key, network := range map[string]string{"ws-opts": "ws", "grpc-opts": "grpc", "xhttp-opts": "xhttp"} {
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
	if flag(p, "skip-cert-verify") {
		return nil, "insecure_tls"
	}
	if flag(p, "tfo") || flag(p, "tcp-fast-open") {
		return nil, "socket_options"
	}
	if protocol == "vmess" && text(p, "alterId") != "" && text(p, "alterId") != "0" {
		return nil, "legacy_vmess"
	}
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
			if k != "path" && k != "headers" {
				return nil, "ws_options"
			}
		}
		stream["network"] = "ws"
		stream["wsSettings"] = opts
	case "grpc":
		opts := object(p, "grpc-opts")
		for k := range opts {
			if k != "grpc-service-name" {
				return nil, "grpc_options"
			}
		}
		stream["network"] = "grpc"
		stream["grpcSettings"] = Proxy{"serviceName": opts["grpc-service-name"]}
	case "xhttp":
		opts := object(p, "xhttp-opts")
		for k := range opts {
			if k != "path" && k != "host" && k != "mode" {
				return nil, "xhttp_options"
			}
		}
		stream["network"] = "xhttp"
		stream["xhttpSettings"] = opts
	default:
		return nil, "transport"
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
		case "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
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
