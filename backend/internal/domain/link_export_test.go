package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigShareLinkRoundTrip(t *testing.T) {
	for _, p := range []ProxyConfig{
		{"name": "匿名 HTTP", "type": "http", "server": "51.159.225.197", "port": 19362, "skip-cert-verify": true},
		{"name": "IPv6 auth", "type": "socks5", "server": "2001:db8::1", "port": 1080, "username": "a@b", "password": "p:%25#密"},
		{"name": "HTTPS", "type": "http", "server": "example.com", "port": 443, "tls": true, "skip-cert-verify": true, "sni": "tls.example.com"},
		{"name": "ss", "type": "ss", "server": "example.com", "port": 443, "cipher": "aes-128-gcm", "password": "secret:#%"},
		{"name": "vmess", "type": "vmess", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": true, "network": "ws", "ws-opts": map[string]any{"path": "/a?b=c", "headers": map[string]any{"Host": "ws.example.com"}}},
		{"name": "vless", "type": "vless", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": true, "network": "grpc", "grpc-opts": map[string]any{"grpc-service-name": "test"}},
		{"name": "reality", "type": "vless", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": true, "client-fingerprint": "chrome", "reality-opts": map[string]any{"public-key": "public", "short-id": "ab"}},
		{"name": "trojan", "type": "trojan", "server": "example.com", "port": 443, "password": "p:a@%"},
		{"name": "hy2", "type": "hysteria2", "server": "example.com", "port": 443, "password": "secret", "obfs": "salamander", "obfs-password": "obfs-secret"},
		{"name": "anytls", "type": "anytls", "server": "example.com", "port": 443, "password": "secret"},
		{"name": "tuic", "type": "tuic", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "password": "secret"},
		{"name": "ssr", "type": "ssr", "server": "example.com", "port": 443, "cipher": "aes-128-cfb", "password": "secret", "protocol": "auth_sha1_v4", "obfs": "plain"},
	} {
		t.Run(str(p["name"]), func(t *testing.T) {
			link, err := ConfigShareLink(p)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := ShareLinkProxy(link, str(p["name"]))
			if err != nil || !equivalentShareConfig(p, decoded) {
				t.Fatalf("changed connection: %v", err)
			}
		})
	}
}

func TestOriginalURIAndGeneratedFallbackStaySeparate(t *testing.T) {
	p := ProxyConfig{"name": "n", "type": "http", "server": "example.com", "port": 8080}
	n := Node{RawConfig: p}
	if err := EnsureNodeLink(&n); err != nil || n.GeneratedLink == "" || n.RawLink != "" {
		t.Fatalf("fallback: %v", err)
	}
	original := "http://example.com:8080?futureOption=%2f#Original%20Name"
	n.RawLink = original
	if err := EnsureNodeLink(&n); err != nil || n.RawLink != original {
		t.Fatal("original URI overwritten")
	}
	before, _ := SnapshotFingerprint(Snapshot{Nodes: []Node{n}})
	n.GeneratedLink = "changed"
	after, _ := SnapshotFingerprint(Snapshot{Nodes: []Node{n}})
	if before != after {
		t.Fatal("generated provenance changed batch fingerprint")
	}
	n.ClearCredentials()
	raw, _ := json.Marshal(n)
	if strings.Contains(string(raw), "generatedLink") {
		t.Fatal("generated credentials leaked")
	}
	p["dialer-proxy"] = "required-chain"
	if _, err := ConfigShareLink(p); err == nil {
		t.Fatal("silently omitted required option")
	}
}

func TestSourcesPreserveUnnamedVMessAndDuplicateNames(t *testing.T) {
	p := ProxyConfig{"name": "same name", "type": "vmess", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111"}
	link, err := ConfigShareLink(p)
	if err != nil {
		t.Fatal(err)
	}
	links, err := ParseShareLinks(link)
	if err != nil || links["same name"] != link {
		t.Fatal("VMess ps discarded")
	}
	node := SummarizeProxy(p)
	key, _ := NodeIdentity(node)
	source := SourceForNode(node, &Sources{Links: links, Configs: map[string][]ProxyConfig{key: {p}}}, true)
	if source == nil || source.RawLink != link {
		t.Fatal("VMess original not attached")
	}
	a := "http://user:secret@example.com:8080#same"
	b := "http://other:password@example.com:8080#same"
	links, _ = ParseShareLinks(a + "\n" + b)
	if len(links) != 2 {
		t.Fatal("duplicate names lost original links")
	}
	proxy, _ := ShareLinkProxy(a, "same")
	node = SummarizeProxy(proxy)
	key, _ = NodeIdentity(node)
	source = SourceForNode(node, &Sources{Links: links, Configs: map[string][]ProxyConfig{key: {proxy}}}, true)
	if source == nil || source.RawLink != a {
		t.Fatal("attached wrong credentials")
	}
	// URL fragments are already percent-decoded by net/url: '+' stays literal.
	links, _ = ParseShareLinks("http://example.com:80#A+B%2520")
	if links["A+B%20"] == "" {
		t.Fatal("name double-decoded")
	}
}

func TestLateOriginalEnrichesSameBatchWithoutChangingHistory(t *testing.T) {
	p := ProxyConfig{"name": "n", "type": "http", "server": "example.com", "port": 8080}
	n := SummarizeProxy(p)
	n.RawConfig = p
	_ = EnsureNodeLink(&n)
	old, _ := json.Marshal(Snapshot{CheckedAt: "2026-10-26T00:00:00Z", Nodes: []Node{n}})
	n.RawLink = "http://example.com:8080#upstream%20original"
	incoming, _ := json.Marshal(Snapshot{CheckedAt: "2026-10-26T00:00:00Z", Nodes: []Node{n}})
	merged, changed, err := MergeSnapshotLinks(old, incoming)
	if err != nil || !changed {
		t.Fatalf("late URI ignored: %v", err)
	}
	var got Snapshot
	_ = json.Unmarshal(merged, &got)
	if got.Nodes[0].RawLink != n.RawLink {
		t.Fatal("raw link changed")
	}
	again, changed, err := MergeSnapshotLinks(merged, old)
	if err != nil || changed || string(again) != string(merged) {
		t.Fatal("config-only redelivery erased original")
	}
}
