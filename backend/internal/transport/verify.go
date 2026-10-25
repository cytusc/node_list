package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// verificationClient bounds one controller call. The probe's own timeout is
// shorter, so a hung controller fails the handshake instead of stalling the
// verifier's worker.
var verificationClient = &http.Client{Timeout: 30 * time.Second}

// DirectProxy is Mihomo's built-in direct outbound. Probing its delay is how
// the verifier proves its own egress works, without trusting a registry node
// whose reachable flag may be stale or falsely positive.
const DirectProxy = "DIRECT"

// LoadVerificationProxies replaces the instance's proxy set with one entry per
// usable node and returns the generated names plus the index of the node each
// name stands for. Verification never routes traffic through these proxies: the
// controller's delay probe performs the handshake, so no listener, rule set or
// geo database is involved.
func LoadVerificationProxies(ctx context.Context, instance MihomoConfig, nodes []domain.Node) ([]string, []int, error) {
	proxies := make([]domain.ProxyConfig, 0, len(nodes))
	picked := make([]int, 0, len(nodes))
	for i, node := range nodes {
		// outboundConfig always renders a map; a node without a real endpoint
		// would be rejected by the controller and take the whole wave with it.
		if p := outboundConfig(node); usableProxy(p) {
			proxies = append(proxies, p)
			picked = append(picked, i)
		}
	}
	config := mihomoConfig{
		MixedPort:          7893,
		AllowLan:           false,
		BindAddress:        "127.0.0.1",
		Mode:               "rule",
		ExternalController: strings.TrimPrefix(strings.TrimPrefix(instance.Controller, "https://"), "http://"),
		Secret:             instance.Secret,
		Rules:              []string{"MATCH,DIRECT"},
	}
	names := make([]string, 0, len(proxies))
	if len(proxies) > 0 {
		config.Proxies = applyECHDNS(&config, proxies)
		for i := range config.Proxies {
			name := fmt.Sprintf("verify-%d", i)
			config.Proxies[i]["name"] = name
			names = append(names, name)
		}
	}
	// One unsupported parameter rejects the whole wave with HTTP 400. The
	// controller names the offending index in some versions and not others, so
	// the index is read when present and bisected for otherwise: a single bad
	// node must never block the pool. A node that keeps failing leaves the
	// candidate query's oldest-first order and the wave moves on.
	for attempt := 0; attempt < 24; attempt++ {
		payload, err := marshalMihomoConfig(config)
		if err != nil {
			return nil, nil, fmt.Errorf("render verification config: %w", err)
		}
		status, body, err := putConfig(ctx, instance, payload)
		if err != nil {
			return nil, nil, err
		}
		if status == http.StatusNoContent || status == http.StatusOK {
			return names, picked, nil
		}
		if status != http.StatusBadRequest {
			return nil, nil, fmt.Errorf("load verification proxies: controller answered %d: %s", status, clip(body))
		}
		index, ok := rejectedProxyIndex(body)
		if !ok {
			if index, err = findRejectedProxy(ctx, instance, config); err != nil {
				return nil, nil, fmt.Errorf("load verification proxies: %w (controller said %s)", err, clip(body))
			}
		}
		if index < 0 || index >= len(config.Proxies) || len(config.Proxies) <= 1 {
			return nil, nil, fmt.Errorf("load verification proxies: controller answered %d: %s", status, clip(body))
		}
		config.Proxies = append(config.Proxies[:index], config.Proxies[index+1:]...)
		names = append(names[:index], names[index+1:]...)
		picked = append(picked[:index], picked[index+1:]...)
	}
	return nil, nil, errors.New("load verification proxies: every attempt rejected")
}

// findRejectedProxy locates one proxy that makes the controller reject the
// config, using bisection so the verifier never depends on the controller's
// error wording. The caller already knows the full set is rejected; every
// subset containing the offender is rejected too, so halving is sound.
func findRejectedProxy(ctx context.Context, instance MihomoConfig, config mihomoConfig) (int, error) {
	lo, hi := 0, len(config.Proxies)
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		rejected, err := rejectsSubset(ctx, instance, config, lo, mid)
		if err != nil {
			return 0, err
		}
		if rejected {
			hi = mid
		} else {
			lo = mid
		}
	}
	return lo, nil
}

func rejectsSubset(ctx context.Context, instance MihomoConfig, config mihomoConfig, from, to int) (bool, error) {
	subset := config
	subset.Proxies = config.Proxies[from:to]
	payload, err := marshalMihomoConfig(subset)
	if err != nil {
		return false, err
	}
	status, _, err := putConfig(ctx, instance, payload)
	if err != nil {
		return false, err
	}
	return status == http.StatusBadRequest, nil
}

// clip keeps an upstream error readable in a log line.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func putConfig(ctx context.Context, instance MihomoConfig, payload []byte) (int, string, error) {
	body, err := json.Marshal(map[string]string{"payload": string(payload)})
	if err != nil {
		return 0, "", err
	}
	endpoint := strings.TrimSuffix(instance.Controller, "/") + "/configs?force=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if instance.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+instance.Secret)
	}
	resp, err := verificationClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("load verification proxies: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(raw), nil
}

// rejectedProxyIndex extracts Mihomo's "proxy N:" index from a rejected config.
func rejectedProxyIndex(body string) (int, bool) {
	match := proxyErrorIndex.FindStringSubmatch(strings.TrimSpace(body))
	if len(match) != 2 {
		return 0, false
	}
	index, err := strconv.Atoi(match[1])
	return index, err == nil
}

// usableProxy reports whether a rendered proxy names a dialable endpoint.
func usableProxy(p domain.ProxyConfig) bool {
	server, _ := p["server"].(string)
	if server == "" {
		return false
	}
	switch port := p["port"].(type) {
	case int:
		return port > 0
	case string:
		return port != "" && port != "0"
	case float64:
		return port > 0
	}
	return false
}

// ProxyDelay runs the controller's url-test probe for one proxy: a real
// handshake through that node to DelayTarget. A non-200 answer is a verdict
// ("did not answer"), not a transport error.
func ProxyDelay(ctx context.Context, instance MihomoConfig, name string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	endpoint := strings.TrimSuffix(instance.Controller, "/") + "/proxies/" + url.PathEscape(name) +
		"/delay?timeout=" + strconv.Itoa(int(timeout/time.Millisecond)) +
		"&url=" + url.QueryEscape(DelayTarget)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if instance.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+instance.Secret)
	}
	resp, err := verificationClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil
	}
	var body struct {
		Delay int `json:"delay"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&body); err != nil {
		return false, nil
	}
	return body.Delay > 0, nil
}

// ProxyCount reports how many proxies the controller currently holds, so the
// verifier can confirm a wave actually landed before trusting its verdicts.
func ProxyCount(ctx context.Context, instance MihomoConfig) (int, error) {
	endpoint := strings.TrimSuffix(instance.Controller, "/") + "/proxies"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	if instance.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+instance.Secret)
	}
	resp, err := verificationClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("proxies: controller answered %d", resp.StatusCode)
	}
	var body struct {
		Proxies map[string]json.RawMessage `json:"proxies"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(&body); err != nil {
		return 0, err
	}
	return len(body.Proxies), nil
}
