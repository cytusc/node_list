package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// ProxyConfigError identifies the one outbound Mihomo rejected. Controller
// error text may contain credentials, so only its index is retained.
type ProxyConfigError struct{ Index int }

func (e *ProxyConfigError) Error() string {
	return "代理配置参数不受当前 Mihomo 支持 (HTTP 400)"
}

var proxyErrorIndex = regexp.MustCompile(`^proxy ([0-9]+):`)

// mihomoConfig is the document sent to PUT /configs.
type mihomoConfig struct {
	DNS                *mihomoDNS       `yaml:"dns,omitempty"`
	Authentication     []string         `yaml:"authentication,omitempty"`
	MixedPort          int              `yaml:"mixed-port"`
	AllowLan           bool             `yaml:"allow-lan"`
	BindAddress        string           `yaml:"bind-address"`
	Mode               string           `yaml:"mode"`
	ExternalController string           `yaml:"external-controller"`
	Secret             string           `yaml:"secret"`
	Proxies            []map[string]any `yaml:"proxies"`
	Listeners          []mihomoListener `yaml:"listeners"`
	Rules              []string         `yaml:"rules"`
}

type mihomoDNS struct {
	Enable                      bool                `yaml:"enable"`
	NameServer                  []string            `yaml:"nameserver"`
	ProxyServerNameserver       []string            `yaml:"proxy-server-nameserver"`
	ProxyServerNameserverPolicy map[string][]string `yaml:"proxy-server-nameserver-policy,omitempty"`
}

type mihomoListener struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"`
	Port   int    `yaml:"port"`
	Listen string `yaml:"listen"`
	Proxy  string `yaml:"proxy"`
}

// applyECHDNS extracts the resolver metadata retained by the Qilan share-link
// parser and routes each ECH query name through the resolver supplied with
// that link. Mihomo only supports ECH resolver configuration at the DNS level.
func applyECHDNS(config *mihomoConfig, proxies []domain.ProxyConfig) []map[string]any {
	var echDNS mihomoDNS
	echDNS.Enable = true
	echDNS.NameServer = []string{"system"}
	resolverSet := make(map[string]struct{})
	policySet := make(map[string]map[string]struct{})
	namedProxies := make([]map[string]any, 0, len(proxies))
	for _, proxy := range proxies {
		named := make(map[string]any, len(proxy)+1)
		for k, v := range proxy {
			if k == "__ech-resolver" {
				resolver, _ := v.(string)
				if resolver != "" {
					resolverSet[resolver] = struct{}{}
					if opts, ok := proxy["ech-opts"].(map[string]any); ok {
						queryName, _ := opts["query-server-name"].(string)
						if queryName != "" {
							if policySet[queryName] == nil {
								policySet[queryName] = make(map[string]struct{})
							}
							policySet[queryName][resolver] = struct{}{}
						}
					}
				}
				continue
			}
			named[k] = v
		}
		namedProxies = append(namedProxies, named)
	}
	if len(resolverSet) == 0 {
		config.DNS = nil
		return namedProxies
	}
	for resolver := range resolverSet {
		echDNS.ProxyServerNameserver = append(echDNS.ProxyServerNameserver, resolver)
	}
	sort.Strings(echDNS.ProxyServerNameserver)
	echDNS.ProxyServerNameserverPolicy = make(map[string][]string, len(policySet))
	for queryName, servers := range policySet {
		for resolver := range servers {
			echDNS.ProxyServerNameserverPolicy[queryName] = append(echDNS.ProxyServerNameserverPolicy[queryName], resolver)
		}
		sort.Strings(echDNS.ProxyServerNameserverPolicy[queryName])
	}
	config.DNS = &echDNS
	return namedProxies
}

