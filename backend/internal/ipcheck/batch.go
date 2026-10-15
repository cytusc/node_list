package ipcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PrimeIPAPIBatch fills the normal lookup cache using IP-API's documented batch
// endpoint: 100 IPs/request, at most 15 requests/minute on the public endpoint.
// Lookup still queries the other independent sources for each requested IP.
func (c *Client) PrimeIPAPIBatch(ctx context.Context, ips []string) error {
	for offset := 0; offset < len(ips); offset += 100 {
		if offset > 0 {
			timer := time.NewTimer(4100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		end := offset + 100
		if end > len(ips) {
			end = len(ips)
		}
		body, err := json.Marshal(ips[offset:end])
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
		if err != nil {
			return fmt.Errorf("IP-API batch request failed")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("IP-API batch HTTP %d", resp.StatusCode)
		}
		if readErr != nil {
			return readErr
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("invalid IP-API batch response")
		}
		requested := map[string]bool{}
		for _, ip := range ips[offset:end] {
			requested[ip] = true
		}
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		c.mu.Lock()
		if c.cache == nil {
			c.cache = map[string]cachedEvidence{}
			c.next = map[string]time.Time{}
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
	}
	return nil
}
