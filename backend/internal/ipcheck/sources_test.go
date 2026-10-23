package ipcheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
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
	var mu sync.Mutex
	c := NewClient("private-token", "")
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
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

func TestNetworkTypeSourcesUseConfiguredCredentialsAndPublicDefaults(t *testing.T) {
	calls := map[string]int{}
	var mu sync.Mutex
	c := NewClient("", "", "registry-fixture", "abuse-fixture")
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[r.URL.Host]++
		body := `{}`
		switch r.URL.Host {
		case "ipinfo.io":
			if r.URL.Path != "/widget/demo/8.8.8.8" || r.Header.Get("Authorization") != "" {
				t.Fatal("public IPinfo should use the script's widget endpoint")
			}
		case "api.ipregistry.co":
			wantKey := "registry-fixture"
			if calls["ipregistry.co"] > 0 {
				wantKey = "publicfixture"
				if r.Header.Get("Origin") != "https://ipregistry.co" || r.Header.Get("Referer") != "https://ipregistry.co/" {
					t.Fatal("missing public IPregistry browser headers")
				}
			}
			if r.URL.Query().Get("key") != wantKey || r.URL.Query().Get("hostname") != "true" {
				t.Fatal("missing IPregistry key")
			}
			body = `{"ip":"8.8.8.8","connection":{"asn":15169,"type":"mobile"},"company":{"type":"isp"},"security":{"is_cloud_provider":false}}`
		case "api.abuseipdb.com":
			if r.Header.Get("Key") != "abuse-fixture" || r.URL.Query().Get("ipAddress") != "8.8.8.8" {
				t.Fatal("invalid AbuseIPDB request")
			}
			body = `{"data":{"ipAddress":"8.8.8.8","usageType":"Commercial"}}`
		case "ipregistry.co":
			body = `<script apiKey="publicfixture"></script>`
		case "ipinfo.check.place":
			if r.URL.Path != "/8.8.8.8" || (r.URL.Query().Get("db") != "abuseipdb" && r.URL.Query().Get("db") != "ipapi" && r.URL.Query().Get("db") != "ip2location") || r.Header.Get("Key") != "" {
				t.Fatal("public AbuseIPDB should use the script's endpoint without personal credentials")
			}
			if r.URL.Query().Get("db") == "abuseipdb" {
				body = `{"data":{"ipAddress":"8.8.8.8","usageType":"Commercial"}}`
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	evidence := c.Lookup(context.Background(), "8.8.8.8")
	if len(evidence) != 6 || evidence[2].ASN != "15169" {
		t.Fatalf("optional sources not decoded: %+v", evidence)
	}
	r := Score("8.8.8.8", evidence, time.Now())
	if strings.Join(r.Attributes(nil), ",") != "商业宽带" || r.ResidentialVotes != 0 {
		t.Fatalf("incorrect optional source types: %+v", r)
	}
	public := NewClient("", "")
	public.HTTP = c.HTTP
	publicEvidence := public.Lookup(context.Background(), "8.8.8.8")
	public.Lookup(context.Background(), "8.8.8.8")
	if calls["ipregistry.co"] != 1 || calls["api.ipregistry.co"] != 2 || calls["ipinfo.check.place"] != 5 || calls["api.abuseipdb.com"] != 1 {
		t.Fatalf("public lookups should use the script's endpoints and cache results: %v", calls)
	}
	if got := strings.Join(Score("8.8.8.8", publicEvidence, time.Now()).Attributes(nil), ","); got != "商业宽带" {
		t.Fatalf("public source types were not decoded: %s", got)
	}
}

// A deployment without credentials narrows the fan-out instead of burning
// quota slots on sources that can only cool down.
func TestProvidersFromEnvNarrowsAndKeepsOrder(t *testing.T) {
	t.Setenv("IP_PROVIDERS", " ip-api , ipinfo ,bogus ")
	got := ProvidersFromEnv()
	want := []string{"ipinfo", "ip-api"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	t.Setenv("IP_PROVIDERS", "")
	c := NewClient("", "")
	if len(c.providerList()) != len(DefaultProviders) {
		t.Fatalf("empty list must keep every source, got %v", c.providerList())
	}
	t.Setenv("IP_PROVIDERS", "bogus")
	if got := ProvidersFromEnv(); got != nil {
		t.Fatalf("unknown names must not disable everything, got %v", got)
	}
}
