package ipcheck

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client shares rate limits and metadata caches across nodes with the same IP.
// It only queries IPinfo and IP-API; IPPure evidence comes from the tunnel.
type Client struct {
	HTTP        *http.Client
	IPinfoToken string
	IPAPIKey    string
	mu          sync.Mutex
	cache       map[string]cachedEvidence
	next        map[string]time.Time
}
type cachedEvidence struct {
	evidence Evidence
	until    time.Time
}

func NewClient(token, proKey string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, IPinfoToken: token, IPAPIKey: proKey}
}

func (c *Client) Lookup(ctx context.Context, ip string) []Evidence {
	return []Evidence{c.lookup(ctx, "ipinfo", ip), c.lookup(ctx, "ip-api", ip)}
}

func (c *Client) lookup(ctx context.Context, provider, ip string) Evidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]cachedEvidence{}
		c.next = map[string]time.Time{}
	}
	key := provider + ":" + ip
	now := time.Now()
	if old, ok := c.cache[key]; ok && now.Before(old.until) {
		return old.evidence
	}
	e := Evidence{Provider: provider, IP: ip, CheckedAt: now.UTC().Format(time.RFC3339Nano), Outcome: "unknown"}
	if wait := time.Until(c.next[provider]); wait > 0 {
		// Quota blocks should not stall a recovery wave. Short pacing is bounded.
		if wait > 2*time.Second {
			return e
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return e
		case <-timer.C:
		}
	}
	url := "https://ipinfo.io/" + ip + "/json"
	if provider == "ip-api" {
		url = "http://ip-api.com/json/" + ip + "?fields=status,message,query,as,hosting,proxy,mobile"
		if c.IPAPIKey != "" {
			url = "https://pro.ip-api.com/json/" + ip + "?fields=status,message,query,as,hosting,proxy,mobile&key=" + neturl.QueryEscape(c.IPAPIKey)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return e
	}
	if provider == "ipinfo" && c.IPinfoToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.IPinfoToken)
	}
	resp, err := c.HTTP.Do(req)
	c.next[provider] = time.Now().Add(1500 * time.Millisecond)
	if err != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		c.next[provider] = retryTime(resp.Header)
		return e
	}
	if provider == "ip-api" && resp.Header.Get("X-Rl") == "0" {
		if ttl, err := strconv.Atoi(resp.Header.Get("X-Ttl")); err == nil && ttl > 0 {
			c.next[provider] = time.Now().Add(time.Duration(ttl) * time.Second)
		} else {
			c.next[provider] = time.Now().Add(time.Minute)
		}
	}
	if resp.StatusCode != 200 {
		return e
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return e
	}
	if provider == "ipinfo" {
		e = ParseIPinfo(raw, ip, e.CheckedAt)
	} else {
		e = ParseIPAPI(raw, ip, e.CheckedAt)
	}
	ttl := 10 * time.Minute
	if e.Outcome == "unknown" {
		ttl = time.Minute
	}
	if len(c.cache) >= 1024 {
		for k, v := range c.cache {
			if !time.Now().Before(v.until) {
				delete(c.cache, k)
			}
		}
	}
	c.cache[key] = cachedEvidence{e, time.Now().Add(ttl)}
	return e
}

func retryTime(h http.Header) time.Time {
	for _, field := range []string{"Retry-After", "X-Ttl"} {
		if seconds, err := strconv.Atoi(h.Get(field)); err == nil && seconds > 0 && seconds <= 86400 {
			return time.Now().Add(time.Duration(seconds) * time.Second)
		}
	}
	if stamp, err := http.ParseTime(h.Get("Retry-After")); err == nil && stamp.After(time.Now()) {
		return stamp
	}
	return time.Now().Add(time.Minute)
}

func ParseIPAPI(raw []byte, ip, checkedAt string) Evidence {
	e := Evidence{Provider: "ip-api", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	var d struct {
		Status  string `json:"status"`
		IP      string `json:"query"`
		ASN     string `json:"as"`
		Hosting *bool  `json:"hosting"`
		Proxy   bool   `json:"proxy"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Status != "success" || d.IP != ip || d.Hosting == nil {
		return e
	}
	e.ASN, e.Hosting, e.Proxy = d.ASN, d.Hosting, d.Proxy
	if *d.Hosting {
		e.Outcome = "provider-conflict"
	} else {
		e.Outcome = "non-hosting"
	}
	return e
}

func ParseIPinfo(raw []byte, ip, checkedAt string) Evidence {
	e := Evidence{Provider: "ipinfo", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return e
	}
	if data, ok := root["data"]; ok {
		raw = data
	}
	var d struct {
		IP      string          `json:"ip"`
		ASN     json.RawMessage `json:"asn"`
		ASType  string          `json:"as_type"`
		Company struct {
			Type string `json:"type"`
		} `json:"company"`
		Privacy struct {
			Hosting *bool `json:"hosting"`
			VPN     bool  `json:"vpn"`
			Proxy   bool  `json:"proxy"`
		} `json:"privacy"`
		Hosting *bool           `json:"is_hosting"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &d) != nil || d.IP != ip || (len(d.Error) > 0 && string(d.Error) != "null") {
		return e
	}
	var asn struct {
		ASN  string `json:"asn"`
		Type string `json:"type"`
	}
	if json.Unmarshal(d.ASN, &asn) == nil {
		e.ASN, e.ASNType = asn.ASN, asn.Type
	} else {
		_ = json.Unmarshal(d.ASN, &e.ASN)
		e.ASNType = d.ASType
	}
	if e.ASNType == "" {
		e.ASNType = d.ASType
	}
	e.CompanyType = strings.ToLower(d.Company.Type)
	e.ASNType = strings.ToLower(e.ASNType)
	e.Hosting, e.Proxy, e.VPN = d.Privacy.Hosting, d.Privacy.Proxy, d.Privacy.VPN
	if e.Hosting == nil {
		e.Hosting = d.Hosting
	}
	switch {
	case (d.Hosting != nil && *d.Hosting) || (e.Hosting != nil && *e.Hosting) || e.CompanyType == "hosting" || e.ASNType == "hosting":
		e.Outcome = "provider-conflict"
	case (e.Hosting != nil && !*e.Hosting):
		e.Outcome = "non-hosting"
	case e.CompanyType == "isp" || e.ASNType == "isp":
		e.Outcome = "supports-residential"
	}
	return e
}
