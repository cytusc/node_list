// Package collector probes node exits and queries IPPure, and schedules that
// work across the available engines.
package collector

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

// exitProbe describes one echo service used to discover the node's exit IP.
type exitProbe struct {
	URL      string
	Provider string
	Trace    bool
}

// ExitProbeServices raced per round. Cloudflare also reports the exit country.
var ExitProbeServices = []exitProbe{
	{"https://myip.ipip.net/", "IPIP", false},
	{"https://api-ipv4.ip.sb/ip", "IP.SB", false},
	{"https://checkip.amazonaws.com/", "AWS CheckIP", false},
	{"https://api.ipify.org", "ipify", false},
	{"https://ifconfig.me/ip", "ifconfig.me", false},
	{"https://ipv4.icanhazip.com/", "icanhazip", false},
	{"https://www.cloudflare.com/cdn-cgi/trace", "Cloudflare", true},
	{"https://api.ip.sb/ip", "IP.SB", false},
	{"https://icanhazip.com/", "icanhazip", false},
}

// ExitProbeBatch is how many endpoints race inside one timeout window.
const ExitProbeBatch = 4

// ExitOutcome is one successful exit discovery.
type ExitOutcome struct {
	IP            string
	Provider      string
	AddressFamily int
	Endpoint      string
	Country       string
}

// ExitAttempt records one dial for diagnostics.
type ExitAttempt struct {
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Round     int    `json:"round"`
	ElapsedMs int64  `json:"elapsedMs"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code,omitempty"`
}

func exitProbeCode(err error) string {
	var probeErr *exitError
	if errors.As(err, &probeErr) && probeErr.code != "" {
		return probeErr.code
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "TIMEOUT"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TIMEOUT"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "context deadline exceeded"),
		strings.Contains(message, "i/o timeout"),
		strings.Contains(message, "Client.Timeout"):
		return "TIMEOUT"
	case strings.Contains(message, "connection reset"):
		return "CONNECTION_RESET"
	case strings.Contains(message, "no such host"), strings.Contains(message, "lookup "):
		return "DNS_ERROR"
	case strings.Contains(message, "certificate"), strings.Contains(message, "TLS"),
		strings.Contains(message, "tls:"):
		return "TLS_ERROR"
	}
	return "NETWORK_ERROR"
}

