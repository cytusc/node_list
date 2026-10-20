package domain

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestShadowsocksLinkEncodings(t *testing.T) {
	auth := "aes-128-gcm:secret"
	for _, link := range []string{
		"ss://aes-128-gcm:secret@example.com:443#Node",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte(auth)) + "@example.com:443#Node",
		"ss://" + base64.StdEncoding.EncodeToString([]byte(auth+"@example.com:443")) + "#Node",
	} {
		proxy, err := ShareLinkProxy(link, "Node")
		if err != nil {
			t.Fatal(err)
		}
		if proxy["cipher"] != "aes-128-gcm" || proxy["password"] != "secret" || proxy["server"] != "example.com" || proxy["port"] != 443 {
			t.Fatal("Shadowsocks endpoint or credentials were not preserved")
		}
	}
}

func TestURLUserinfoIsDecodedExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name, link, wantType, key, want string
	}{
		{
			name:     "trojan percent credentials",
			link:     "trojan://p%252F%25word@example.com:443?security=tls",
			wantType: "trojan", key: "password", want: "p%2F%word",
		},
		{
			name:     "shadowsocks percent credentials",
			link:     "ss://aes-128-gcm:p%252F%25word@example.com:443",
			wantType: "ss", key: "password", want: "p%2F%word",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, err := ShareLinkProxy(test.link, "Node")
			if err != nil {
				t.Fatalf("valid escaped credentials were rejected: %v", err)
			}
			if proxy["type"] != test.wantType || proxy[test.key] != test.want {
				t.Fatalf("userinfo was not preserved after one URL decode: %#v", proxy)
			}
		})
	}
}

func TestVLESSLinkMapsECHConfig(t *testing.T) {
	ech := "AEn+DQBFKwAgACABWIHUGj4u+PIggYXcR5JF0gYk3dCRioBW8uJq9H4mKAAIAAEAAQABAANAEnB1YmxpYy50bHMtZWNoLmRldgAA"
	link := "vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?encryption=none&security=tls&type=ws&ech=" + url.QueryEscape(ech)
	proxy, err := ShareLinkProxy(link, "Node")
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := proxy["ech-opts"].(map[string]any)
	if !ok || opts["enable"] != true || opts["config"] != ech {
		t.Fatalf("ECH options were not preserved in Mihomo format: %#v", proxy["ech-opts"])
	}
}

func TestVLESSLinkMapsQilanECHDNSSettings(t *testing.T) {
	for _, ech := range []string{
		"ip.gs+udp://8.8.8.8",
		"cloudflare-ech.com+https://dns.alidns.com/dns-query",
	} {
		t.Run(ech, func(t *testing.T) {
			link := "vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&ech=" + url.QueryEscape(ech)
			proxy, err := ShareLinkProxy(link, "Node")
			if err != nil {
				t.Fatal(err)
			}
			opts, ok := proxy["ech-opts"].(map[string]any)
			if !ok || opts["enable"] != true || opts["query-server-name"] != strings.SplitN(ech, "+", 2)[0] {
				t.Fatalf("ECH DNS query options were not mapped to Mihomo: %#v", proxy["ech-opts"])
			}
			if _, exists := opts["config"]; exists {
				t.Fatalf("DNS ECH setting must not be mistaken for a base64 config: %#v", opts)
			}
			if resolver := proxy["__ech-resolver"]; resolver != strings.SplitN(ech, "+", 2)[1] {
				t.Fatalf("ECH resolver URL was not retained for Mihomo DNS setup: %#v", resolver)
			}
		})
	}
}

func TestVLESSLinkIgnoresQilanTransportTagSuffix(t *testing.T) {
	link := "vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=reality&pbk=public-key&sid=abcd&type=tcp%23%F0%9F%92%A195%40oneclickvpnkeys"
	proxy, err := ShareLinkProxy(link, "Node")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := proxy["network"]; exists {
		t.Fatalf("default TCP network should not be overridden by a tag suffix: %#v", proxy["network"])
	}
}

func TestVLESSLinkRejectsMalformedECHConfig(t *testing.T) {
	link := "vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&ech=not-base64"
	if _, err := ShareLinkProxy(link, "Node"); err == nil {
		t.Fatal("malformed ECH config should be rejected")
	}
}

