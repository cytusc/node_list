package domain

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestVLESSCustomIDMatchesMihomoUUIDMap(t *testing.T) {
	p, err := ShareLinkProxy("vless://custom-user@example.com:443?type=splithttp&security=tls", "中国 🇨🇳")
	if err != nil {
		t.Fatal(err)
	}
	// UUIDv5(nil, "custom-user"), as used by Mihomo's UUIDMap.
	if p["uuid"] != "73641eb6-f94b-5b94-b083-8bd4ddcd0ba6" || p["network"] != "xhttp" {
		t.Fatalf("custom ID/transport conversion: %v", p)
	}
	if _, err := ShareLinkProxy("vless://user%0Asecret@example.com:443", "node"); err == nil {
		t.Fatal("control character in credentials was accepted")
	}
}

func TestXHTTPExtraModeIsPreserved(t *testing.T) {
	p, err := ShareLinkProxy(`vless://custom-user@example.com:443?type=xhttp&extra=%7B%22mode%22%3A%22packet-up%22%2C%22xPaddingBytes%22%3A%22100-1000%22%7D`, "node")
	if err != nil {
		t.Fatal(err)
	}
	opts := p["xhttp-opts"].(map[string]any)
	if opts["mode"] != "packet-up" || opts["x-padding-bytes"] != "100-1000" {
		t.Fatalf("extra options lost: %v", opts)
	}
}

func TestXHTTPExtraMapsClientOptions(t *testing.T) {
	extra := `{"scMaxConcurrentPosts":32,"sessionIDKey":"session","sessionIDPlacement":"query","xmux":{"maxConcurrency":"16-32","maxConnections":0,"cMaxReuseTimes":0,"hMaxRequestTimes":"600-900","hMaxReusableSecs":"1800-3000","hKeepAlivePeriod":30}}`
	p, err := ShareLinkProxy("vless://custom-user@example.com:443?type=xhttp&extra="+url.QueryEscape(extra), "node")
	if err != nil {
		t.Fatal(err)
	}
	opts := p["xhttp-opts"].(map[string]any)
	reuse := opts["reuse-settings"].(map[string]any)
	if opts["session-key"] != "session" || opts["session-placement"] != "query" || reuse["max-concurrency"] != "16-32" || reuse["max-connections"] != "0" || reuse["h-max-request-times"] != "600-900" || reuse["h-keep-alive-period"] != 30 {
		t.Fatalf("client options lost: %#v", opts)
	}
	if _, exists := opts["sc-max-concurrent-posts"]; exists {
		t.Fatal("obsolete setting was emitted as an unsupported kernel option")
	}
	for _, extra := range []string{
		`{"xmux":{"unknown":1}}`, `{"unknown":1}`, `{"xmux":true}`,
		`{"xmux":{"maxConcurrency":-1}}`, `{"xmux":{"maxConcurrency":"32-16"}}`,
		`{"xmux":{"hKeepAlivePeriod":"10-20"}}`, `{"scMaxConcurrentPosts":true}`,
	} {
		if _, err := ShareLinkProxy("vless://user@example.com:443?type=xhttp&extra="+url.QueryEscape(extra), "node"); err == nil {
			t.Fatalf("unsupported or malformed options accepted: %s", extra)
		}
	}
}

func TestECHPreservesLiteralAndEscapedPluses(t *testing.T) {
	for _, ech := range []string{
		"ip.gs+udp://8.8.8.8",
		"cloudflare-ech.com+https://dns.alidns.com/dns-query",
		"AEn+DQBFKwAgACABWIHUGj4u+PIggYXcR5JF0gYk3dCRioBW8uJq9H4mKAAIAAEAAQABAANAEnB1YmxpYy50bHMtZWNoLmRldgAA",
	} {
		for _, value := range []string{ech, url.QueryEscape(ech)} {
			link := "vless://user@example.com:443?security=tls&sni=some+host&ech=" + value
			p, err := ShareLinkProxy(link, "node")
			if err != nil {
				t.Fatal(err)
			}
			opts := p["ech-opts"].(map[string]any)
			if strings.Contains(ech, "://") {
				name, resolver, _ := strings.Cut(ech, "+")
				if opts["query-server-name"] != name || p["__ech-resolver"] != resolver {
					t.Fatalf("ECH DNS setting changed: %#v", p)
				}
			} else if opts["config"] != ech {
				t.Fatal("ECH base64 was corrupted")
			}
			if p["servername"] != "some host" {
				t.Fatal("ECH compatibility changed unrelated query decoding")
			}
		}
	}
	if _, err := ShareLinkProxy("vless://user@example.com:443?ech=ip.gs%20udp://8.8.8.8", "node"); err == nil {
		t.Fatal("actual whitespace must not be repaired into a plus")
	}
}

func TestShareLinkRejectsWithCredentialFreeReason(t *testing.T) {
	for _, tc := range []struct{ link, code string }{
		{"vless://example.com:443", "missing-credentials"},
		{"trojan://example.com:443", "missing-credentials"},
		{"vless://private-secret@example.com:443?security=reality", "missing-reality-key"},
		{"vless://private-secret@example.com:443?type=unknown", "unsupported-transport"},
	} {
		_, err := ShareLinkProxy(tc.link, "节点")
		var detail *ShareLinkError
		if !errors.As(err, &detail) || !errors.Is(err, ErrShareLink) || detail.Code != tc.code || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("unsafe or missing reason: %v", err)
		}
	}
}

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
			name: "v4 token without password",
			link: "tuic://user@example.com:443",
			want: ProxyConfig{"token": "user"},
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