type exitError struct {
	code string
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

var traceCountryPattern = regexp.MustCompile(`(?m)^loc=([A-Za-z]{2})\s*$`)
var ipipAddressPattern = regexp.MustCompile(`当前\s*IP[：:]\s*([^\s]+)`)

// ResolveExitIPInfo races the echo services through one node's transport.
//
// A remembered endpoint goes first: echo reachability is a property of the
// exit, so the previous winner usually wins again. Batches race inside one
// timeout window instead of dialling sequentially. Failure of one batch never
// skips later services: reachability can differ by destination and region.
func ResolveExitIPInfo(ctx context.Context, client *http.Client, timeout time.Duration, preferred []string, onAttempt func(ExitAttempt)) (*ExitOutcome, error) {
	if client == nil {
		return nil, &exitError{code: "NETWORK_ERROR", err: errors.New("IPPure 缺少隔离代理入口")}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	preferredSet := make(map[string]struct{}, len(preferred))
	for _, p := range preferred {
		preferredSet[p] = struct{}{}
	}
	services := make([]exitProbe, 0, len(ExitProbeServices))
	for _, s := range ExitProbeServices {
		if _, ok := preferredSet[s.URL]; ok {
			services = append(services, s)
		}
	}
	for _, s := range ExitProbeServices {
		if _, ok := preferredSet[s.URL]; !ok {
			services = append(services, s)
		}
	}

	var observed *ExitOutcome
	var ipv6 *ExitOutcome
	failures := 0
	var mu sync.Mutex

	for offset := 0; offset < len(services); offset += ExitProbeBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + ExitProbeBatch
		if end > len(services) {
			end = len(services)
		}
		batch := services[offset:end]
		results := make([]struct {
			outcome *ExitOutcome
			err     error
			elapsed int64
		}, len(batch))

		var wg sync.WaitGroup
		for i, service := range batch {
			wg.Add(1)
			go func(i int, service exitProbe) {
				defer wg.Done()
				started := time.Now()
				outcome, err := probeOneEndpoint(ctx, client, timeout, service)
				mu.Lock()
				results[i].outcome = outcome
				results[i].err = err
				results[i].elapsed = time.Since(started).Milliseconds()
				mu.Unlock()
			}(i, service)
		}
		wg.Wait()

		for i, service := range batch {
			result := results[i]
			if result.err != nil {
				code := exitProbeCode(result.err)
				if onAttempt != nil {
					onAttempt(ExitAttempt{
						Provider:  service.Provider,
						Endpoint:  service.URL,
						Round:     1,
						ElapsedMs: result.elapsed,
						Outcome:   "error",
						Code:      code,
					})
				}
				continue
			}
			if onAttempt != nil {
				outcome := "ipv6"
				if result.outcome.AddressFamily == 4 {
					outcome = "ipv4"
				}
				onAttempt(ExitAttempt{
					Provider:  service.Provider,
					Endpoint:  service.URL,
					Round:     1,
					ElapsedMs: result.elapsed,
					Outcome:   outcome,
				})
			}
			if result.outcome.AddressFamily == 4 {
				if observed == nil {
					observed = result.outcome
				}
			} else if ipv6 == nil {
				ipv6 = result.outcome
			}
		}
		if observed != nil {
			return observed, nil
		}
		failures += len(batch) - countSuccesses(results)
		// Timeouts only prove these destinations are unreachable. Regional
		// proxies may still reach another echo, so always try the next batch.
	}
	if ipv6 != nil {
		return ipv6, nil
	}
	failure := fmt.Errorf("出口 IP 探测失败：%d 个查询入口均未取得有效 IP", failures)
	return nil, &exitError{code: "EXIT_IP", err: failure}
}

func countSuccesses(results []struct {
	outcome *ExitOutcome
	err     error
	elapsed int64
}) int {
	n := 0
	for _, r := range results {
		if r.err == nil {
			n++
		}
	}
	return n
}

func probeOneEndpoint(ctx context.Context, client *http.Client, timeout time.Duration, service exitProbe) (*ExitOutcome, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, service.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", "https://ippure.com/")
	req.Header.Set("Origin", "https://ippure.com")
	req.Header.Set("User-Agent", "node-explorer-exit-probe")
	response, err := client.Do(req)
	if err != nil {
		// Transport messages can embed proxy credentials; only the category is kept.
		return nil, sanitizeError(err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
		response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &exitError{code: fmt.Sprintf("HTTP_%d", response.StatusCode),
			err: errors.New("出口查询 HTTP 错误")}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		return nil, &exitError{code: "NETWORK_ERROR", err: err}
	}
	body := strings.TrimSpace(string(raw))
	ip := body
	if service.Trace {
		ip = ""
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "ip=") {
				ip = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
				break
			}
		}
	}
	parsed := net.ParseIP(ip)
	if parsed == nil && service.Provider == "IPIP" {
		// IPIP returns a labelled Chinese sentence, not a bare address.
		if match := ipipAddressPattern.FindStringSubmatch(body); match != nil {
			parsed = net.ParseIP(match[1])
		}
	}
	if parsed == nil {
		return nil, &exitError{code: "INVALID_IP", err: errors.New("出口查询未返回有效 IP")}
	}
	ip = parsed.String()
	family := 4
	if parsed.To4() == nil {
		family = 6
	}
	outcome := &ExitOutcome{
		IP:            ip,
		Provider:      service.Provider,
		AddressFamily: family,
		Endpoint:      service.URL,
	}
	if service.Trace {
		if match := traceCountryPattern.FindStringSubmatch(body); match != nil {
			outcome.Country = strings.ToUpper(match[1])
		}
	} else if family == 4 {
		// One extra request over the already-open tunnel buys the exit country.
		// IPv6 outcomes skip it: IPPure rejects IPv6 anyway, and the lookup
		// would be wasted whenever a later endpoint yields IPv4.
		if country, err := ResolveExitCountry(ctx, client, timeout); err == nil {
			outcome.Country = country
		}
	}
	return outcome, nil
}

