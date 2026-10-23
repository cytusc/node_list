package ipcheck

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client shares rate limits and metadata caches across nodes with the same IP.
// IPPure evidence comes from the tunnel. IPregistry and AbuseIPDB use IPQuality's
// public lookup paths unless personal API keys are configured.
type Client struct {
	HTTP          *http.Client
	IPinfoToken   string
	IPAPIKey      string
	IPRegistryKey string
	AbuseIPDBKey  string
	// ProviderWait bounds how long one lookup waits for its provider slot
	// before recording a cooldown. Sources pace themselves by a few seconds, so
	// a two-second tolerance used to abandon nearly every multi-source lookup
	// and the missing type evidence then stayed missing for a whole day.
	ProviderWait time.Duration
	// Providers restricts the fan-out to the given sources, in the order they
	// are queried. Empty means every known source. Sources without credentials
	// can never answer, so a deployment is expected to narrow this list instead
	// of burning quota slots on requests that always cool down.
	Providers []string
	// FreshSweep waits for quotas instead of skipping IPs during maintenance.
	BeforeProvider    func(context.Context, string) error
	AfterProvider     func(context.Context, string, time.Time) error
	FreshSweep        bool
	mu                sync.Mutex
	providers         map[string]chan struct{}
	denied            map[string]time.Time
	cache             map[string]cachedEvidence
	next              map[string]time.Time
	registryDemoKey   string
	registryDemoUntil time.Time
}
type cachedEvidence struct {
	evidence Evidence
	until    time.Time
}

func NewClient(token, proKey string, extraKeys ...string) *Client {
	c := &Client{HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, IPinfoToken: token, IPAPIKey: proKey}
	if len(extraKeys) > 0 {
		c.IPRegistryKey = extraKeys[0]
	}
	if len(extraKeys) > 1 {
		c.AbuseIPDBKey = extraKeys[1]
	}
	return c
}

// defaultProviderWait keeps one lookup inside a few seconds per source while
// still waiting out ordinary quota spacing.
const defaultProviderWait = 10 * time.Second

func (c *Client) providerWait() time.Duration {
	if c.ProviderWait > 0 {
		return c.ProviderWait
	}
	return defaultProviderWait
}

// DefaultProviders is the full fan-out, in query order.
var DefaultProviders = []string{"ipinfo", "ip-api", "ipregistry", "abuseipdb", "ipapi", "ip2location"}

// ProvidersFromEnv reads IP_PROVIDERS (comma separated). Unknown names are
// dropped and the order always follows DefaultProviders so identities and
// vote weighting stay stable across deployments.
func ProvidersFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("IP_PROVIDERS"))
	if raw == "" {
		return nil
	}
	enabled := map[string]bool{}
	for _, name := range strings.Split(raw, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			enabled[name] = true
		}
	}
	out := make([]string, 0, len(enabled))
	for _, name := range DefaultProviders {
		if enabled[name] {
			out = append(out, name)
			delete(enabled, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *Client) providerList() []string {
	if len(c.Providers) > 0 {
		return c.Providers
	}
	return DefaultProviders
}

func (c *Client) Lookup(ctx context.Context, ip string) []Evidence {
	providers := c.providerList()
	out := make([]Evidence, len(providers))
	var wg sync.WaitGroup
	for i, provider := range providers {
		wg.Add(1)
		go func(i int, provider string) {
			defer wg.Done()
			out[i] = c.lookup(ctx, provider, ip)
		}(i, provider)
	}
	wg.Wait()
	return out
}

const browserUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// IPQuality's db_ipregistry uses this public demo fallback, then replaces it
// with apiKey from the provider's homepage when available.
const registryDemoFallback = "sb69ksjcajfs4c"

var registryDemoKeyPattern = regexp.MustCompile(`apiKey="([a-zA-Z0-9]+)"`)

// Called under the ipregistry gate, without holding the cache mutex.
// Cache homepage discovery across IPs, including failures.
func (c *Client) publicRegistryKey(ctx context.Context) string {
	if time.Now().Before(c.registryDemoUntil) {
		return c.registryDemoKey
	}
	c.registryDemoKey = registryDemoFallback
	c.registryDemoUntil = time.Now().Add(time.Minute)
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "https://ipregistry.co", nil)
	if err != nil {
		return c.registryDemoKey
	}
	req.Header.Set("User-Agent", browserUserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.registryDemoKey
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		c.registryDemoUntil = retryTime(resp.Header)
		return c.registryDemoKey
	}
	if resp.StatusCode != http.StatusOK {
		return c.registryDemoKey
	}
	html, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return c.registryDemoKey
	}
	if match := registryDemoKeyPattern.FindSubmatch(html); len(match) == 2 {
		c.registryDemoKey = string(match[1])
		c.registryDemoUntil = time.Now().Add(time.Hour)
	}
	return c.registryDemoKey
}

