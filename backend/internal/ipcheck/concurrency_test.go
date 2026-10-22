package ipcheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("operation blocked")
		var zero T
		return zero
	}
}

func TestLookupSlowProviderDoesNotBlockOtherSourcesOrCache(t *testing.T) {
	c := NewClient("token", "", "registry", "abuse")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	healthy := make(chan struct{}, 1)
	// Prime the public mirrors so this test has no pacing delays.
	c.cache = map[string]cachedEvidence{}
	for _, p := range []string{"ipapi", "ip2location"} {
		c.cache[p+":8.8.8.8"] = cachedEvidence{Evidence{Provider: p}, time.Now().Add(time.Minute)}
	}
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "ipinfo.io" {
			started <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		if r.URL.Host == "ip-api.com" {
			healthy <- struct{}{}
			return response(200, `{"status":"success","query":"8.8.8.8","hosting":true}`), nil
		}
		return response(200, `{}`), nil
	})}
	done := make(chan []Evidence, 1)
	go func() { done <- c.Lookup(ctx, "8.8.8.8") }()
	receive(t, started)
	receive(t, healthy)
	// Wait for the healthy request's cache write, using its own provider gate.
	cache := make(chan Evidence, 1)
	go func() { cache <- c.lookup(ctx, "ip-api", "8.8.8.8") }()
	if e := receive(t, cache); e.UsageType != "hosting" {
		t.Fatalf("healthy evidence unavailable while another provider stalled: %+v", e)
	}
	cancel()
	if got := receive(t, done); len(got) != 6 || got[0].Provider != "ipinfo" || got[1].Provider != "ip-api" {
		t.Fatalf("unstable source order: %+v", got)
	}
}

func TestProviderWaitCanBeCanceledAndConcurrentCacheMissesDeduplicate(t *testing.T) {
	c := NewClient("token", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var calls atomic.Int32
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-unblock:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return response(200, `{"ip":"8.8.8.8","asn":{"type":"isp"}}`), nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.lookup(ctx, "ipinfo", "8.8.8.8") }()
	}
	receive(t, started)
	waitCtx, stop := context.WithCancel(ctx)
	stop()
	wait := make(chan Evidence, 1)
	go func() { wait <- c.lookup(waitCtx, "ipinfo", "1.1.1.1") }()
	if e := receive(t, wait); e.QueryStatus != "request-failed" {
		t.Fatalf("canceled lookup misreported: %+v", e)
	}
	close(unblock)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	receive(t, done)
	if calls.Load() != 1 {
		t.Fatalf("duplicate upstream requests: %d", calls.Load())
	}
}

func TestPublicMirrorDenialSharesCooldownButAuthenticatedSourceIsIndependent(t *testing.T) {
	c := NewClient("", "")
	c.FreshSweep = true
	var calls atomic.Int32
	var persistedKey string
	var persistedUntil time.Time
	c.AfterProvider = func(_ context.Context, key string, until time.Time) error {
		persistedKey, persistedUntil = key, until
		return nil
	}
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(403, `denied`), nil
	})}
	first := c.lookup(context.Background(), "ipapi", "8.8.8.8")
	if first.HTTPStatus != 403 || persistedKey != "check-place" || time.Until(persistedUntil) < 14*time.Minute {
		t.Fatalf("denial was not shared: %+v, %s %v", first, persistedKey, persistedUntil)
	}
	for _, p := range []string{"ip2location", "abuseipdb"} {
		if e := c.lookup(context.Background(), p, "1.1.1.1"); e.QueryStatus != "quota-cooldown" || e.Outcome != "unknown" {
			t.Fatalf("denial should skip request and abstain: %+v", e)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("retried denied shared mirror %d times", calls.Load())
	}
	// A separately configured account must not inherit public mirror denial.
	private := NewClient("", "", "", "private")
	if private.quotaKey("abuseipdb") == private.quotaKey("ipapi") {
		t.Fatal("private account shares public quota")
	}
}

func TestCanceledRequestStillPublishesCooldown(t *testing.T) {
	c := NewClient("", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		cancel()
		return nil, r.Context().Err()
	})}
	published := false
	c.AfterProvider = func(cleanup context.Context, _ string, until time.Time) error {
		published = cleanup.Err() == nil && time.Until(until) >= 29*time.Second
		return nil
	}
	c.lookup(ctx, "ip-api", "8.8.8.8")
	if !published {
		t.Fatal("canceled request lost its cooldown")
	}
}

// Compare orchestration with identical simulated 20 ms upstreams. The public
// mirror denies requests, as in production; no external service is contacted.
func BenchmarkLookupSources(b *testing.B) {
	for _, parallel := range []bool{false, true} {
		name := "sequential"
		if parallel {
			name = "parallel"
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c := NewClient("token", "", "registry")
				c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
					time.Sleep(20 * time.Millisecond)
					if r.URL.Host == "ipinfo.check.place" {
						return response(403, `denied`), nil
					}
					return response(200, `{}`), nil
				})}
				if parallel {
					c.Lookup(context.Background(), "8.8.8.8")
				} else {
					for _, p := range []string{"ipinfo", "ip-api", "ipregistry", "abuseipdb", "ipapi", "ip2location"} {
						c.lookup(context.Background(), p, "8.8.8.8")
					}
				}
			}
		})
	}
}
