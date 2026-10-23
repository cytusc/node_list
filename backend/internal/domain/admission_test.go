package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestNodeAdmissionError(t *testing.T) {
	for _, tc := range []struct {
		name string
		node Node
		code string
	}{
		{"missing credentials", Node{RawLink: "trojan://example.com:443"}, "missing-credentials"},
		{"malformed known link", Node{RawLink: "vmess://private-secret"}, "invalid-share-link"},
		{"invalid port", Node{RawLink: "http://example.com:70000"}, "invalid-endpoint"},
		{"missing endpoint", Node{Type: "http"}, "invalid-endpoint"},
		{"valid summary", Node{Type: "http", Server: "example.com", Port: "80"}, ""},
		{"anonymous HTTP allowed", Node{RawLink: "http://example.com:80"}, ""},
		{"anonymous SOCKS allowed", Node{RawLink: "socks5://example.com:1080"}, ""},
		{"partial SOCKS auth", Node{RawLink: "socks5://user@example.com:1080"}, "incomplete-proxy-auth"},
		{"SOCKS auth", Node{RawLink: "socks5://user:secret@example.com:1080"}, ""},
		{"Reality missing fingerprint", Node{RawLink: "vless://user@example.com:443?security=reality&pbk=key"}, "missing-reality-fingerprint"},
		{"Reality fingerprint checked before transport", Node{RawLink: "vless://user@example.com:443?security=reality&pbk=key&type=future"}, "missing-reality-fingerprint"},
		{"complete Reality", Node{RawLink: "vless://user@example.com:443?security=reality&pbk=key&fp=chrome"}, ""},
		{"unknown transport retained", Node{RawLink: "vless://user@example.com:443?type=future"}, ""},
		{"unknown protocol retained", Node{RawLink: "future://example.com:443"}, ""},
		{"unknown plugin retained", Node{RawLink: "ss://YWVzLTEyOC1nY206c2VjcmV0@example.com:443?plugin=future-plugin"}, ""},
		{"unknown plugin mode retained", Node{RawLink: "ss://YWVzLTEyOC1nY206c2VjcmV0@example.com:443?plugin=v2ray-plugin%3Bmode%3Dquic"}, ""},
		{"raw config wins over malformed link", Node{RawLink: "vmess://private-secret", RawConfig: ProxyConfig{"type": "http", "server": "example.com", "port": 80}}, ""},
		{"raw config missing protocol", Node{RawConfig: ProxyConfig{"server": "example.com", "port": 80}}, "missing-proxy-type"},
		{"raw config fractional port", Node{RawConfig: ProxyConfig{"type": "http", "server": "example.com", "port": 80.5}}, "invalid-endpoint"},
		{"raw config JSON port", Node{RawConfig: ProxyConfig{"type": "http", "server": "example.com", "port": float64(80)}}, ""},
		{"raw config string port", Node{RawConfig: ProxyConfig{"type": "http", "server": "example.com", "port": "80"}}, ""},
		{"SS missing cipher", Node{RawConfig: ProxyConfig{"type": "ss", "server": "example.com", "port": 443, "password": "secret"}}, "missing-credentials"},
		{"TUIC v5 empty password retained", Node{RawConfig: ProxyConfig{"type": "tuic", "server": "example.com", "port": 443, "uuid": "user"}}, ""},
		{"TUIC v4 token", Node{RawConfig: ProxyConfig{"type": "tuic", "server": "example.com", "port": 443, "token": "secret"}}, ""},
		{"raw config invalid text", Node{RawConfig: ProxyConfig{"type": "http", "server": "example.com", "port": 80, "password": "private-secret\n"}}, "control-character"},
		{"raw Reality missing fingerprint", Node{RawConfig: ProxyConfig{"type": "vless", "server": "example.com", "port": 443, "uuid": "user", "tls": true, "reality-opts": map[string]any{"public-key": "key"}}}, "missing-reality-fingerprint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NodeAdmissionError(tc.node)
			if tc.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var detail *ShareLinkError
			if !errors.As(err, &detail) || detail.Code != tc.code || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("unexpected or unsafe rejection: %v", err)
			}
		})
	}
}