func TestShareLinkTLSEnabledExplicitPerScheme(t *testing.T) {
	// vless: TLS derives from the security parameter only, explicitly.
	proxy, err := ShareLinkProxy("vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?type=ws", "Node")
	if err != nil {
		t.Fatal(err)
	}
	if proxy["tls"] == true {
		t.Fatalf("vless without security must not enable TLS: %#v", proxy)
	}
	proxy, err = ShareLinkProxy("vless://123e4567-e89b-12d3-a456-426614174000@example.com:443?security=tls&type=ws", "Node")
	if err != nil {
		t.Fatal(err)
	}
	if proxy["tls"] != true {
		t.Fatalf("vless security=tls must enable TLS: %#v", proxy)
	}
	// trojan/anytls/hysteria2/tuic: TLS comes from the scheme, passed
	// explicitly at the call site rather than derived from query keys.
	for _, link := range []string{
		"trojan://secret@example.com:443",
		"tuic://123e4567-e89b-12d3-a456-426614174000@example.com:443",
	} {
		proxy, err := ShareLinkProxy(link, "Node")
		if err != nil {
			t.Fatalf("scheme-TLS link rejected: %v", err)
		}
		if proxy["tls"] != true {
			t.Fatalf("scheme-TLS outbound lost TLS: %#v", proxy)
		}
	}
}

func TestVmessLinkIdentityNormalizesNoOpSpellings(t *testing.T) {
	link := func(config string) string {
		return "vmess://" + base64.StdEncoding.EncodeToString([]byte(config))
	}
	// Same connection, different spellings: numeric vs string scalars, unset
	// vs empty/none/auto no-op fields, "---" garbage instead of "none".
	variants := []map[string]any{
		{"add": "x.example", "port": 18000, "id": "u1", "net": "tcp", "v": 2},
		{"add": "x.example", "port": "18000", "id": "u1", "net": "tcp", "v": "2"},
		{"add": "x.example", "port": "18000", "id": "u1", "net": "raw", "v": "2", "sni": "", "host": "", "path": "", "fp": "", "alpn": "", "scy": "", "type": "---"},
	}
	seen := map[string]bool{}
	for _, v := range variants {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		connection, ok := ShareLinkConnection(link(string(b)))
		if !ok {
			t.Fatalf("variant not parseable: %s", b)
		}
		seen[connection] = true
	}
	if len(seen) != 1 {
		t.Fatalf("variants collapsed to %d connections, want 1:\n%v", len(seen), seen)
	}
}

func TestVmessLinkIdentityKeepsRealDifferences(t *testing.T) {
	link := func(config map[string]any) string {
		b, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		return "vmess://" + base64.StdEncoding.EncodeToString(b)
	}
	base := map[string]any{"add": "x.example", "port": 18000, "id": "u1", "net": "tcp", "v": 2}
	// A credential/transport difference must remain distinct.
	distinct := []map[string]any{
		{"add": "x.example", "port": 18001, "id": "u1", "net": "tcp", "v": 2},
		{"add": "x.example", "port": 18000, "id": "u2", "net": "tcp", "v": 2},
		{"add": "x.example", "port": 18000, "id": "u1", "net": "ws", "path": "/a", "v": 2},
	}
	baseConn, ok := ShareLinkConnection(link(base))
	if !ok {
		t.Fatal("base not parseable")
	}
	for _, d := range distinct {
		conn, ok := ShareLinkConnection(link(d))
		if !ok {
			t.Fatalf("distinct variant not parseable: %+v", d)
		}
		if conn == baseConn {
			t.Fatalf("real difference collapsed: %+v", d)
		}
	}
}