// ResolveExitCountry reads the exit country from a Cloudflare trace.
func ResolveExitCountry(ctx context.Context, client *http.Client, timeout time.Duration) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(req)
	if err != nil {
		return "", sanitizeError(err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8192))
		response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("出口国家查询 HTTP 错误 (%d)", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8192))
	if err != nil {
		return "", err
	}
	if match := traceCountryPattern.FindStringSubmatch(strings.TrimSpace(string(body))); match != nil {
		return strings.ToUpper(match[1]), nil
	}
	return "", errors.New("出口国家查询未返回有效地区码")
}

// sanitizeError strips proxy credentials that transport errors can carry.
func sanitizeError(err error) error {
	message := err.Error()
	if strings.Contains(message, "://") {
		if idx := strings.Index(message, "://"); idx >= 0 {
			message = message[:idx] + "://<redacted>"
		}
	}
	return &exitError{code: exitProbeCode(err), err: errors.New(message)}
}

// IPPureClient is a signed client for one Mihomo session.
type IPPureClient struct {
	origin  string
	client  *http.Client
	timeout time.Duration

	mu         sync.Mutex
	key        string
	timeOffset int64
}

type ippureHTTPError struct {
	status int
}

func (e *ippureHTTPError) Error() string {
	return fmt.Sprintf("IPPure 接口访问失败 (HTTP %d)", e.status)
}

// IPPureQueryError preserves a fixed failure stage while hiding response bodies.
type IPPureQueryError struct {
	Stage string
	Err   error
}

