package qilan

import (
	"testing"
)

func TestAdapterPreservesRawURIAndDoesNotTrustPanelFacts(t *testing.T) {
	link := "vless://opaque@example.com:443?unknown=keep#name"
	n := normalizeNode(qilanNode{ID: 1, Name: "label", Link: link, Protocol: "http", Status: "alive", IPType: "residential", Native: true})
	if n.RawLink != link || n.Name != "label" || len(n.RawConfig) != 0 || n.Type != "" || n.IPPure != nil || n.Connectivity != nil || n.IP != "" {
		t.Fatal("adapter interpreted or trusted upstream observations")
	}
	if normalizeNode(qilanNode{ID: 2}).RawLink != "" {
		t.Fatal("missing URI was fabricated")
	}
}
