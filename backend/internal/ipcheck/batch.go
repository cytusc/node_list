package ipcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// batchKey is the rate-limit identity of IP-API's batch endpoint. The free
// batch path and the single-IP path are counted separately upstream, so
// publishing both to one key made a batch burst look like single-IP pressure
// and vice versa. Keeping them apart lets each endpoint run at its own
// permitted rate while still sharing the cooldown across replicas.
const batchKey = "ip-api-batch"

// batchFloor is the slowest pace the batch endpoint may be asked for. The free
// tier permits far fewer batch requests per minute than single-IP lookups, so
// the previous 1.6 s pacing — chosen for the single-IP path — was over the
// ceiling and produced a 429 on most waves. IP_API_BATCH_INTERVAL_MS raises
// the floor; X-Rl/X-Ttl normally decide the pace on their own.
func batchFloor() time.Duration {
	if v := strings.TrimSpace(os.Getenv("IP_API_BATCH_INTERVAL_MS")); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 4 * time.Second
}

// batchCooldownBudget is how long a priming wave is willing to wait for the
// batch endpoint before leaving the address to the single-IP path. IP-API
// answers a sustained overrun by widening the window to an hour, and waiting
// that out inside a task would stall the lane for no benefit.
const batchCooldownBudget = 20 * time.Second

// BatchAvailable reports whether priming is worth attempting right now. The
// caller falls back to the single-IP lookup, which shares nothing with this
// cooldown since the two endpoints are counted separately upstream.
func (c *Client) BatchAvailable() bool {
	return time.Until(c.providerNext(batchKey)) <= batchCooldownBudget
}

// PrimeIPAPIBatch fills the normal lookup cache using IP-API's batch endpoint:
// 100 IPs per request. Addresses that already hold fresh cached evidence are
// skipped, so repeated priming during one classification wave costs nothing.
// Lookup still queries the other independent sources for each requested IP.
//
// Priming is shared between concurrent consumers: every attributes worker
// would otherwise ask for its own wave of 100 addresses, and those duplicate
// waves are what exhausted the batch quota in the first place.
func (c *Client) PrimeIPAPIBatch(ctx context.Context, ips []string) error {
	pending := make([]string, 0, len(ips))
	seen := map[string]bool{}
	for _, ip := range ips {
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		if _, ok := c.cached("ip-api:" + ip); !ok {
			pending = append(pending, ip)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	_, err, _ := c.prime.Do("ip-api-batch", func() (any, error) {
		// Re-check under the single-flight lock: the wave that won the race may
		// already have covered most of this one.
		missing := make([]string, 0, len(pending))
		for _, ip := range pending {
			if _, ok := c.cached("ip-api:" + ip); !ok {
				missing = append(missing, ip)
			}
		}
		return nil, c.primeChunks(ctx, missing)
	})
	return err
}

func (c *Client) primeChunks(ctx context.Context, pending []string) error {
	for offset := 0; offset < len(pending); offset += 100 {
		if err := c.primeBatchChunk(ctx, pending[offset:min(offset+100, len(pending))]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) primeBatchChunk(ctx context.Context, chunk []string) error {
	// Wait out the pace the previous wave earned before asking for a new slot.
	if wait := time.Until(c.providerNext(batchKey)); wait > 0 {
		if wait > batchCooldownBudget {
			return fmt.Errorf("IP-API batch cooling down")
		}
		if err := sleepContext(ctx, wait); err != nil {
			return err
		}
	}
	// Share the cross-replica batch quota with other priming waves.
	for attempt := 0; attempt < 3; attempt++ {
		if c.BeforeProvider == nil {
			break
		}
		err := c.BeforeProvider(ctx, batchKey)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := sleepContext(ctx, 3*time.Second); err != nil {
			return err
		}
		if attempt == 2 {
			return fmt.Errorf("IP-API batch quota unavailable")
		}
	}
	defer func() {
		// Timed-out chunks must still publish their cooldown to replicas.
		if c.AfterProvider == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = c.AfterProvider(cleanup, batchKey, c.providerNext(batchKey))
	}()

	body, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	endpoint := "http://ip-api.com/batch?fields=status,message,query,as,hosting,proxy,mobile"
	if c.IPAPIKey != "" {
		endpoint = "https://pro.ip-api.com/batch?fields=status,message,query,as,hosting,proxy,mobile&key=" + url.QueryEscape(c.IPAPIKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	// Publish the pace this wave earned before anything else can read it.
	pace := batchFloor()
	if err == nil {
		if d, ok := quotaHeaders(resp.Header, pace); ok {
			pace = d
		}
	}
	c.deferProvider(batchKey, time.Now().Add(pace))
	if err != nil {
		c.deferProvider(batchKey, time.Now().Add(30*time.Second))
		return fmt.Errorf("IP-API batch request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("IP-API batch HTTP 429")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("IP-API batch HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("invalid IP-API batch response")
	}
	requested := map[string]bool{}
	for _, ip := range chunk {
		requested[ip] = true
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cachedEvidence{}
	}
	for _, item := range items {
		var v struct {
			IP     string `json:"query"`
			Status string `json:"status"`
		}
		if json.Unmarshal(item, &v) != nil || !requested[v.IP] || v.Status != "success" {
			continue
		}
		e := ParseIPAPI(item, v.IP, stamp)
		e.QueryStatus, e.HTTPStatus = "success", 200
		c.cache["ip-api:"+v.IP] = cachedEvidence{e, time.Now().Add(10 * time.Minute)}
	}
	c.mu.Unlock()
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
