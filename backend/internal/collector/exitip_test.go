package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type echoTransport func(*http.Request) (*http.Response, error)

func (f echoTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExitIPIPFallbackAfterTimeoutBatch(t *testing.T) {
	var mu sync.Mutex
	visited := map[string]bool{}
	client := &http.Client{Transport: echoTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		visited[r.URL.Host] = true
		mu.Unlock()
		if r.URL.Host != "myip.ipip.net" {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("当前 IP：101.251.204.174  来自于：中国 北京 北京  联通\n")), Header: make(http.Header)}, nil
	})}
	// Put four unreachable services first to reproduce premature-abort failures.
	preferred := []string{"https://api-ipv4.ip.sb/ip", "https://checkip.amazonaws.com/", "https://api.ipify.org", "https://ifconfig.me/ip"}
	o, err := ResolveExitIPInfo(context.Background(), client, time.Second, preferred, nil)
	if err != nil || o == nil || o.IP != "101.251.204.174" || o.Provider != "IPIP" {
		t.Fatalf("fallback = %+v, %v", o, err)
	}
	if !visited["myip.ipip.net"] {
		t.Fatal("regional fallback was skipped")
	}
}

func TestIPIPRejectsUnlabelledAndInvalidResponses(t *testing.T) {
	for _, body := range []string{"blocked 101.251.204.174", "当前 IP：999.251.204.174", "<html>request failed</html>"} {
		client := &http.Client{Transport: echoTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		if o, err := probeOneEndpoint(context.Background(), client, time.Second, exitProbe{"https://myip.ipip.net/", "IPIP", false}); err == nil || o != nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestASNCacheCoalescesConcurrentLoadsAndExpires(t *testing.T) {
	cache := NewASNCacheWithTTL(10 * time.Millisecond)
	var calls int
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			value, err := cache.Load(context.Background(), 3462, func(context.Context) (map[string]any, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				return map[string]any{"human": float64(74)}, nil
			})
			if err != nil || value["human"] != float64(74) {
				t.Errorf("load = %v, %v", value, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("concurrent loads = %d, want 1", calls)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := cache.Load(context.Background(), 3462, func(context.Context) (map[string]any, error) {
		calls++
		return map[string]any{"human": float64(75)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("loads after expiry = %d, want 2", calls)
	}
}

func TestQueryIPPureFetchesStatisticsWithoutCache(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cache      *ASNCache
		botPayload string
		botStatus  int
		wantStatus string
		wantCalls  int
	}{
		{"without cache", nil, `{"ok":true,"data":{"human":74.0479,"bot":25.9521}}`, 0, "available", 2},
		{"shared cache", NewASNCache(), `{"ok":true,"data":{"human":74.0479,"bot":25.9521}}`, 0, "available", 1},
		{"not found means no statistics", NewASNCache(), "", http.StatusNotFound, "no-data", 1},
		{"null statistics", NewASNCache(), `{"ok":true,"data":null}`, 0, "no-data", 1},
		{"missing data is a failure", nil, `{"ok":true}`, 0, "query-failed", 2},
		{"other http error", nil, "", http.StatusUnauthorized, "query-failed", 2},
		{"empty successful response", nil, `{"ok":true,"data":{}}`, 0, "no-data", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			botCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/info/ip-basic/118.168.211.221":
					fmt.Fprint(w, `{"ok":true,"data":{"ip":"118.168.211.221","asn":{"number":3462,"organization":"Data Communication Business Group","type":"isp","is_residential":true},"traits":{"is_broadcast":false}}}`)
				case "/api/info/ip-risk/118.168.211.221":
					fmt.Fprint(w, `{"ok":true,"data":{"risk_score":3}}`)
				case "/api/info/asn/botclass/3462":
					botCalls++
					if tc.botPayload == "" {
						http.Error(w, "unavailable", tc.botStatus)
						return
					}
					fmt.Fprint(w, tc.botPayload)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := NewIPPureClient(server.Client(), server.URL, time.Second)
			for range 2 {
				v, err := QueryIPPure(context.Background(), client, "118.168.211.221", tc.cache)
				if err != nil {
					t.Fatal(err)
				}
				if v.Score != 3 || v.Label != "极度纯净" || v.Source != "原生IP" || len(v.Attributes) != 1 || v.Attributes[0] != "住宅IP" {
					t.Fatalf("unexpected verdict: %+v", v)
				}
				if tc.wantStatus == "available" {
					if v.HumanPercent == nil || v.BotPercent == nil || *v.HumanPercent != 74.05 || *v.BotPercent != 25.95 {
						t.Fatalf("unexpected statistics: %+v", v)
					}
				} else if v.HumanPercent != nil || v.BotPercent != nil {
					t.Fatalf("expected absent statistics: %+v", v)
				}
				if v.TrafficRatioStatus != tc.wantStatus {
					t.Fatalf("traffic ratio status = %q, want %q", v.TrafficRatioStatus, tc.wantStatus)
				}
				if tc.botStatus == http.StatusUnauthorized && v.TrafficRatioError != "http-401" {
					t.Fatalf("failure reason lost: %+v", v)
				}
			}
			if botCalls != tc.wantCalls {
				t.Fatalf("statistics requests = %d, want %d", botCalls, tc.wantCalls)
			}
		})
	}
}

func TestASNCacheSharesFailureAndRetriesAfterCooldown(t *testing.T) {
	cache := NewASNCache()
	calls := 0
	load := func(context.Context) (map[string]any, error) { calls++; return nil, &ippureHTTPError{status: 429} }
	for range 10 {
		if _, err := cache.Load(context.Background(), 13335, load); err == nil {
			t.Fatal("missing failure")
		}
	}
	if calls != 1 {
		t.Fatalf("repeated failing ASN calls: %d", calls)
	}
	cache.mu.Lock()
	cache.entries[13335].failedAt = time.Now().Add(-time.Minute)
	cache.mu.Unlock()
	if _, err := cache.Load(context.Background(), 13335, load); err == nil || calls != 2 {
		t.Fatalf("failure never retried: %d %v", calls, err)
	}
}

func TestIPPureFailureKeepsSafeStageAndCode(t *testing.T) {
	stage, code := IPPureFailure(&IPPureQueryError{Stage: "ippure-basic", Err: &ippureHTTPError{status: 403}})
	if stage != "ippure-basic" || code != "http-403" {
		t.Fatalf("%s %s", stage, code)
	}
	_, code = IPPureFailure(&IPPureQueryError{Stage: "ippure-risk", Err: errors.New("IPPure 接口未返回有效 JSON")})
	if code != "invalid-json" {
		t.Fatal(code)
	}
	// The mirror answers 200 with {"ok":false} when its upstream is down. That
	// is not our network failing and must not be reported as a generic request
	// failure, which is what made the last outage hard to diagnose.
	_, code = IPPureFailure(&IPPureQueryError{Stage: "ippure-basic", Err: errors.New("IPPure 接口返回失败")})
	if code != "upstream-refused" {
		t.Fatal(code)
	}
}

// Only failures that another egress address could fix justify leasing another
// carrier. The risk lane retried three carriers plus a direct call for every
// failure, so a refused mirror cost each task up to forty seconds while the
// queue behind it waited.
func TestIPPureCarrierIndependence(t *testing.T) {
	wrap := func(err error) error { return &IPPureQueryError{Stage: "ippure-basic", Err: err} }
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"mirror lost its upstream", wrap(errors.New("IPPure 接口返回失败")), true},
		{"signature rejected", wrap(errors.New("IPPure 接口签名验证失败")), true},
		{"edge forbids us", wrap(&ippureHTTPError{status: 403}), true},
		{"timeout may be the carrier", wrap(context.DeadlineExceeded), false},
		{"rate limited may be the carrier", wrap(&ippureHTTPError{status: 429}), false},
		{"bad payload is not a carrier problem", wrap(errors.New("IPPure 接口未返回有效 JSON")), false},
	} {
		if got := IPPureCarrierIndependent(tc.err); got != tc.want {
			t.Fatalf("%s: carrier independent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNullDataIsOnlyValidForOptionalASNStatistics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"ok":true,"data":null}`) }))
	defer server.Close()
	c := NewIPPureClient(server.Client(), server.URL, time.Second)
	if _, err := c.get(context.Background(), "/api/info/ip-basic/1.1.1.1"); err == nil {
		t.Fatal("missing required IP data was accepted")
	}
	if _, err := c.get(context.Background(), "/api/info/asn/botclass/13335"); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessASNDoesNotBecomeDatacenter(t *testing.T) {
	for _, tc := range []struct{ kind, want string }{
		{"business", "商业宽带"}, {"hosting", "机房IP"}, {"isp", "住宅IP"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			basic := map[string]any{
				"ip":     "8.8.8.8",
				"asn":    map[string]any{"number": float64(15169), "organization": "Example", "type": tc.kind},
				"traits": map[string]any{"is_broadcast": false},
			}
			v, err := NormalizeIPPure("8.8.8.8", basic, map[string]any{"risk_score": float64(10)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Attributes) != 1 || v.Attributes[0] != tc.want {
				t.Fatalf("got %v", v.Attributes)
			}
		})
	}
}

func TestExitReturnsFirstIPv4AndCancelsSlowLosers(t *testing.T) {
	var active, countries atomic.Int32
	client := &http.Client{Transport: echoTransport(func(r *http.Request) (*http.Response, error) {
		active.Add(1)
		defer active.Add(-1)
		switch r.URL.Host {
		case "www.cloudflare.com":
			countries.Add(1)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ip=1.1.1.1\nloc=US\n")), Header: make(http.Header)}, nil
		case "api.ipify.org":
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("1.1.1.1")), Header: make(http.Header)}, nil
		default:
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	o, err := ResolveExitIPInfo(ctx, client, 5*time.Second, nil, nil)
	if err != nil || o == nil || o.IP != "1.1.1.1" || o.Country != "US" {
		t.Fatalf("fast winner waited for slow peers: %+v %v", o, err)
	}
	if active.Load() != 0 || countries.Load() != 1 {
		t.Fatalf("leaked requests or duplicate country: %d %d", active.Load(), countries.Load())
	}
}

func TestExitIPv6DoesNotCancelIPv4Fallback(t *testing.T) {
	client := &http.Client{Transport: echoTransport(func(r *http.Request) (*http.Response, error) {
		body := "2001:4860:4860::8888"
		if r.URL.Host == "ifconfig.me" {
			body = "8.8.8.8"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	o, err := ResolveExitIPInfo(context.Background(), client, time.Second, nil, nil)
	if err != nil || o == nil || o.IP != "8.8.8.8" {
		t.Fatalf("IPv4 fallback skipped: %+v %v", o, err)
	}
}

// A dial that ignores its request context must not hold the batch: before this
// bound, one hung endpoint blocked the whole probe until the task lease expired
// two minutes later, and every worker slot eventually stalled the same way.
func TestResolveExitIPDoesNotWaitForHungEndpoint(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	client := &http.Client{Transport: echoTransport(func(*http.Request) (*http.Response, error) {
		<-block
		return nil, errors.New("unblocked")
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := ResolveExitIPInfo(ctx, client, 200*time.Millisecond, nil, nil); err == nil {
		t.Fatal("hung endpoints must not report a successful probe")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("waited %s for a hung endpoint", elapsed)
	}
}

// A 429 with Retry-After must be repeated rather than surfaced: the upstream
// grants a retry, and dropping it costs the node its risk evidence for hours.
func TestIPPureRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"ok":true,"data":{"asn":{"number":15169}}}`)
	}))
	defer server.Close()
	c := NewIPPureClient(server.Client(), server.URL, 5*time.Second)
	c.retry.RetryWaitMin = 10 * time.Millisecond
	if _, err := c.get(context.Background(), "/api/info/ip-basic/1.1.1.1"); err != nil {
		t.Fatalf("retry after 429 failed: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream saw %d calls, want 2", got)
	}
}

// A missing signature (403) is an answer, not a transport problem. Retrying it
// would triple the cost of every rejected query.
func TestIPPureDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	c := NewIPPureClient(server.Client(), server.URL, 5*time.Second)
	if _, err := c.get(context.Background(), "/api/info/ip-basic/1.1.1.1"); err == nil {
		t.Fatal("403 must fail the query")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream saw %d calls, want 1", got)
	}
}

// A dead tunnel must stay a transport error. Retrying it hides which carrier
// failed and multiplies the time a broken node costs the lane.
func TestIPPureDoesNotRetryTransportErrors(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: echoTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("dial refused")
	})}
	c := NewIPPureClient(client, "http://example.invalid", 2*time.Second)
	if _, err := c.get(context.Background(), "/api/info/ip-basic/1.1.1.1"); err == nil {
		t.Fatal("transport failure must surface")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport attempted %d times, want 1", got)
	}
}
