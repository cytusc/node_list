package qilan

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func detailValues(n *domain.Node) map[string]string {
	out := map[string]string{}
	for _, d := range n.Details {
		out[d.K] = d.V
	}
	return out
}

func TestNormalizeNodeKeepsProtocolFactsFromShareLink(t *testing.T) {
	link := "vless://7c973569-71da-4d18-8c80-aa4c8ee2f257@216.195.196.86:8443" +
		"?security=reality&type=tcp&sni=www.cloudflare.com&fp=chrome&flow=xtls-rprx-vision" +
		"&sid=4fba6fd2c74fd18f&pbk=5wGgDloyck_L25Y5rkWorpV3IK00pK0Lki7LR0VY0io&encryption=none#tag"
	n := normalizeNode(qilanNode{
		ID: 2189336, Name: "SG 1 0", Protocol: "vless", Server: "216.195.196.86",
		Port: float64(8443), Link: link, Ping: float64(172), Status: "online",
		IPType: "hosting", ISP: "Panstar Cloud Limited", ASN: float64(197196),
		// The panel's own columns disagree with the link: it reports tcp and no TLS.
		Network: "tcp", Region: "SG",
	})
	if n == nil {
		t.Fatal("node dropped")
	}
	if !n.Reality {
		t.Error("reality flag lost: the link carries security=reality")
	}
	if !n.TLS {
		t.Error("TLS flag lost")
	}
	if n.SNI != "www.cloudflare.com" {
		t.Errorf("sni = %q", n.SNI)
	}
	if n.Network != "tcp" {
		t.Errorf("network = %q", n.Network)
	}
	if n.Country != "SG" || n.SubTag != "Qilan" || n.Name != "SG 1 0 [Qilan]" {
		t.Errorf("panel metadata lost: %+v", n)
	}
	got := detailValues(n)
	for k, want := range map[string]string{
		"source-id": "2189336", "latency": "172 ms", "status": "online",
		"ip-type": "hosting", "isp": "Panstar Cloud Limited", "asn": "197196",
		"flow": "xtls-rprx-vision", "client-fingerprint": "chrome",
		"reality-public-key": "5wGgDloyck_L25Y5rkWorpV3IK00pK0Lki7LR0VY0io",
		"reality-short-id":   "4fba6fd2c74fd18f",
	} {
		if got[k] != want {
			t.Errorf("detail %q = %q, want %q", k, got[k], want)
		}
	}
}

func TestNormalizeNodeNetworkFollowsProtocolNotPanel(t *testing.T) {
	cases := []struct {
		name, protocol, panel, link, want string
	}{
		{"hysteria2 panel says tcp", "hysteria2", "tcp",
			"hysteria2://secret@203.0.113.9:443?sni=example.com#tag", "quic"},
		{"ssr panel holds the protocol", "ssr", "auth_chain_a",
			ssrLink(t), "tcp"},
		{"raw is tcp", "vless", "raw",
			"vless://7c973569-71da-4d18-8c80-aa4c8ee2f257@203.0.113.9:443?security=tls&type=raw&sni=a.example#tag", "tcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := normalizeNode(qilanNode{
				ID: 1, Name: "n", Protocol: tc.protocol, Server: "203.0.113.9",
				Port: float64(443), Link: tc.link, Network: tc.panel,
			})
			if n == nil {
				t.Fatal("node dropped")
			}
			if n.Network != tc.want {
				t.Fatalf("network = %q, want %q", n.Network, tc.want)
			}
		})
	}
}

func TestNormalizeNodeFallsBackForUnparseableLink(t *testing.T) {
	link := "hysteria2://pw@203.0.113.9:443?sni=a.example\u0007#tag"
	n := normalizeNode(qilanNode{
		ID: 7, Name: "bad", Protocol: "hysteria2", Server: "203.0.113.9",
		Port: float64(443), Link: link, Network: "tcp",
	})
	if n == nil {
		t.Fatal("row must survive an unparseable link")
	}
	if n.Network != "quic" {
		t.Errorf("fallback network = %q, want quic", n.Network)
	}
	if n.Name != "bad [Qilan]" || n.RawLink != link {
		t.Errorf("fallback lost provenance: %+v", n)
	}
}

