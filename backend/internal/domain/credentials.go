package domain

import (
	"encoding/json"
	"net/url"
	"strings"
)

// NodeCredentialError checks required fields, independently of detector
// transport support. Run it after source enrichment: a complete raw config
// takes precedence over an incomplete share link, just as it does when dialing.
// Unrecognized or malformed formats remain observable for compatibility work.
func NodeCredentialError(node Node) error {
	if len(node.RawConfig) > 0 {
		return proxyCredentialError(node.RawConfig)
	}
	if node.RawLink == "" {
		return proxyCredentialError(ProxyConfig{"type": strings.ToLower(node.Type)})
	}
	if strings.HasPrefix(node.RawLink, "vmess://") {
		raw, err := decodeBase64(strings.TrimPrefix(node.RawLink, "vmess://"))
		if err != nil {
			return nil
		}
		var config map[string]any
		if json.Unmarshal([]byte(raw), &config) != nil {
			return nil
		}
		return proxyCredentialError(ProxyConfig{"type": "vmess", "uuid": config["id"]})
	}
	link, err := bracketBareIPv6Authority(strings.ReplaceAll(node.RawLink, "&amp;", "&"))
	if err != nil {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}
	switch strings.ToLower(u.Scheme) {
	case "vless":
		if strings.TrimSpace(u.User.Username()) == "" {
			return shareLinkError("missing-credentials")
		}
	case "trojan", "anytls", "hysteria2", "hy2", "tuic":
		password, _ := u.User.Password()
		if strings.TrimSpace(u.User.Username()) == "" && strings.TrimSpace(password) == "" {
			return shareLinkError("missing-credentials")
		}
	default:
		return nil
	}
	q := u.Query()
	if q.Get("security") == "reality" && strings.TrimSpace(first(q.Get, "pbk", "publicKey", "public-key", "public_key")) == "" {
		return shareLinkError("missing-reality-key")
	}
	return nil
}

func proxyCredentialError(proxy ProxyConfig) error {
	present := func(key string) bool {
		value, _ := proxy[key].(string)
		return strings.TrimSpace(value) != ""
	}
	switch proxy["type"] {
	case "vless", "vmess":
		if !present("uuid") {
			return shareLinkError("missing-credentials")
		}
	case "trojan", "anytls", "hysteria2", "hy2":
		if !present("password") {
			return shareLinkError("missing-credentials")
		}
	case "tuic":
		if !present("token") && !present("uuid") && !present("username") {
			return shareLinkError("missing-credentials")
		}
	}
	if value, exists := proxy["reality-opts"]; exists {
		var publicKey string
		switch opts := value.(type) {
		case map[string]any:
			publicKey, _ = opts["public-key"].(string)
		case ProxyConfig:
			publicKey, _ = opts["public-key"].(string)
		}
		if strings.TrimSpace(publicKey) == "" {
			return shareLinkError("missing-reality-key")
		}
	}
	return nil
}