func TestTUICLinkCredentialShapes(t *testing.T) {
	for _, test := range []struct {
		name, link string
		want       ProxyConfig
		wantErr    bool
	}{
		{
			name: "uuid token",
			link: "tuic://123e4567-e89b-12d3-a456-426614174000@example.com:443?sni=example.com&congestion-controller=bbr&udp-relay-mode=native&allow_insecure=1",
			want: ProxyConfig{"uuid": "123e4567-e89b-12d3-a456-426614174000", "password": ""},
		},
		{
			name: "compact uuid",
			link: "tuic://123e4567e89b12d3a456426614174000:secret@2001:db8::1:443",
			want: ProxyConfig{"uuid": "123e4567-e89b-12d3-a456-426614174000", "password": "secret", "server": "2001:db8::1", "port": 443},
		},
		{
			name: "bracketed ipv6",
			link: "tuic://123e4567e89b12d3a456426614174000:secret@[2001:db8::1]:443",
			want: ProxyConfig{"uuid": "123e4567-e89b-12d3-a456-426614174000", "password": "secret", "server": "2001:db8::1", "port": 443},
		},
		{
			name: "bare ipv6 without port",
			link: "tuic://123e4567e89b12d3a456426614174000:secret@2001:db8::1",
			want: ProxyConfig{"uuid": "123e4567-e89b-12d3-a456-426614174000", "password": "secret", "server": "2001:db8::1", "port": 443},
		},
		{
			name: "plain username and password",
			link: "tuic://user:pass@example.com:8443",
			want: ProxyConfig{"username": "user", "password": "pass"},
		},
		{
			name:    "bare username without password",
			link:    "tuic://user@example.com:443",
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, err := ShareLinkProxy(test.link, "Node")
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got %#v", proxy)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid tuic link was rejected: %v", err)
			}
			for key, want := range test.want {
				if proxy[key] != want {
					t.Fatalf("%s: got %#v, want %#v", key, proxy[key], want)
				}
			}
			if proxy["type"] != "tuic" || proxy["server"] == "" {
				t.Fatalf("tuic endpoint not preserved: %#v", proxy)
			}
		})
	}
}

// TestVlessLinkIdentityFoldsNoOpQuerySpelling verifies that vless variants
// differing only in no-op query spellings collapse to one identity. Found on
// production: the same endpoint surfaced 5 rows because sources spelled
// security=none, packetEncoding=none, headerType=none and a sni duplicating
// the host header inconsistently.
func TestVlessLinkIdentityFoldsNoOpQuerySpelling(t *testing.T) {
	links := []string{
		"vless://e13cf851-b4fd-413b-8d49-146e6eb8e5d4@platform106.golnamakcert.top:80?security=none&type=ws&path=%2Fplatform106&host=platform106.halalkhor.info&packetEncoding=none&sni=platform106.halalkhor.info#BR",
		"vless://e13cf851-b4fd-413b-8d49-146e6eb8e5d4@platform106.golnamakcert.top:80?security=none&type=ws&path=%2Fplatform106&host=platform106.halalkhor.info&packetEncoding=none#JP",
		"vless://e13cf851-b4fd-413b-8d49-146e6eb8e5d4@platform106.golnamakcert.top:80?sni=platform106.halalkhor.info&type=ws&host=platform106.halalkhor.info&path=/platform106#unknown",
		"vless://e13cf851-b4fd-413b-8d49-146e6eb8e5d4@platform106.golnamakcert.top:80?type=ws&host=platform106.halalkhor.info&path=/platform106",
		"vless://e13cf851-b4fd-413b-8d49-146e6eb8e5d4@platform106.golnamakcert.top:80?&type=ws&headerType=none&host=platform106.halalkhor.info&path=/platform106#unknown",
	}
	seen := map[string]bool{}
	for _, link := range links {
		conn, ok := ShareLinkConnection(link)
		if !ok {
			t.Fatalf("link failed to connect: %s", link)
		}
		seen[conn] = true
	}
	if len(seen) != 1 {
		t.Fatalf("expected no-op vless variants to fold to 1 identity, got %d", len(seen))
	}
}

// TestVlessLinkIdentityKeepsRealQueryDifferences guards the folding rules:
// security and header values that actually change the connection, a sni
// distinct from the transport host, and non-no-op values must retain
// separate identities.
func TestVlessLinkIdentityKeepsRealQueryDifferences(t *testing.T) {
	base := "vless://uuid@example.com:443?"
	cases := []struct {
		name string
		a, b string
	}{
		{"security tls vs none", "security=tls&type=ws", "security=none&type=ws"},
		{"sni differs from host", "type=ws&host=h1.example.com&sni=s1.example.com", "type=ws&host=h1.example.com"},
		{"fingerprint preserved", "security=tls&fp=chrome&type=ws", "security=tls&type=ws"},
		{"flow preserved", "flow=xtls-rprx-vision&type=ws", "type=ws"},
		{"path differs", "type=ws&path=/a", "type=ws&path=/b"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			connA, okA := ShareLinkConnection(base + test.a)
			connB, okB := ShareLinkConnection(base + test.b)
			if !okA || !okB {
				t.Fatalf("parse failed: %v %v", okA, okB)
			}
			if connA == connB {
				t.Fatalf("real difference collapsed: %s vs %s", connA, connB)
			}
		})
	}
}