func (c *Client) lookup(ctx context.Context, provider, ip string) Evidence {
	key := provider + ":" + ip
	now := time.Now()
	if old, ok := c.cached(key); ok {
		return old
	}
	e := Evidence{Provider: provider, IP: ip, CheckedAt: now.UTC().Format(time.RFC3339Nano), Outcome: "unknown", QueryStatus: "request-failed"}
	quota := c.quotaKey(provider)
	var release func()
	for {
		var err error
		release, err = c.acquireProvider(ctx, quota)
		if err != nil {
			return e
		}
		// Another caller may have populated the cache while we waited.
		if old, ok := c.cached(key); ok {
			release()
			return old
		}
		wait := time.Until(c.providerNext(quota))
		if wait <= 0 {
			break // hold the slot for the request below
		}
		// Quota pacing must not hold the upstream slot: the shared clock
		// already spaces requests by the same interval, so sibling sources
		// proceed while this one waits. Only the request itself serialises
		// per upstream.
		release()
		if c.providerDenied(quota) || (!c.FreshSweep && wait > c.providerWait()) {
			e.QueryStatus = "quota-cooldown"
			return e
		}
		if err := sleepContext(ctx, wait); err != nil {
			return e
		}
	}
	defer release()
	if c.BeforeProvider != nil {
		if err := c.BeforeProvider(ctx, quota); err != nil {
			e.QueryStatus = "quota-cooldown"
			return e
		}
	}
	if c.AfterProvider != nil {
		defer func() {
			// A timed-out request must still publish its cooldown to replicas.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_ = c.AfterProvider(cleanup, quota, c.providerNext(quota))
		}()
	}
	e.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	url := "https://ipinfo.io/" + ip + "/json"
	if provider == "ipinfo" && c.IPinfoToken == "" {
		// IPQuality uses this public IPinfo endpoint, whose response includes
		// usage/company fields absent from the basic unauthenticated JSON API.
		url = "https://ipinfo.io/widget/demo/" + ip
	}
	if provider == "ipregistry" {
		registryKey := c.IPRegistryKey
		if registryKey == "" {
			registryKey = c.publicRegistryKey(ctx)
		}
		url = "https://api.ipregistry.co/" + ip + "?hostname=true&key=" + neturl.QueryEscape(registryKey)
	}
	if provider == "abuseipdb" {
		if c.AbuseIPDBKey == "" {
			url = "https://ipinfo.check.place/" + ip + "?db=abuseipdb"
		} else {
			url = "https://api.abuseipdb.com/api/v2/check?ipAddress=" + neturl.QueryEscape(ip) + "&maxAgeInDays=90"
		}
	}
	if provider == "ipapi" || provider == "ip2location" {
		url = "https://ipinfo.check.place/" + ip + "?db=" + provider
	}
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
	// Match IPQuality: lookup APIs use curl; IPregistry's demo uses a browser.
	req.Header.Set("User-Agent", "curl/8.10.1")
	if provider == "ipregistry" {
		req.Header.Set("User-Agent", browserUserAgent)
	}
	if provider == "ipregistry" && c.IPRegistryKey == "" {
		req.Header.Set("Origin", "https://ipregistry.co")
		req.Header.Set("Referer", "https://ipregistry.co/")
	}
	if provider == "abuseipdb" && c.AbuseIPDBKey != "" {
		req.Header.Set("Key", c.AbuseIPDBKey)
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	c.deferProvider(quota, time.Now().Add(1500*time.Millisecond))
	if err != nil {
		c.deferProvider(quota, time.Now().Add(30*time.Second))
		return e
	}
	defer resp.Body.Close()
	e.HTTPStatus = resp.StatusCode
	e.QueryStatus = "http-error"
	if resp.StatusCode == 429 {
		c.deferProvider(quota, retryTime(resp.Header))
		return e
	}
	if provider == "ip-api" && resp.Header.Get("X-Rl") == "0" {
		if ttl, err := strconv.Atoi(resp.Header.Get("X-Ttl")); err == nil && ttl > 0 {
			c.deferProvider(quota, time.Now().Add(time.Duration(ttl)*time.Second))
		} else {
			c.deferProvider(quota, time.Now().Add(time.Minute))
		}
	}
	if resp.StatusCode != 200 {
		// Public demo endpoints may reject an entire egress address. Avoid
		// repeating that rejection for every IP in a maintenance sweep.
		cooldown := time.Minute
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			cooldown = 15 * time.Minute
			c.denyProvider(quota, time.Now().Add(cooldown))
		}
		c.deferProvider(quota, time.Now().Add(cooldown))
		if provider == "ipregistry" && c.IPRegistryKey == "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			// Refresh rejected demo keys on the next lookup, with bounded pacing.
			c.registryDemoUntil = time.Time{}
		}
		return e
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		e.QueryStatus = "request-failed"
		c.deferProvider(quota, time.Now().Add(30*time.Second))
		return e
	}
	if provider == "ipinfo" {
		e = ParseIPinfo(raw, ip, e.CheckedAt)
	} else if provider == "ip-api" {
		e = ParseIPAPI(raw, ip, e.CheckedAt)
	} else if provider == "ipregistry" {
		e = ParseIPRegistry(raw, ip, e.CheckedAt)
	} else if provider == "ipapi" {
		e = ParseIPAPIIs(raw, ip, e.CheckedAt)
	} else if provider == "ip2location" {
		e = ParseIP2Location(raw, ip, e.CheckedAt)
	} else {
		e = ParseAbuseIPDB(raw, ip, e.CheckedAt)
	}
	e.QueryStatus = "success"
	e.HTTPStatus = resp.StatusCode
	ttl := 10 * time.Minute
	types := e.networkTypes()
	if e.Outcome == "unknown" && types.UsageType == "" && types.CompanyType == "" {
		ttl = time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]cachedEvidence{}
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

