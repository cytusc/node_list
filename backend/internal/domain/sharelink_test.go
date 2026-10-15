package domain

import (
	"encoding/base64"
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