// Enriching the summary must not move the stored identity: Qilan rows carry a
// share link and no raw config, so identity is the link hash.
func TestNormalizeNodeKeepsShareLinkIdentity(t *testing.T) {
	link := "vless://7c973569-71da-4d18-8c80-aa4c8ee2f257@216.195.196.86:8443" +
		"?security=reality&type=tcp&sni=www.cloudflare.com&fp=chrome#tag"
	enriched := normalizeNode(qilanNode{
		ID: 1, Name: "n", Protocol: "vless", Server: "216.195.196.86",
		Port: float64(8443), Link: link, Network: "tcp",
	})
	legacy := domain.Node{
		Type: "vless", Server: "216.195.196.86", Port: "8443", Network: "tcp",
		RawLink: link, Details: []domain.Detail{{K: "source-id", V: "1"}},
	}
	if domain.AggregateIdentity(*enriched) != domain.AggregateIdentity(legacy) {
		t.Fatalf("identity moved: %q vs %q",
			domain.AggregateIdentity(*enriched), domain.AggregateIdentity(legacy))
	}
}

// ssrLink builds an SSR link with an obfsparam so the parser has real work.
func ssrLink(t *testing.T) string {
	t.Helper()
	body := "203.0.113.9:443:auth_chain_a:aes-256-cfb:tcp-plain:" +
		base64.RawURLEncoding.EncodeToString([]byte("secret")) +
		"/?obfsparam=" + base64.RawURLEncoding.EncodeToString([]byte("cdn.example"))
	return "ssr://" + base64.RawURLEncoding.EncodeToString([]byte(body))
}

func TestNormalizeNetworkAlwaysReturnsKnownTransport(t *testing.T) {
	for _, in := range []string{"", "raw", "tcp#💡95@tag", "weird", "WS", " raw "} {
		got := normalizeNetwork(in, "vless")
		if !strings.Contains(" tcp ws grpc http h2 httpupgrade xhttp quic kcp ", " "+got+" ") {
			t.Fatalf("normalizeNetwork(%q) = %q", in, got)
		}
	}
}

// The live panel leaves network empty for every hysteria2 and ss row and fills
// it with SSR's protocol for ssr, so protocol facts must win.
func TestNormalizeNodeEmptyPanelNetworkFallsBackToProtocol(t *testing.T) {
	n := normalizeNode(qilanNode{
		ID: 5194812, Name: "PL 32 D+", Protocol: "hysteria2", Server: "78.17.128.153",
		Port: float64(35076), Link: "hysteria2://pw@78.17.128.153:35076?sni=a.example#t",
		Network: "", Region: "PL",
	})
	if n == nil {
		t.Fatal("node dropped")
	}
	if n.Network != "quic" {
		t.Fatalf("network = %q, want quic", n.Network)
	}
}

// Panel columns beyond the seven the adapter used to keep must reach Details.
func TestNormalizeNodeKeepsPanelMetadata(t *testing.T) {
	n := normalizeNode(qilanNode{
		ID: 1, Name: "n", Protocol: "vless", Server: "203.0.113.9", Port: float64(443),
		Link: "vless://7c973569-71da-4d18-8c80-aa4c8ee2f257@203.0.113.9:443?security=tls&sni=a.example#t",
		Ping: float64(268), Status: "online", IPType: "hosting",
		ISP: "NETRANEX", Org: "NETRANEX HOLDINGS", City: "Tel Aviv",
		ASN: "AS153914", IPScore: float64(42), Native: true, Black: true,
		TrafficRate: float64(2), FailCount: float64(3),
		LastCheck: "2026-10-23 07:03:19", Region: "CY",
	})
	got := detailValues(n)
	for k, want := range map[string]string{
		"isp": "NETRANEX", "org": "NETRANEX HOLDINGS", "city": "Tel Aviv",
		"ip-score": "42", "native": "true", "blacklisted": "true",
		"traffic-rate": "2x", "fail-count": "3", "last-check": "2026-10-23 07:03:19",
	} {
		if got[k] != want {
			t.Errorf("detail %q = %q, want %q", k, got[k], want)
		}
	}
	if got["traffic-rate"] == "" || got["last-check"] == "" {
		t.Error("panel metadata missing")
	}
}

// org duplicates isp for most rows; only a distinct owner is worth a row.
func TestNormalizeNodeSkipsRedundantOrgAndNeutralFlags(t *testing.T) {
	n := normalizeNode(qilanNode{
		ID: 2, Name: "n", Protocol: "ss", Server: "203.0.113.10", Port: float64(8388),
		Link: "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw")) + "@203.0.113.10:8388#t",
		ISP:  "Same ISP", Org: "Same ISP", TrafficRate: float64(1),
	})
	got := detailValues(n)
	if _, ok := got["org"]; ok {
		t.Error("redundant org row kept")
	}
	if _, ok := got["traffic-rate"]; ok {
		t.Error("1x traffic rate is noise")
	}
	if _, ok := got["native"]; ok {
		t.Error("false native flag kept")
	}
}
