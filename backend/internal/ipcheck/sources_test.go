package ipcheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParsersAbstainOnFailuresAndKeepProxySeparate(t *testing.T) {
	for _, tc := range []struct{ name, provider, raw, want string }{
		{"IP-API nonhosting proxy", "ip-api", `{"status":"success","query":"8.8.8.8","hosting":false,"proxy":true}`, "non-hosting"},
		{"IP-API hosting", "ip-api", `{"status":"success","query":"8.8.8.8","hosting":true}`, "provider-conflict"},
		{"IP-API missing flag", "ip-api", `{"status":"success","query":"8.8.8.8"}`, "unknown"},
		{"IP-API failed", "ip-api", `{"status":"fail","message":"quota"}`, "unknown"},
		{"IPinfo public basic", "ipinfo", `{"ip":"8.8.8.8","org":"AS15169 Example"}`, "unknown"},
		{"IPinfo ISP", "ipinfo", `{"ip":"8.8.8.8","asn":{"type":"isp"}}`, "supports-residential"},
		{"IPinfo VPN nonhosting", "ipinfo", `{"ip":"8.8.8.8","privacy":{"hosting":false,"vpn":true}}`, "non-hosting"},
		{"IPinfo hosting", "ipinfo", `{"data":{"ip":"8.8.8.8","is_hosting":true}}`, "provider-conflict"},
		{"wrong IP", "ipinfo", `{"ip":"1.1.1.1","is_hosting":false}`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ParseIPinfo([]byte(tc.raw), "8.8.8.8", "now")
			if tc.provider == "ip-api" {
				e = ParseIPAPI([]byte(tc.raw), "8.8.8.8", "now")
			}
			if e.Outcome != tc.want {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

func TestClientCachesPerIPAndHonorsIPAPIQuota(t *testing.T) {
	calls := map[string]int{}
	c := NewClient("private-token", "")
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Host]++
		header := make(http.Header)
		body := `{"ip":"8.8.8.8","is_hosting":false}`
		if r.URL.Host == "ipinfo.io" {
			if r.Header.Get("Authorization") != "Bearer private-token" || strings.Contains(r.URL.String(), "private-token") {
				t.Fatal("token must only be in header")
			}
		} else {
			header.Set("X-Rl", "0")
			header.Set("X-Ttl", "60")
			body = `{"status":"success","query":"8.8.8.8","hosting":false}`
		}
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	first := c.Lookup(context.Background(), "8.8.8.8")
	_ = c.Lookup(context.Background(), "8.8.8.8")
	if calls["ipinfo.io"] != 1 || calls["ip-api.com"] != 1 || first[1].Outcome != "non-hosting" {
		t.Fatalf("lookup not deduplicated: %v %+v", calls, first)
	}
	blocked := c.lookup(context.Background(), "ip-api", "1.1.1.1")
	if blocked.Outcome != "unknown" || calls["ip-api.com"] != 1 {
		t.Fatal("quota ignored")
	}
}

func TestRateLimitResponsesDoNotBecomeDatacenterVotes(t *testing.T) {
	c := NewClient("", "")
	c.HTTP = &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader("limited"))}, nil
	})}
	e := c.lookup(context.Background(), "ip-api", "8.8.8.8")
	if e.Outcome != "unknown" || time.Until(c.next["ip-api"]) < 119*time.Second {
		t.Fatalf("bad rate limit handling %+v", e)
	}
}