// ConfigureProxyBatch loads a wave's proxies into one Mihomo instance and
// returns the listener URL for each of them.
//
// The base port stays assigned to the original entry: reusing it for a listener
// during a reload can fail to bind before that entry has closed, which would
// silently black-hole the whole wave.
func ConfigureProxyBatch(ctx context.Context, instance MihomoConfig, proxies []domain.ProxyConfig) ([]string, error) {
	base, err := url.Parse(instance.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}
	port := 0
	if base.Port() != "" {
		port, err = strconv.Atoi(base.Port())
		if err != nil {
			return nil, fmt.Errorf("parse proxy port: %w", err)
		}
	} else if base.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}
	// Segmentation monitor (REFACTOR_PLAN §6.3): each wave allocates listener
	// ports upward from base, so warn long before the fatal range error makes
	// the whole wave fail.
	if headroom := 65535 - port; headroom < 4096 {
		log.Printf("mihomo: proxy port segment nearly exhausted: base=%d free=%d next wave=%d", port, headroom, len(proxies))
	}
	if port < 1 || port+len(proxies) > 65535 {
		return nil, fmt.Errorf("代理配置端口范围无效: base=%d proxies=%d", port, len(proxies))
	}
	controllerPort := 9090
	if c, err := url.Parse(instance.Controller); err == nil && c.Port() != "" {
		if p, err := strconv.Atoi(c.Port()); err == nil {
			controllerPort = p
		}
	}
	secret := instance.Secret
	if secret == "" {
		secret = ""
	}
	config := mihomoConfig{
		MixedPort:          port,
		AllowLan:           true,
		BindAddress:        "*",
		Mode:               "rule",
		ExternalController: fmt.Sprintf("0.0.0.0:%d", controllerPort),
		Secret:             secret,
		Rules:              []string{"MATCH,REJECT"},
	}
	urls := make([]string, len(proxies))
	config.Proxies = applyECHDNS(&config, proxies)
	for i := range proxies {
		named := config.Proxies[i]
		named["name"] = fmt.Sprintf("ippure-%d", i)
		listenPort := port + i + 1
		config.Listeners = append(config.Listeners, mihomoListener{
			Name:   fmt.Sprintf("ippure-%d", i),
			Type:   "mixed",
			Port:   listenPort,
			Listen: "0.0.0.0",
			Proxy:  fmt.Sprintf("ippure-%d", i),
		})
		listenURL := *base
		listenURL.Host = fmt.Sprintf("%s:%d", base.Hostname(), listenPort)
		urls[i] = listenURL.String()
	}
	payload, err := marshalMihomoConfig(config)
	if err != nil {
		return nil, fmt.Errorf("render mihomo config: %w", err)
	}
	body, err := json.Marshal(map[string]string{"payload": string(payload)})
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSuffix(instance.Controller, "/") + "/configs?force=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("mihomo reload: %w", err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusBadRequest {
			var detail struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(responseBody, &detail) == nil {
				if match := proxyErrorIndex.FindStringSubmatch(detail.Message); len(match) == 2 {
					index, err := strconv.Atoi(match[1])
					if err == nil && index >= 0 && index < len(proxies) {
						return nil, &ProxyConfigError{Index: index}
					}
				}
			}
		}
		// Never surface the controller response or submitted configuration.
		return nil, fmt.Errorf("代理配置失败 (HTTP %d)", response.StatusCode)
	}
	return urls, nil
}

// JSON-backed proxy options contain float64 values. yaml.v3 renders large
// integral floats as 1e+06; Mihomo's weak integer decoder cannot parse that
// notation for options such as sc-max-each-post-bytes. Encode integral values
// as YAML integers, including options nested inside transport maps and lists.
func marshalMihomoConfig(config mihomoConfig) ([]byte, error) {
	var document yaml.Node
	if err := document.Encode(config); err != nil {
		return nil, err
	}
	var normalize func(*yaml.Node)
	normalize = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && node.Tag == "!!float" {
			value, err := strconv.ParseFloat(node.Value, 64)
			if err == nil && !math.IsInf(value, 0) && !math.IsNaN(value) && math.Trunc(value) == value {
				node.Tag = "!!int"
				node.Value = strconv.FormatFloat(value, 'f', -1, 64)
			}
		}
		for _, child := range node.Content {
			normalize(child)
		}
	}
	normalize(&document)
	return yaml.Marshal(&document)
}

// listenerURLs reproduces the port mapping for an already loaded configuration.
func listenerURLs(base string, count int) ([]string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	port := 0
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return nil, err
		}
	} else if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}
	out := make([]string, count)
	for i := 0; i < count; i++ {
		listenURL := *u
		listenURL.Host = fmt.Sprintf("%s:%d", u.Hostname(), port+i+1)
		out[i] = listenURL.String()
	}
	return out, nil
}

// reloadGuard serialises reloads per instance so two engines sharing one
// Mihomo cannot interleave a configuration push.
var reloadGuard sync.Mutex

// GuardReload runs fn under the global reload lock.
func GuardReload(fn func() error) error {
	reloadGuard.Lock()
	defer reloadGuard.Unlock()
	return fn()
}
