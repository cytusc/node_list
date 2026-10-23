package ipcheck

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaHeadersSpreadsRemainingBudget(t *testing.T) {
	h := http.Header{}
	h.Set("X-Rl", "14")
	h.Set("X-Ttl", "60")
	// 60s over 15 remaining slots.
	if got, ok := quotaHeaders(h, time.Second); !ok || got != 4*time.Second {
		t.Fatalf("share = %v ok=%v", got, ok)
	}
	// The floor wins when the upstream would allow a faster pace.
	if got, ok := quotaHeaders(h, 10*time.Second); !ok || got != 10*time.Second {
		t.Fatalf("floor ignored: %v ok=%v", got, ok)
	}
}

func TestQuotaHeadersWaitsOutExhaustedWindow(t *testing.T) {
	h := http.Header{}
	h.Set("X-Rl", "0")
	h.Set("X-Ttl", "26")
	if got, ok := quotaHeaders(h, time.Second); !ok || got != 26*time.Second {
		t.Fatalf("exhausted window = %v ok=%v", got, ok)
	}
}

func TestQuotaHeadersIgnoresProvidersWithoutAccounting(t *testing.T) {
	if _, ok := quotaHeaders(http.Header{}, time.Second); ok {
		t.Fatal("missing X-Rl must not invent a pace")
	}
	h := http.Header{}
	h.Set("X-Rl", "3")
	if _, ok := quotaHeaders(h, time.Second); ok {
		t.Fatal("missing X-Ttl must not invent a pace")
	}
}

// The batch endpoint is counted separately upstream, so its cooldown must not
// land on the single-IP key. Sharing the key is what turned a batch 429 into a
// single-IP cooldown and left the attribute queue without evidence.
func TestBatchPrimingUsesItsOwnQuotaKey(t *testing.T) {
	c := NewClient("", "")
	var keysMu sync.Mutex
	var keys []string
	c.BeforeProvider = func(_ context.Context, key string) error {
		keysMu.Lock()
		defer keysMu.Unlock()
		keys = append(keys, key)
		return nil
	}
	var bodies [][]string
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var chunk []string
		_ = json.Unmarshal(raw, &chunk)
		bodies = append(bodies, chunk)
		resp := response(200, `[{"status":"success","query":"8.8.8.8","hosting":true}]`)
		resp.Header.Set("X-Rl", "13")
		resp.Header.Set("X-Ttl", "60")
		return resp, nil
	})}
	if err := c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatalf("prime: %v", err)
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	if len(keys) != 1 || keys[0] != batchKey {
		t.Fatalf("batch used quota key %v", keys)
	}
	if c.quotaKey("ip-api") == batchKey {
		t.Fatal("single-IP and batch must not share one quota key")
	}
	if _, ok := c.cached("ip-api:8.8.8.8"); !ok {
		t.Fatal("batch evidence was not cached")
	}
	// X-Rl=13, X-Ttl=60 -> 60/14 ≈ 4.28 s, not the old 1.6 s.
	if wait := time.Until(c.providerNext(batchKey)); wait < 3*time.Second {
		t.Fatalf("batch paced too fast: %v", wait)
	}
}

// Every attributes consumer asks for its own wave. Without single-flight the
// duplicate waves are exactly what exhausts the batch quota.
func TestConcurrentBatchPrimingCollapsesToOneRequest(t *testing.T) {
	c := NewClient("", "")
	var calls atomic.Int32
	c.HTTP = &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return response(200, `[{"status":"success","query":"8.8.8.8","hosting":true}]`), nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}) }()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("duplicate batch waves: %d", got)
	}
}

// A cached address must never consume a batch slot.
func TestBatchPrimingSkipsCachedAddresses(t *testing.T) {
	c := NewClient("", "")
	c.cache = map[string]cachedEvidence{"ip-api:8.8.8.8": {Evidence{Provider: "ip-api"}, time.Now().Add(time.Minute)}}
	var calls atomic.Int32
	c.HTTP = &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(200, `[]`), nil
	})}
	if err := c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("cached address still primed: %d", calls.Load())
	}
}

func TestBatchRateLimitDoesNotPublishSingleIPCooldown(t *testing.T) {
	c := NewClient("", "")
	c.HTTP = &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		resp := response(429, "")
		resp.Header.Set("X-Rl", "0")
		resp.Header.Set("X-Ttl", "30")
		return resp, nil
	})}
	if err := c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}); err == nil {
		t.Fatal("429 must surface as an error")
	}
	if !strings.Contains(c.providerNext(batchKey).Sub(time.Now()).String(), "s") {
		t.Fatal("batch cooldown not recorded")
	}
	if wait := time.Until(c.providerNext("ip-api")); wait > time.Second {
		t.Fatalf("batch 429 leaked into the single-IP cooldown: %v", wait)
	}
}

// A widened window must not stall a task: the caller leaves the address to the
// single-IP path instead of waiting out an hour-long penalty.
func TestBatchUnavailableSkipsPrimingImmediately(t *testing.T) {
	c := NewClient("", "")
	c.HTTP = &http.Client{Transport: fixtureTransport(func(*http.Request) (*http.Response, error) {
		resp := response(429, "")
		resp.Header.Set("X-Rl", "0")
		resp.Header.Set("X-Ttl", "3600")
		return resp, nil
	})}
	_ = c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"})
	if c.BatchAvailable() {
		t.Fatal("an hour-long penalty must not look available")
	}
	start := time.Now()
	err := c.PrimeIPAPIBatch(context.Background(), []string{"1.1.1.1"})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("priming during a long penalty took %v (err=%v)", time.Since(start), err)
	}
	// The single-IP path stays usable, which is what makes skipping safe.
	if wait := time.Until(c.providerNext("ip-api")); wait > time.Second {
		t.Fatalf("single-IP path blocked by the batch penalty: %v", wait)
	}
}
