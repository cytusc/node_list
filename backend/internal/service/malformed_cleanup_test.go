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
