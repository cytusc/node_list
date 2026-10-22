package domain

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNodeCredentialErrorDistinguishesMissingFieldsFromCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		node Node
		code string
	}{
		{"vless missing UUID", Node{RawLink: "vless://example.com:443"}, "missing-credentials"},
		{"trojan missing password", Node{RawLink: "trojan://example.com:443"}, "missing-credentials"},
		{"hy2 missing auth", Node{RawLink: "hy2://example.com:443"}, "missing-credentials"},
		{"tuic missing token", Node{RawLink: "tuic://example.com:443"}, "missing-credentials"},
		{"reality missing public key", Node{RawLink: "vless://private-secret@example.com:443?security=reality&type=unsupported"}, "missing-reality-key"},
		{"complete reality", Node{RawLink: "vless://user@example.com:443?security=reality&pbk=key"}, ""},
		{"public key alias", Node{RawLink: "vless://user@example.com:443?security=reality&publicKey=key"}, ""},
		{"unsupported transport retained", Node{RawLink: "vless://user@example.com:443?type=unsupported"}, ""},
		{"unsupported format retained", Node{RawLink: "future-protocol://example.com:443"}, ""},
		{"optional HTTP auth", Node{Type: "http"}, ""},
		{"vless summary has no credentials", Node{Type: "vless"}, "missing-credentials"},
		{"vmess missing ID", Node{RawLink: "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"example.com","port":443}`))}, "missing-credentials"},
		{"config missing password", Node{RawConfig: ProxyConfig{"type": "trojan"}}, "missing-credentials"},
		{"config missing public key", Node{RawConfig: ProxyConfig{"type": "vless", "uuid": "user", "reality-opts": map[string]any{}}}, "missing-reality-key"},
		{"config repairs link", Node{RawLink: "trojan://example.com:443", RawConfig: ProxyConfig{"type": "trojan", "password": "secret"}}, ""},
		{"tuic v4 token", Node{RawConfig: ProxyConfig{"type": "tuic", "token": "secret"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NodeCredentialError(tc.node)
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
