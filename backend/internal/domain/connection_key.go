package domain

import "strings"

// ConnectionKey groups equivalent usable outbounds without changing persisted
// node identities or losing their observations and lifecycle history.
func ConnectionKey(n Node) string {
	proxy := n.RawConfig
	if len(proxy) == 0 {
		var err error
		proxy, err = ShareLinkProxy(n.RawLink, n.Name)
		if err != nil {
			// Source-only records lack credentials. Do not guess equivalence.
			return ""
		}
	}
	c := connectionConfig(proxy)
	if emptyString(c["server"]) || c["port"] == nil || emptyString(c["type"]) {
		return ""
	}
	c["server"] = strings.ToLower(str(c["server"]))
	c["port"] = toInt(c["port"])
	kind := str(c["type"])
	if kind == "vmess" || kind == "vless" {
		if uuid, err := normalizeUUID(str(c["uuid"])); err == nil {
			c["uuid"] = uuid
		}
	}
	if kind == "vmess" {
		c["alterId"] = toInt(c["alterId"])
		if emptyString(c["cipher"]) {
			c["cipher"] = "auto"
		}
	}
	// Explicit false and absent flags have the same Mihomo defaults.
	for _, key := range []string{"skip-cert-verify", "tfo", "mptcp"} {
		if v, ok := c[key].(bool); ok && !v {
			delete(c, key)
		}
	}
	if (kind == "vmess" || kind == "vless" || kind == "http" || kind == "socks5") && c["tls"] != true {
		for _, key := range []string{"servername", "sni", "skip-cert-verify", "client-fingerprint", "fingerprint", "alpn"} {
			delete(c, key)
		}
	}
	hash, err := canonicalHash(c)
	if err != nil {
		return ""
	}
	return hash
}
