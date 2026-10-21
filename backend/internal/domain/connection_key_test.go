package domain

import (
	"encoding/base64"
	"testing"
)

func TestConnectionKeyMergesEffectiveVMessVariants(t *testing.T) {
	link := func(extra string) Node {
		return Node{RawLink: "vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"add":"example.com","port":"443","id":"11111111-1111-4111-8111-111111111111","net":"tcp"`+extra+`}`))}
	}
	a := link(`,"ps":"first","host":"ignored.example","path":"/ignored","insecure":"1","test_name":"98"`)
	b := link(`,"ps":"second","sni":"also-ignored.example","tls":"none"`)
	key := ConnectionKey(a)
	if key == "" || key != ConnectionKey(b) {
		t.Fatal("equivalent VMess links should share a connection key")
	}
	c, err := ShareLinkProxy(a.RawLink, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if key != ConnectionKey(Node{RawConfig: c, Details: []Detail{{K: "status", V: "changed"}}}) {
		t.Fatal("Clash config and share link must converge independently of summary fields")
	}
	c["uuid"] = "22222222-2222-4222-8222-222222222222"
	if key == ConnectionKey(Node{RawConfig: c}) {
		t.Fatal("distinct credentials collapsed")
	}
}

func TestConnectionKeyPreservesTransportAndTLS(t *testing.T) {
	base := ProxyConfig{"type": "vless", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111", "tls": true, "servername": "one.example", "network": "ws", "ws-opts": map[string]any{"path": "/one"}}
	key := ConnectionKey(Node{RawConfig: base})
	base["servername"] = "two.example"
	if key == ConnectionKey(Node{RawConfig: base}) {
		t.Fatal("different TLS destinations collapsed")
	}
	base["servername"] = "one.example"
	base["ws-opts"] = map[string]any{"path": "/two"}
	if key == ConnectionKey(Node{RawConfig: base}) {
		t.Fatal("different websocket paths collapsed")
	}
	if ConnectionKey(Node{Server: "example.com", Port: "443"}) != "" {
		t.Fatal("missing credentials must not be guessed")
	}
}
