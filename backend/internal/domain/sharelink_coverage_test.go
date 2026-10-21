package domain

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Protocol coverage fixtures (REFACTOR_PLAN §6.1): every supported share-link
// protocol must produce a stable connection identity, and parse failures must
// fall back to the summary identity while staying observable.
func TestShareLinkProtocolCoverage(t *testing.T) {
	links := []string{
		"vless://11111111-2222-3333-4444-555555555555@192.0.2.10:443?encryption=none&security=tls&sni=example.com&type=ws&host=example.com&path=%2Fws#fixture",
		"hysteria2://letmein@192.0.2.11:8443?sni=example.com&insecure=0#h2",
		"tuic://11111111-2222-3333-4444-555555555555:letmein@192.0.2.12:443?congestion_control=bbr&alpn=h3#tuic",
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@192.0.2.13:8388#ss",
		"trojan://letmein@192.0.2.14:443?sni=example.com#trojan",
	}
	for _, link := range links {
		first, ok := ShareLinkConnection(link)
		if !ok {
			t.Fatalf("supported protocol rejected: %s", link)
		}
		second, ok := ShareLinkConnection(link)
		if !ok || first != second {
			t.Fatalf("unstable connection rendering: %s", link)
		}
		n := Node{Type: strings.SplitN(link, ":", 2)[0], Server: "192.0.2.1", Port: "443", RawLink: link}
		id := AggregateIdentity(n)
		if !strings.HasPrefix(id, "share-link:") {
			t.Fatalf("share link identity missing prefix: %s -> %s", link, id)
		}
	}
}

func TestShareLinkParseFailureCounts(t *testing.T) {
	before := ShareLinkParseFailures.Load()
	// Valid base64 wrapping invalid JSON must fall back to the summary
	// identity and increment the observable failure counter.
	raw := base64.RawStdEncoding.EncodeToString([]byte("not-json!"))
	n := Node{Type: "vmess", Server: "192.0.2.9", Port: "443", RawLink: "vmess://" + raw}
	id := AggregateIdentity(n)
	if strings.HasPrefix(id, "share-link:") {
		t.Fatal("malformed link still produced a connection identity")
	}
	if got := ShareLinkParseFailures.Load(); got != before+1 {
		t.Fatalf("parse failure not counted: before=%d after=%d", before, got)
	}
}
