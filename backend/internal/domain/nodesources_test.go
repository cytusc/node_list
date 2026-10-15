package domain

import "testing"

func TestSourceMatchingIncludesTransportDetails(t *testing.T) {
	proxy := ProxyConfig{
		"name": "node", "type": "vless", "server": "example.com", "port": 443,
		"network": "ws", "tls": true, "skip-cert-verify": false,
		"ws-opts": map[string]any{"path": "/tunnel", "headers": map[string]any{"Host": "example.com"}},
	}
	node := Node{
		Name: "node", Type: "vless", Server: "example.com", Port: "443",
		Network: "ws", TLS: true,
		Details: []Detail{{K: "skip-cert-verify", V: "false"}, {K: "ws-path", V: "/tunnel"}, {K: "ws-host", V: "example.com"}},
	}
	key, _ := NodeIdentity(SummarizeProxy(proxy))
	sources := &Sources{Configs: map[string][]ProxyConfig{key: {proxy}}}
	if SourceForNode(node, sources, true) == nil {
		t.Fatal("matching transport details rejected")
	}
	node.Details[1].V = "/different"
	if SourceForNode(node, sources, true) != nil {
		t.Fatal("different transport path accepted")
	}
}