// BatchEvidence returns fresh, cached ip-api evidence for the requested IPs,
// as primed by PrimeIPAPIBatch. Missing or expired entries are omitted.
func (c *Client) BatchEvidence(ips []string) []Evidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Evidence, 0, len(ips))
	for _, ip := range ips {
		if e, ok := c.cache["ip-api:"+ip]; ok && time.Now().Before(e.until) {
			out = append(out, e.evidence)
		}
	}
	return out
}

// IPAPICached reports whether fresh batch evidence already covers the address,
// so callers can skip another priming round.
func (c *Client) IPAPICached(ip string) bool {
	_, ok := c.cached("ip-api:" + ip)
	return ok
}

// ParseIPRegistry interprets connection and security attributes. Business and
// education networks abstain from the residential/hosting vote.
func ParseIPRegistry(raw []byte, ip, checkedAt string) Evidence {
	e := Evidence{Provider: "ipregistry", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	var d struct {
		IP         string `json:"ip"`
		Connection struct {
			Type string          `json:"type"`
			ASN  json.RawMessage `json:"asn"`
		} `json:"connection"`
		Company struct {
			Type string `json:"type"`
		} `json:"company"`
		Security struct {
			Cloud *bool `json:"is_cloud_provider"`
			Proxy bool  `json:"is_proxy"`
			VPN   bool  `json:"is_vpn"`
		} `json:"security"`
	}
	if json.Unmarshal(raw, &d) != nil || d.IP != ip {
		return e
	}
	if json.Unmarshal(d.Connection.ASN, &e.ASN) != nil {
		var asn json.Number
		if json.Unmarshal(d.Connection.ASN, &asn) == nil {
			e.ASN = asn.String()
		}
	}
	e.ASNType, e.CompanyType = strings.ToLower(strings.TrimSpace(d.Connection.Type)), strings.ToLower(strings.TrimSpace(d.Company.Type))
	e.Proxy, e.VPN = d.Security.Proxy, d.Security.VPN
	e.Hosting = d.Security.Cloud
	e.classifyUsage()
	return e
}

// ParseAbuseIPDB uses its registered usage type as independent network evidence.
func ParseAbuseIPDB(raw []byte, ip, checkedAt string) Evidence {
	e := Evidence{Provider: "abuseipdb", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	var d struct {
		Data struct {
			IP        string `json:"ipAddress"`
			UsageType string `json:"usageType"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Data.IP != ip {
		return e
	}
	e.UsageType = strings.TrimSpace(d.Data.UsageType)
	e.classifyUsage()
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
		Mobile  bool   `json:"mobile"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Status != "success" || d.IP != ip {
		return e
	}
	e.ASN, e.Hosting, e.Proxy = d.ASN, d.Hosting, d.Proxy
	if d.Mobile {
		e.UsageType = "mobile"
	} else if d.Hosting != nil && *d.Hosting {
		e.UsageType = "hosting"
	}
	e.classifyUsage()
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
	e.CompanyType = strings.ToLower(strings.TrimSpace(d.Company.Type))
	e.ASNType = strings.ToLower(strings.TrimSpace(e.ASNType))
	e.Hosting, e.Proxy, e.VPN = d.Privacy.Hosting, d.Privacy.Proxy, d.Privacy.VPN
	if e.Hosting == nil {
		e.Hosting = d.Hosting
	}
	if d.Hosting != nil && *d.Hosting {
		e.Hosting = d.Hosting
	}
	e.classifyUsage()
	return e
}

// ParseIPAPIIs uses IPQuality's db_ipapi fields, independently of ip-api.com.
func ParseIPAPIIs(raw []byte, ip, checkedAt string) Evidence {
	e := Evidence{Provider: "ipapi", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	var d struct {
		IP  string `json:"ip"`
		ASN struct {
			Type string `json:"type"`
		} `json:"asn"`
		Company struct {
			Type string `json:"type"`
		} `json:"company"`
		Hosting *bool `json:"is_datacenter"`
		Proxy   bool  `json:"is_proxy"`
		VPN     bool  `json:"is_vpn"`
	}
	if json.Unmarshal(raw, &d) != nil || d.IP != ip {
		return e
	}
	e.UsageType, e.CompanyType = d.ASN.Type, d.Company.Type
	e.Hosting, e.Proxy, e.VPN = d.Hosting, d.Proxy, d.VPN
	e.classifyUsage()
	return e
}

// ParseIP2Location follows db_ip2location's usage_type/as_info.as_usage_type.
// networkType handles the first code of compound values such as ISP/MOB.
func ParseIP2Location(raw []byte, ip, checkedAt string) Evidence {
	e := Evidence{Provider: "ip2location", IP: ip, CheckedAt: checkedAt, Outcome: "unknown"}
	var d struct {
		IP        string `json:"ip"`
		UsageType string `json:"usage_type"`
		ASInfo    struct {
			UsageType string `json:"as_usage_type"`
		} `json:"as_info"`
		Proxy struct {
			Hosting *bool `json:"is_data_center"`
			VPN     bool  `json:"is_vpn"`
		} `json:"proxy"`
		IsProxy bool `json:"is_proxy"`
	}
	if json.Unmarshal(raw, &d) != nil || d.IP != ip {
		return e
	}
	e.UsageType, e.CompanyType = d.UsageType, d.ASInfo.UsageType
	e.Hosting, e.Proxy, e.VPN = d.Proxy.Hosting, d.IsProxy, d.Proxy.VPN
	e.classifyUsage()
	return e
}
