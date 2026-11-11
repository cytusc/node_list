package proxyprocess

import "testing"

func TestDecodePreservesHTTPTLSOptionsAndSafeErrors(t *testing.T) {
	for _, raw := range []string{
		`{"rawConfig":{"type":"http","server":"120.232.204.58","port":31343,"tls":true,"skip-cert-verify":true}}`,
		`{"rawLink":"https://120.232.204.58:31343?allowInsecure=1&security=tls"}`,
	} {
		p, err := Decode([]byte(raw))
		if err != nil || p["type"] != "http" || p["tls"] != true || p["skip-cert-verify"] != true || p["port"] != 31343 {
			t.Fatalf("HTTP TLS settings changed: %v", err)
		}
	}
	for _, tc := range []struct{ raw, reason string }{
		{`{"rawConfig":{"server":"example.com","port":0}}`, "invalid_endpoint"},
		{`{"rawLink":"vless://secret@example.com:443?security=reality"}`, "share_link_missing-reality-key"},
		{`{"port":{}}`, "invalid_node"},
	} {
		_, err := Decode([]byte(tc.raw))
		if err == nil || err.Error() != tc.reason {
			t.Fatalf("want safe diagnostic %q, got %v", tc.reason, err)
		}
	}
}
