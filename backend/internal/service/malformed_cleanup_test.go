package service

import (
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestMalformedReasonPreservesShareLinksTheParserCannotRepresent(t *testing.T) {
	node := domain.Node{
		Name:    "node",
		Type:    "vless",
		Port:    "443",
		RawLink: "vless://123e4567-e89b-12d3-a456-426614174000@edge.example:443?encryption=none&security=tls&type=unsupported",
	}
	if _, err := domain.ShareLinkProxy(node.RawLink, node.Name); err == nil {
		t.Fatal("fixture must exercise an unsupported share-link parameter")
	}
	if got := malformedReason(node); got != "" {
		t.Fatalf("parser limitation must not be treated as proof of malformed data: %q", got)
	}
}

func TestMalformedReasonStillFindsMissingConnectionConfig(t *testing.T) {
	if got := malformedReason(domain.Node{Type: "vless"}); got != "missing-full-config" {
		t.Fatalf("expected missing connection config, got %q", got)
	}
}

func TestMalformedReasonFindsRequiredFieldsMissingFromKnownConfig(t *testing.T) {
	for name, config := range map[string]domain.ProxyConfig{
		"server": {"type": "vless", "port": 443, "uuid": "123e4567-e89b-12d3-a456-426614174000"},
		"port":   {"type": "vless", "server": "edge.example", "uuid": "123e4567-e89b-12d3-a456-426614174000"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := malformedReason(domain.Node{RawConfig: config}); got != "missing-required-connection-field" {
				t.Fatalf("expected missing required field, got %q", got)
			}
		})
	}
}

func TestMalformedReasonFindsMissingVLESSAuthenticationAndRealityKey(t *testing.T) {
	for name, node := range map[string]domain.Node{
		"UUID": {
			Type: "vless", RawLink: "vless://@edge.example:443?security=tls",
		},
		"Reality public key": {
			Type: "vless", RawLink: "vless://123e4567-e89b-12d3-a456-426614174000@edge.example:443?security=reality&type=grpc",
		},
		"RawConfig UUID": {
			RawConfig: domain.ProxyConfig{"type": "vless", "server": "edge.example", "port": 443},
		},
		"RawConfig Reality public key": {
			RawConfig: domain.ProxyConfig{
				"type": "vless", "server": "edge.example", "port": 443,
				"uuid":         "123e4567-e89b-12d3-a456-426614174000",
				"reality-opts": map[string]any{"short-id": "abcd"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := malformedReason(node); got == "" {
				t.Fatal("configuration missing a required authentication field was accepted")
			}
		})
	}
}

func TestMalformedReasonPreservesCompleteConfigAndAlternatePortRange(t *testing.T) {
	for _, config := range []domain.ProxyConfig{
		{"type": "vless", "server": "edge.example", "port": 443, "uuid": "123e4567-e89b-12d3-a456-426614174000"},
		{"type": "hysteria2", "server": "edge.example", "ports": "2000-3000", "password": "secret"},
	} {
		if got := malformedReason(domain.Node{RawConfig: config}); got != "" {
			t.Fatalf("complete config was marked malformed: %q", got)
		}
	}
}

func TestMalformedReasonAlignsTUICCredentialShapesWithParser(t *testing.T) {
	// user:pass over a rawLink is a valid tuic v5 credential shape after P1-5:
	// the audit channel must never veto it as a malformed UUID.
	plain := domain.Node{
		Type:    "tuic",
		Port:    "443",
		RawLink: "tuic://operator%40example.com:secret@edge.example:443?sni=example.com&congestion_control=bbr",
	}
	if _, err := domain.ShareLinkProxy(plain.RawLink, "tuic"); err != nil {
		t.Fatalf("parser must accept the user:pass credential shape: %v", err)
	}
	if got := malformedReason(plain); got != "" {
		t.Fatalf("valid tuic user:pass link must be preserved by the audit channel: %q", got)
	}
	// A username that is a UUID with a password still provides UUID evidence.
	if got := malformedReason(domain.Node{
		Type:    "tuic",
		Port:    "443",
		RawLink: "tuic://123e4567-e89b-12d3-a456-426614174001@edge.example:443?sni=example.com",
	}); got != "" {
		t.Fatalf("clean UUID-token tuic link must be preserved: %q", got)
	}
	// A genuinely damaged UUID (both password absent and shape invalid) stays a candidate.
	if got := malformedReason(domain.Node{
		Type:    "tuic",
		Port:    "443",
		RawLink: "tuic://zzzz-not-a-uuid@edge.example:443?sni=example.com",
	}); got != "invalid-tuic-uuid" {
		t.Fatalf("damaged bare-username UUID must stay a deletion candidate: %q", got)
	}
}
