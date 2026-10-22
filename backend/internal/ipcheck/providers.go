package ipcheck

import (
	"context"
	"time"
)

// The public mirrors share an upstream and therefore share pacing and denial
// cooldowns. An authenticated AbuseIPDB account has its own quota.
func (c *Client) quotaKey(provider string) string {
	if provider == "ipapi" || provider == "ip2location" || (provider == "abuseipdb" && c.AbuseIPDBKey == "") {
		return "check-place"
	}
	return provider
}

// Only requests to the same upstream serialize. Waiting for that upstream is
// cancellable, and neither network I/O nor pacing holds the cache mutex.
func (c *Client) acquireProvider(ctx context.Context, provider string) (func(), error) {
	c.mu.Lock()
	if c.providers == nil {
		c.providers = map[string]chan struct{}{}
	}
	gate := c.providers[provider]
	if gate == nil {
		gate = make(chan struct{}, 1)
		c.providers[provider] = gate
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-gate
			return nil, err
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) cached(key string) (Evidence, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.cache[key]
	return old.evidence, ok && time.Now().Before(old.until)
}

func (c *Client) providerNext(provider string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.next[provider]
}

func (c *Client) deferProvider(provider string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.next == nil {
		c.next = map[string]time.Time{}
	}
	if until.After(c.next[provider]) {
		c.next[provider] = until
	}
}

func (c *Client) denyProvider(provider string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.denied == nil {
		c.denied = map[string]time.Time{}
	}
	c.denied[provider] = until
}

func (c *Client) providerDenied(provider string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.denied[provider])
}
