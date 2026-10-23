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

// batchInterval is the pacing between IP-API batch requests. The public quota
// (45 requests/minute) is shared with the single-IP endpoint, and one batch
// covers up to 100 addresses. A 1.6 s interval keeps ~37 requests/minute —
// under the ceiling while still 100x-ing the per-request throughput of the
// single-IP path. Override with IP_API_BATCH_INTERVAL_MS; 429 and X-Rl=0
// responses fall back to the provider cooldown instead of a fixed interval.
func batchInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv("IP_API_BATCH_INTERVAL_MS")); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 1600 * time.Millisecond
}

// PrimeIPAPIBatch fills the normal lookup cache using IP-API's batch endpoint:
// 100 IPs per request. Addresses that already hold fresh cached evidence are
// skipped, so repeated priming during one classification wave costs nothing.
// Lookup still queries the other independent sources for each requested IP.
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
	for offset := 0; offset < len(pending); offset += 100 {
		if offset > 0 {
			if err := sleepContext(ctx, batchInterval()); err != nil {
				return err
			}
		}
		if err := c.primeBatchChunk(ctx, pending[offset:min(offset+100, len(pending))]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) primeBatchChunk(ctx context.Context, chunk []string) error {
	// Share the cross-replica ip-api quota with the single-IP lookup path.
	for attempt := 0; attempt < 3; attempt++ {
		if c.BeforeProvider == nil {
			break
		}
		err := c.BeforeProvider(ctx, "ip-api")
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
		_ = c.AfterProvider(cleanup, "ip-api", c.providerNext("ip-api"))
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
	c.deferProvider("ip-api", time.Now().Add(batchInterval()))
	if err != nil {
		c.deferProvider("ip-api", time.Now().Add(30*time.Second))
		return fmt.Errorf("IP-API batch request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.deferProvider("ip-api", retryTime(resp.Header))
		return fmt.Errorf("IP-API batch HTTP 429")
	}
	if resp.Header.Get("X-Rl") == "0" {
		if ttl, err := strconv.Atoi(resp.Header.Get("X-Ttl")); err == nil && ttl > 0 {
			c.deferProvider("ip-api", time.Now().Add(time.Duration(ttl)*time.Second))
		} else {
			c.deferProvider("ip-api", time.Now().Add(time.Minute))
		}
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
