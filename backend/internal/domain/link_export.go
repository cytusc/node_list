package domain

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// 原始链接必须逐字保留；配置转换结果单独保存，禁止覆盖 rawLink。
// EnsureNodeLink creates a fallback, never original provenance. Keep rawLink untouched:
// upstream URLs can contain options a converter does not understand yet.
func EnsureNodeLink(n *Node) error {
	if n.RawLink != "" || n.GeneratedLink != "" {
		return nil
	}
	if len(n.RawConfig) == 0 {
		return shareLinkError("missing-source-config")
	}
	link, err := ConfigShareLink(n.RawConfig)
	if err == nil {
		n.GeneratedLink = link
	}
	return err
}

// ConfigShareLink exports a standard client-importable URI. Round-trip checking
// deliberately rejects unrepresentable options instead of saving a broken link.
func ConfigShareLink(p ProxyConfig) (string, error) {
	kind := str(p["type"])
	u := url.URL{Scheme: kind, Host: net.JoinHostPort(str(p["server"]), fmt.Sprint(p["port"])), Fragment: str(p["name"])}
	q := url.Values{}
	put := func(k string, v any) {
		if v != nil && fmt.Sprint(v) != "" {
			q.Set(k, fmt.Sprint(v))
		}
	}
	for input, output := range map[string]string{"servername": "sni", "sni": "sni", "client-fingerprint": "fp", "fingerprint": "pinSHA256", "flow": "flow", "encryption": "encryption", "packet-encoding": "packetEncoding", "congestion-controller": "congestion-controller", "udp-relay-mode": "udp-relay-mode", "obfs": "obfs", "obfs-password": "obfs-password"} {
		put(output, p[input])
	}
	if p["tls"] == true {
		q.Set("security", "tls")
	}
	if p["skip-cert-verify"] == true {
		q.Set("allowInsecure", "1")
	}
	if v, ok := p["alpn"]; ok {
		switch a := v.(type) {
		case []any:
			var parts []string
			for _, x := range a {
				parts = append(parts, fmt.Sprint(x))
			}
			q.Set("alpn", strings.Join(parts, ","))
		case []string:
			q.Set("alpn", strings.Join(a, ","))
		}
	}
	if r, ok := p["reality-opts"].(map[string]any); ok {
		q.Set("security", "reality")
		put("pbk", r["public-key"])
		put("sid", r["short-id"])
	}
	network := str(p["network"])
	if network != "" {
		q.Set("type", network)
	}
	switch network {
	case "ws":
		if o, ok := p["ws-opts"].(map[string]any); ok {
			put("path", o["path"])
			put("ed", o["max-early-data"])
			put("eh", o["early-data-header-name"])
			if o["v2ray-http-upgrade"] == true {
				q.Set("type", "httpupgrade")
			}
			if h, ok := o["headers"].(map[string]any); ok {
				put("host", h["Host"])
			}
		}
	case "grpc":
		if o, ok := p["grpc-opts"].(map[string]any); ok {
			put("serviceName", o["grpc-service-name"])
		}
	case "h2":
		if o, ok := p["h2-opts"].(map[string]any); ok {
			put("path", o["path"])
			switch hosts := o["host"].(type) {
			case []any:
				var a []string
				for _, h := range hosts {
					a = append(a, fmt.Sprint(h))
				}
				q.Set("host", strings.Join(a, ","))
			case []string:
				q.Set("host", strings.Join(hosts, ","))
			}
		}
	case "xhttp":
		if o, ok := p["xhttp-opts"].(map[string]any); ok {
			put("path", o["path"])
			put("host", o["host"])
			put("mode", o["mode"])
			b, _ := json.Marshal(o)
			q.Set("extra", string(b))
		}
	}
	var link string
	switch kind {
	case "http", "socks5":
		if kind == "http" && p["tls"] == true {
			u.Scheme = "https"
		}
		if str(p["username"]) != "" {
			u.User = url.UserPassword(str(p["username"]), str(p["password"]))
		}
	case "vless":
		u.User = url.User(str(p["uuid"]))
	case "trojan", "anytls", "hysteria2":
		u.User = url.User(str(p["password"]))
	case "tuic":
		if str(p["token"]) != "" {
			u.User = url.User(str(p["token"]))
		} else {
			u.User = url.UserPassword(str(p["uuid"]), str(p["password"]))
		}
	case "ss":
		u.User = url.User(base64.RawURLEncoding.EncodeToString([]byte(str(p["cipher"]) + ":" + str(p["password"]))))
		if plugin := str(p["plugin"]); plugin != "" {
			o, _ := p["plugin-opts"].(map[string]any)
			var parts []string
			if plugin == "obfs" {
				parts = []string{"obfs-local", "obfs=" + str(o["mode"])}
				if str(o["host"]) != "" {
					parts = append(parts, "obfs-host="+str(o["host"]))
				}
			} else {
				parts = []string{plugin}
				var keys []string
				for k := range o {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if o[k] == true {
						parts = append(parts, k)
					} else {
						parts = append(parts, k+"="+fmt.Sprint(o[k]))
					}
				}
			}
			q.Set("plugin", strings.Join(parts, ";"))
		}
	case "vmess":
		v := map[string]any{"v": "2", "ps": str(p["name"]), "add": str(p["server"]), "port": fmt.Sprint(p["port"]), "id": str(p["uuid"]), "aid": toInt(p["alterId"]), "scy": orDefault(str(p["cipher"]), "auto"), "net": orDefault(network, "tcp"), "type": "none"}
		for _, k := range []string{"sni", "fp", "alpn", "allowInsecure", "path", "host", "serviceName", "ech"} {
			if q.Get(k) != "" {
				v[k] = q.Get(k)
			}
		}
		if p["tls"] == true {
			if str(v["sni"]) == "" {
				v["sni"] = str(p["server"])
			}
			v["tls"] = "tls"
		}
		b, _ := json.Marshal(v)
		link = "vmess://" + base64.StdEncoding.EncodeToString(b)
	case "ssr":
		b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
		sq := url.Values{"remarks": {b64(str(p["name"]))}}
		for k, v := range map[string]string{"obfsparam": "obfs-param", "protoparam": "protocol-param"} {
			if str(p[v]) != "" {
				sq.Set(k, b64(str(p[v])))
			}
		}
		raw := fmt.Sprintf("%s:%v:%s:%s:%s:%s/?%s", p["server"], p["port"], p["protocol"], p["cipher"], p["obfs"], b64(str(p["password"])), sq.Encode())
		link = "ssr://" + b64(raw)
	default:
		return "", shareLinkError("unsupported-export-protocol")
	}
	if link == "" {
		u.RawQuery = q.Encode()
		link = u.String()
	}
	decoded, err := ShareLinkProxy(link, str(p["name"]))
	if err != nil {
		return "", err
	}
	if !equivalentShareConfig(p, decoded) {
		return "", shareLinkError("unrepresentable-export-options")
	}
	return link, nil
}

// Compare full credentials and transport, not just address/name. Share URIs do
// not encode Mihomo's local UDP toggle; protocol defaults are normalized here
// without changing persisted node identities.
func equivalentShareConfig(a, b ProxyConfig) bool {
	normalize := func(p ProxyConfig) string {
		c := connectionConfig(p)
		for _, k := range []string{"udp", "name"} {
			delete(c, k)
		}
		for k, v := range c {
			if v == nil || v == "" || v == false {
				delete(c, k)
			}
		}
		c["port"] = toInt(c["port"])
		for _, key := range []string{"sni", "servername"} {
			if c[key] == c["server"] {
				delete(c, key)
			}
		}
		if c["type"] == "vmess" {
			if toInt(c["alterId"]) == 0 {
				delete(c, "alterId")
			}
			if c["cipher"] == "auto" {
				delete(c, "cipher")
			}
		}
		if c["type"] == "trojan" || c["type"] == "anytls" || c["type"] == "hysteria2" || c["type"] == "tuic" {
			delete(c, "tls")
		}
		if c["type"] == "http" && c["tls"] != true {
			delete(c, "skip-cert-verify")
		}
		s, _ := canonicalJSON(c)
		return string(s)
	}
	return normalize(a) == normalize(b)
}
