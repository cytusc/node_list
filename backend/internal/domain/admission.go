package domain

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// NodeAdmissionError rejects deterministic configuration faults before storage.
// It does not infer availability from an upstream result or a network timeout.
// Unknown protocols/transports remain available for future compatibility work.
func NodeAdmissionError(node Node) error {
	if err := NodeCredentialError(node); err != nil {
		return err
	}
	if len(node.RawConfig) > 0 {
		return proxyAdmissionError(node.RawConfig)
	}
	if node.RawLink == "" {
		if !admissionProtocol(node.Type) {
			return nil
		}
		return admissionEndpoint(node.Server, node.Port)
	}
	scheme, _, _ := strings.Cut(node.RawLink, ":")
	if !admissionProtocol(scheme) {
		return nil
	}
	// Transport conversion can fail before TLS settings are validated. Check
	// this requirement independently, including unsupported transport links.
	if link, err := bracketBareIPv6Authority(strings.ReplaceAll(node.RawLink, "&amp;", "&")); err == nil {
		if u, err := url.Parse(link); err == nil && u.Query().Get("security") == "reality" && strings.TrimSpace(u.Query().Get("fp")) == "" {
			return shareLinkError("missing-reality-fingerprint")
		}
	}
	proxy, err := ShareLinkProxy(node.RawLink, node.Name)
	if err != nil {
		var detail *ShareLinkError
		if errors.As(err, &detail) {
			if strings.HasPrefix(detail.Code, "unsupported-") {
				return nil
			}
			return err
		}
		return shareLinkError("invalid-share-link")
	}
	return proxyAdmissionError(proxy)
}

func admissionProtocol(protocol string) bool {
	switch strings.ToLower(protocol) {
	case "http", "https", "socks", "socks5", "ss", "ssr", "vmess", "vless", "trojan", "anytls", "hysteria2", "hy2", "tuic":
		return true
	}
	return false
}

func proxyAdmissionError(proxy ProxyConfig) error {
	protocol, _ := proxy["type"].(string)
	if strings.TrimSpace(protocol) == "" {
		return shareLinkError("missing-proxy-type")
	}
	if !admissionProtocol(protocol) {
		return nil
	}
	server, _ := proxy["server"].(string)
	// JSON snapshots use float64 ports; Clash sources also use integer/string
	// ports. Do not silently truncate a fractional or overflowing port.
	port := str(proxy["port"])
	if port == "" {
		switch value := proxy["port"].(type) {
		case int:
			port = strconv.Itoa(value)
		case float64:
			port = strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	if err := admissionEndpoint(server, port); err != nil {
		return err
	}
	if err := checkValues(map[string]any(proxy)); err != nil {
		return err
	}
	if _, reality := proxy["reality-opts"]; reality {
		if strings.TrimSpace(str(proxy["client-fingerprint"])) == "" {
			return shareLinkError("missing-reality-fingerprint")
		}
	}
	if protocol == "ss" || protocol == "ssr" {
		if strings.TrimSpace(str(proxy["cipher"])) == "" {
			return shareLinkError("missing-credentials")
		}
	}
	if protocol == "socks5" || protocol == "socks" {
		user, password := str(proxy["username"]), str(proxy["password"])
		if (user == "") != (password == "") {
			return shareLinkError("incomplete-proxy-auth")
		}
	}
	return nil
}

func admissionEndpoint(server, port string) error {
	value, err := strconv.Atoi(port)
	if strings.TrimSpace(server) == "" || strings.TrimSpace(server) != server || err != nil || value < 1 || value > 65535 {
		return shareLinkError("invalid-endpoint")
	}
	return checkValues(server)
}