func (e *IPPureQueryError) Error() string { return e.Stage + ": " + IPPureErrorCode(e.Err) }
func (e *IPPureQueryError) Unwrap() error { return e.Err }
func IPPureFailure(err error) (string, string) {
	stage := "ippure-query"
	var e *IPPureQueryError
	if errors.As(err, &e) {
		stage = e.Stage
	}
	return stage, IPPureErrorCode(err)
}
func IPPureErrorCode(err error) string {
	var query *IPPureQueryError
	if errors.As(err, &query) {
		err = query.Err
	}
	if err == nil {
		return ""
	}
	var h *ippureHTTPError
	if errors.As(err, &h) {
		return fmt.Sprintf("http-%d", h.status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var probe *exitError
	if errors.As(err, &probe) {
		return probe.code
	}
	switch {
	case strings.Contains(err.Error(), "签名"):
		return "signature-failed"
	case strings.Contains(err.Error(), "JSON"):
		return "invalid-json"
	case strings.Contains(err.Error(), "IPv6"):
		return "unsupported-ipv6"
	case strings.Contains(err.Error(), "信息不完整"):
		return "incomplete-data"
	case strings.Contains(err.Error(), "不一致"):
		return "ip-mismatch"
	default:
		return "request-failed"
	}
}

// NewIPPureClient builds a signing client bound to one node transport.
func NewIPPureClient(client *http.Client, origin string, timeout time.Duration) *IPPureClient {
	if origin == "" {
		origin = "https://api.123169.xyz"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &IPPureClient{origin: origin, client: client, timeout: timeout}
}

// get performs one signed request, following the server's key challenge.
func (c *IPPureClient) get(ctx context.Context, path string) (map[string]any, error) {
	target := strings.TrimSuffix(c.origin, "/") + path
	var retries int
	for challenge := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Referer", "https://ippure.com/")
		req.Header.Set("Origin", "https://ippure.com")
		req.Header.Set("User-Agent", "Mozilla/5.0")
		c.mu.Lock()
		key, offset := c.key, c.timeOffset
		c.mu.Unlock()
		if key != "" {
			now := time.Now().UnixMilli() + offset
			mac := hmac.New(sha256.New, []byte(key))
			fmt.Fprintf(mac, "%s-%s--%d", "GET", target, now)
			req.Header.Set("x-k", key)
			req.Header.Set("x-t", fmt.Sprintf("%d-%s", now, hex.EncodeToString(mac.Sum(nil))))
		}
		response, err := c.fetch(ctx, req)
		if err != nil {
			return nil, err
		}
		issued := response.Header.Get("x-k")
		if issued != "" {
			serverTime, err := strconv.ParseInt(response.Header.Get("x-t"), 10, 64)
			drainAndClose(response)
			challenge++
			if challenge > 3 || err != nil {
				return nil, errors.New("IPPure 接口签名验证失败")
			}
			c.mu.Lock()
			c.key = issued
			c.timeOffset = serverTime - time.Now().UnixMilli()
			c.mu.Unlock()
			continue
		}
		status := response.StatusCode
		if (status == 429 || status >= 500) && retries < 2 {
			retryAfter := parseRetryAfter(response.Header.Get("retry-after"))
			drainAndClose(response)
			delay := time.Duration(1000*pow2(retries)) * time.Millisecond
			if retryAfter > delay {
				delay = retryAfter
			}
			if delay > 5*time.Second {
				delay = 5 * time.Second
			}
			retries++
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if status < 200 || status >= 300 {
			drainAndClose(response)
			return nil, &ippureHTTPError{status: status}
		}
		var payload struct {
			OK   bool           `json:"ok"`
			Data map[string]any `json:"data"`
		}
		decoded, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		drainAndClose(response)
		if readErr != nil {
			return nil, errors.New("IPPure 接口未返回有效 JSON")
		}
		if err := json.Unmarshal(decoded, &payload); err != nil {
			return nil, errors.New("IPPure 接口未返回有效 JSON")
		}
		if payload.OK && payload.Data == nil && strings.HasPrefix(path, "/api/info/asn/botclass/") {
			// The service returns {ok:true,data:null} for ASNs with no statistics.
			// Missing data and errors from required endpoints remain failures.
			var raw map[string]json.RawMessage
			if json.Unmarshal(decoded, &raw) == nil && strings.TrimSpace(string(raw["data"])) == "null" {
				return map[string]any{}, nil
			}
		}
		if !payload.OK || payload.Data == nil {
			return nil, errors.New("IPPure 接口返回失败")
		}
		return payload.Data, nil
	}
}

func (c *IPPureClient) fetch(ctx context.Context, req *http.Request) (*http.Response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	response, err := c.client.Do(req.WithContext(reqCtx))
	if err != nil {
		cancel()
		return nil, sanitizeError(err)
	}
	// Keep the request alive until its body is consumed. Cancelling after
	// headers discards the reusable connection and can invalidate the API's
	// signing challenge before the next request.
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func drainAndClose(r *http.Response) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 8192))
	r.Body.Close()
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func pow2(n int) int {
	result := 1
	for i := 0; i < n; i++ {
		result *= 2
	}
	return result
}

// IPPureResult is the normalised risk verdict.
type IPPureResult struct {
	IP                 string   `json:"ip"`
	ASN                string   `json:"asn"`
	Organization       string   `json:"asnOrganization"`
	Source             string   `json:"source"`
	Attributes         []string `json:"attributes"`
	HumanPercent       *float64 `json:"humanPercent"`
	BotPercent         *float64 `json:"botPercent"`
	TrafficRatioStatus string   `json:"trafficRatioStatus"`
	TrafficRatioError  string   `json:"trafficRatioError,omitempty"`
	Score              float64  `json:"score"`
	Label              string   `json:"label"`
	Country            string   `json:"country,omitempty"`
}

// scoreBands maps a risk score to its label.
var scoreBands = []struct {
	limit float64
	label string
}{
	{15, "极度纯净"}, {25, "纯净"}, {40, "中性"},
	{50, "轻度风险"}, {70, "中度风险"}, {100, "极度风险"},
}

// QueryIPPureType requests only current ASN type data for an exact exit IP.
// Type cleaning does not need risk scores or bot statistics.
func QueryIPPureType(ctx context.Context, c *IPPureClient, target string) (ipcheck.Evidence, error) {
	e := ipcheck.Evidence{Provider: "ippure", IP: target, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Outcome: "unknown", QueryStatus: "request-failed"}
	if net.ParseIP(target) == nil || net.ParseIP(target).To4() == nil {
		return e, errors.New("IPPure 类型查询需要 IPv4 出口")
	}
	basic, err := c.get(ctx, "/api/info/ip-basic/"+target)
	if err != nil {
		return e, err
	}
	ip, _ := basic["ip"].(string)
	if net.ParseIP(ip) == nil || normalizeIP(ip) != normalizeIP(target) {
		return e, errors.New("IPPure 返回 IP 与查询出口 IP 不一致")
	}
	asn, _ := basic["asn"].(map[string]any)
	kind, _ := asn["type"].(string)
	e = ipcheck.IPPureEvidence(target, []string{ipcheck.NetworkTypeLabel(kind)}, e.CheckedAt)
	e.QueryStatus = "success"
	return e, nil
}

// QueryIPPure resolves the risk verdict for one exit IP.
func QueryIPPure(ctx context.Context, c *IPPureClient, target string, asnCache *ASNCache) (*IPPureResult, error) {
	if net.ParseIP(target) == nil {
		return nil, errors.New("IPPure 查询目标必须是出口 IP")
	}
	if net.ParseIP(target).To4() == nil {
		return nil, errors.New("节点仅获取到 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6")
	}
	basic, err := c.get(ctx, "/api/info/ip-basic/"+target)
	if err != nil {
		return nil, &IPPureQueryError{Stage: "ippure-basic", Err: err}
	}
	risk, err := c.get(ctx, "/api/info/ip-risk/"+target)
	if err != nil {
		return nil, &IPPureQueryError{Stage: "ippure-risk", Err: err}
	}
	asnNumber, ok := numberField(basic, "asn", "number")
	if !ok || asnNumber < 1 {
		return nil, errors.New("IPPure ASN 信息不完整")
	}
	var bot map[string]any
	var botErr error
	loadBot := func(ctx context.Context) (map[string]any, error) {
		statistics, err := c.get(ctx, fmt.Sprintf("/api/info/asn/botclass/%d", int64(asnNumber)))
		var httpErr *ippureHTTPError
		if errors.As(err, &httpErr) && httpErr.status == http.StatusNotFound {
			// A missing ASN entry is a valid no-statistics result. Cache it so
			// every IP on that ASN does not repeat the same 404 lookup.
			return map[string]any{}, nil
		}
		return statistics, err
	}
	if asnCache != nil {
		bot, botErr = asnCache.Load(ctx, int64(asnNumber), loadBot)
	} else {
		bot, botErr = loadBot(ctx)
	}
	// ASN statistics are optional; their absence must not discard a valid risk score.
	if botErr != nil {
		bot = nil
	}
	result, err := NormalizeIPPure(target, basic, risk, bot)
	if err != nil {
		return nil, err
	}
	switch {
	case botErr != nil:
		var httpErr *ippureHTTPError
		if errors.As(botErr, &httpErr) && httpErr.status == http.StatusNotFound {
			result.TrafficRatioStatus = "no-data"
		} else {
			result.TrafficRatioStatus = "query-failed"
			result.TrafficRatioError = IPPureErrorCode(botErr)
		}
	case result.HumanPercent != nil && result.BotPercent != nil:
		result.TrafficRatioStatus = "available"
	default:
		result.TrafficRatioStatus = "no-data"
	}
	return result, nil
}

// NormalizeIPPure validates and shapes the API response.
func NormalizeIPPure(target string, basic, risk map[string]any, bot map[string]any) (*IPPureResult, error) {
	if basic == nil {
		return nil, errors.New("IPPure 返回信息不完整")
	}
	ip, _ := basic["ip"].(string)
	if net.ParseIP(ip) == nil || normalizeIP(ip) != normalizeIP(target) {
		return nil, errors.New("IPPure 返回 IP 与查询出口 IP 不一致")
	}
	asn, _ := basic["asn"].(map[string]any)
	asnNumber, ok := numberField(basic, "asn", "number")
	organization, _ := asn["organization"].(string)
	isBroadcast, broadcastOK := false, false
	if traits, ok := basic["traits"].(map[string]any); ok {
		if v, ok := traits["is_broadcast"].(bool); ok {
			isBroadcast, broadcastOK = v, true
		}
	}
	asnType, _ := asn["type"].(string)
	asnType = strings.ToLower(strings.TrimSpace(asnType))
	if !ok || asnNumber < 1 || strings.TrimSpace(organization) == "" || !broadcastOK || asnType == "" {
		return nil, errors.New("IPPure 返回信息不完整")
	}
	score, err := percentage(risk, "risk_score")
	if err != nil {
		return nil, err
	}
	label := "极度风险"
	for _, band := range scoreBands {
		if score <= band.limit {
			label = band.label
			break
		}
	}
	statistics := botStatistics(bot)
	typeLabel := ipcheck.NetworkTypeLabel(asnType)
	if typeLabel == "" {
		typeLabel = "类型未知"
	}
	attributes := []string{typeLabel}
	if asnType == "business" && statistics != nil && statistics.Human > 50 {
		attributes = append(attributes, "商业用途")
	}
	result := &IPPureResult{
		IP:           ip,
		ASN:          fmt.Sprintf("AS%d", int64(asnNumber)),
		Organization: strings.TrimSpace(organization),
		Source:       "原生IP",
		Attributes:   attributes,
		Score:        score,
		Label:        label,
	}
	if isBroadcast {
		result.Source = "广播IP"
	}
	if statistics != nil {
		human := round2(statistics.Human)
		botPercent := round2(statistics.Bot)
		result.HumanPercent = &human
		result.BotPercent = &botPercent
	}
	if country, ok := basic["country"].(string); ok && len(country) == 2 && isASCIILetters(country) {
		result.Country = strings.ToUpper(country)
	}
	return result, nil
}

func botStatistics(bot map[string]any) *struct{ Human, Bot float64 } {
	if bot == nil {
		return nil
	}
	human, _ := bot["human"].(float64)
	botValue, _ := bot["bot"].(float64)
	if human == 0 && botValue == 0 {
		return nil
	}
	human, err := clampPercent(human)
	if err != nil {
		return nil
	}
	botValue, err = clampPercent(botValue)
	if err != nil {
		return nil
	}
	return &struct{ Human, Bot float64 }{human, botValue}
}

func clampPercent(v float64) (float64, error) {
	if v < 0 || v > 100 || v != v {
		return 0, errors.New("IPPure 百分比无效")
	}
	return v, nil
}

func percentage(source map[string]any, key string) (float64, error) {
	if source == nil {
		return 0, errors.New("IPPure 百分比无效")
	}
	v, ok := source[key].(float64)
	if !ok || v != v || v < 0 || v > 100 {
		return 0, errors.New("IPPure 百分比无效")
	}
	return v, nil
}

func numberField(source map[string]any, path ...string) (float64, bool) {
	current := any(source)
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current = m[key]
	}
	v, ok := current.(float64)
	return v, ok
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func normalizeIP(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if parsed.To4() == nil {
		return "[" + ip + "]"
	}
	return parsed.To4().String()
}

func isASCIILetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// ASNCache deduplicates bot-class lookups across nodes that share an ASN.
type ASNCache struct {
	mu      sync.Mutex
	entries map[int64]*asnEntry
	ttl     time.Duration
}

type asnEntry struct {
	err      error
	failedAt time.Time
	value    map[string]any
	loadedAt time.Time
	loading  chan struct{}
	loaded   bool
}

// NewASNCache builds a cache. It is safe for concurrent use.
func NewASNCache() *ASNCache {
	return NewASNCacheWithTTL(0)
}

// NewASNCacheWithTTL builds a cache that refreshes successful values after ttl.
func NewASNCacheWithTTL(ttl time.Duration) *ASNCache {
	return &ASNCache{entries: make(map[int64]*asnEntry, 512), ttl: ttl}
}

// Load returns a cached value, coalescing concurrent lookups for one ASN.
// Transient failures share a short cooldown; successful values use the normal TTL.
func (c *ASNCache) Load(ctx context.Context, asn int64, compute func(context.Context) (map[string]any, error)) (map[string]any, error) {
	for {
		c.mu.Lock()
		entry := c.entries[asn]
		if entry == nil {
			entry = &asnEntry{}
			c.entries[asn] = entry
		}
		if entry.err != nil && time.Since(entry.failedAt) < 30*time.Second {
			err := entry.err
			c.mu.Unlock()
			return nil, err
		}
		if entry.loaded && (c.ttl <= 0 || time.Since(entry.loadedAt) < c.ttl) {
			value := entry.value
			c.mu.Unlock()
			return value, nil
		}
		if entry.loading != nil {
			loading := entry.loading
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-loading:
				continue
			}
		}
		loading := make(chan struct{})
		entry.loading = loading
		c.mu.Unlock()

		value, err := compute(ctx)
		c.mu.Lock()
		entry.err = nil
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			entry.err = err
			entry.failedAt = time.Now()
		}
		if err == nil {
			entry.value = value
			entry.loadedAt = time.Now()
			entry.loaded = true
		}
		entry.loading = nil
		close(loading)
		c.mu.Unlock()
		return value, err
	}
}
