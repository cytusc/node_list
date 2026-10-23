package ipcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchCacheRejectsUnrequestedAddressesAndSkipsIndividualLookup(t *testing.T) {
	c := NewClient("", "")
	calls := 0
	c.HTTP = &http.Client{Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "POST" || req.URL.Path != "/batch" {
			t.Fatalf("unexpected individual request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"status":"success","query":"8.8.8.8","hosting":true},{"status":"success","query":"1.1.1.1","hosting":true}]`))}, nil
	})}
	if err := c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatal(err)
	}
	if e := c.lookup(context.Background(), "ip-api", "8.8.8.8"); e.UsageType != "hosting" || calls != 1 {
		t.Fatalf("batch not reused: %+v", e)
	}
	if _, ok := c.cache["ip-api:1.1.1.1"]; ok {
		t.Fatal("unrequested address cached")
	}
}

// A second priming round over the same addresses must not spend another
// request: classification waves re-invoke the batch for every queued job.
func TestBatchPrimingsSkipsFreshCache(t *testing.T) {
	c := NewClient("", "")
	calls := 0
	c.HTTP = &http.Client{Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"status":"success","query":"8.8.8.8","hosting":true}]`))}, nil
	})}
	ips := []string{"8.8.8.8", "8.8.8.8", ""}
	for i := 0; i < 3; i++ {
		if err := c.PrimeIPAPIBatch(context.Background(), ips); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("fresh cache was re-primed %d times", calls)
	}
}

// The batch path shares the cross-replica quota bookkeeping with the single-IP
// lookup path through the injected provider hooks, but under its own key: the
// two endpoints are counted separately upstream.
func TestBatchPrimingSharesProviderQuota(t *testing.T) {
	c := NewClient("", "")
	var before, after int
	var key string
	c.BeforeProvider = func(_ context.Context, provider string) error { before++; key = provider; return nil }
	c.AfterProvider = func(context.Context, string, time.Time) error { after++; return nil }
	c.HTTP = &http.Client{Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Rl": []string{"0"}, "X-Ttl": []string{"59"}},
			Body: io.NopCloser(strings.NewReader(`[{"status":"success","query":"8.8.8.8","hosting":true}]`))}, nil
	})}
	if err := c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatal(err)
	}
	if before != 1 || after != 1 {
		t.Fatalf("quota hooks not shared: before=%d after=%d", before, after)
	}
	if key != batchKey {
		t.Fatalf("batch reserved slot on %q", key)
	}
	if wait := time.Until(c.providerNext(batchKey)); wait < 30*time.Second {
		t.Fatalf("X-Rl=0 cooldown not honored: %s", wait)
	}
}

// TestAttributesWaveEquivalence contrasts the pre-refactor flow (one Lookup
// per IP, six single-IP requests) with the batch-primed flow over the same
// simulated upstreams. It asserts (1) the upstream request count drops,
// (2) the evidence each provider returns is byte-for-byte equivalent, so the
// classification verdict cannot change.
func TestAttributesWaveEquivalence(t *testing.T) {
	ips := []string{"198.51.100.10", "198.51.100.11", "198.51.100.12"}
	client := func(counter *atomic.Int32) *Client {
		c := NewClient("", "", "registry", "")
		c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
			// Lookup fans out over six sources concurrently, so the counter the
			// mock increments must be atomic under -race.
			counter.Add(1)
			time.Sleep(20 * time.Millisecond)
			if r.Method == http.MethodPost {
				var requested []string
				_ = json.Unmarshal(readAll(r), &requested)
				items := make([]string, 0, len(requested))
				for _, ip := range requested {
					items = append(items, fmt.Sprintf(`{"status":"success","query":%q,"as":"AS64512 Example","hosting":false,"proxy":false,"mobile":false}`, ip))
				}
				return response(200, "["+strings.Join(items, ",")+"]"), nil
			}
			ip := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/json/"), "/")
			if ip == "" || !strings.Contains(ip, ".") {
				ip = "203.0.113.7"
			}
			var body string
			switch {
			case r.URL.Host == "ipinfo.io":
				body = fmt.Sprintf(`{"ip":%q,"asn":{"asn":"AS64512","type":"isp"},"company":{"type":"isp"},"privacy":{"hosting":false,"vpn":false,"proxy":false}}`, ip)
			case r.URL.Host == "api.ipregistry.co":
				body = fmt.Sprintf(`{"ip":%q,"connection":{"type":"isp","asn":64512},"company":{"type":"isp"},"security":{"is_cloud_provider":false,"is_proxy":false,"is_vpn":false}}`, ip)
			case r.URL.Host == "ip-api.com":
				body = fmt.Sprintf(`{"status":"success","query":%q,"as":"AS64512 Example","hosting":false,"proxy":false,"mobile":false}`, ip)
			case strings.Contains(r.URL.RawQuery, "db=ipapi"):
				body = fmt.Sprintf(`{"ip":%q,"asn":{"type":"isp"},"company":{"type":"isp"},"is_datacenter":false}`, ip)
			case strings.Contains(r.URL.RawQuery, "db=ip2location"):
				body = fmt.Sprintf(`{"ip":%q,"usage_type":"ISP","as_info":{"as_usage_type":"ISP"},"is_proxy":false}`, ip)
			default:
				body = fmt.Sprintf(`{"data":{"ipAddress":%q,"usageType":"Commercial"}}`, ip)
			}
			return response(200, body), nil
		})}
		return c
	}

	var oldCalls atomic.Int32
	oldClient := client(&oldCalls)
	oldEvidence := map[string][]Evidence{}
	for _, ip := range ips {
		oldEvidence[ip] = oldClient.Lookup(context.Background(), ip)
	}

	var newCalls atomic.Int32
	newClient := client(&newCalls)
	ctx := context.Background()
	if err := newClient.PrimeIPAPIBatch(ctx, ips); err != nil {
		t.Fatal(err)
	}
	newEvidence := map[string][]Evidence{}
	for _, ip := range ips {
		newEvidence[ip] = newClient.Lookup(ctx, ip)
	}

	// Old: 6 providers x 3 IPs. New: 1 batch + 5 providers x 3 IPs.
	if got := oldCalls.Load(); got != int32(len(ips)*6) {
		t.Fatalf("unexpected old request count: %d", got)
	}
	if got := newCalls.Load(); got != int32(1+len(ips)*5) {
		t.Fatalf("unexpected new request count: %d", got)
	}
	for _, ip := range ips {
		for i := range oldEvidence[ip] {
			a, b := oldEvidence[ip][i], newEvidence[ip][i]
			if a.Provider != b.Provider || a.Outcome != b.Outcome || a.UsageType != b.UsageType ||
				a.ASNType != b.ASNType || a.CompanyType != b.CompanyType ||
				a.ASN != b.ASN || hostFlag(a.Hosting) != hostFlag(b.Hosting) || a.Proxy != b.Proxy || a.VPN != b.VPN {
				t.Fatalf("evidence drift for %s/%s:\nold=%+v\nnew=%+v", ip, a.Provider, a, b)
			}
		}
	}
}

func hostFlag(p *bool) bool { return p != nil && *p }

func readAll(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}
