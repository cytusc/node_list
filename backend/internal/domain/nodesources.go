package domain

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// detailPaths lists the upstream summary columns extracted from a Clash proxy.
var detailPaths = []struct {
	key  string
	path []string
}{
	{"cipher", []string{"cipher"}},
	{"flow", []string{"flow"}},
	{"client-fingerprint", []string{"client-fingerprint"}},
	{"alpn", []string{"alpn"}},
	{"skip-cert-verify", []string{"skip-cert-verify"}},
	{"ws-path", []string{"ws-opts", "path"}},
	{"ws-host", []string{"ws-opts", "headers", "Host"}},
	{"grpc-service-name", []string{"grpc-opts", "grpc-service-name"}},
	{"h2-path", []string{"h2-opts", "path"}},
	{"http-path", []string{"http-opts", "path"}},
	{"plugin", []string{"plugin"}},
	{"obfs", []string{"obfs"}},
	{"protocol", []string{"protocol"}},
	{"up", []string{"up"}},
	{"down", []string{"down"}},
	{"ports", []string{"ports"}},
	{"congestion-controller", []string{"congestion-controller"}},
	{"udp-relay-mode", []string{"udp-relay-mode"}},
	{"version", []string{"version"}},
}

func dig(value any, path []string) any {
	current := value
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func formatDetail(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s := formatDetail(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconvFloat(t)
	case int:
		return fmt.Sprintf("%d", t)
	case string:
		return strings.TrimSpace(t)
	default:
		b, _ := json.Marshal(t)
		return strings.TrimSpace(string(b))
	}
}

func strconvFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", f), "0"), ".")
}

// SummarizeProxy mirrors the upstream results summary so a reused display name
// never supplies another endpoint's credentials. Arrays use the upstream
// comma-space format, which keeps the identity stable across versions.
func SummarizeProxy(proxy ProxyConfig) Node {
	nodeType := str(proxy["type"])
	quic := nodeType == "hysteria" || nodeType == "hysteria2" || nodeType == "tuic"
	realityOpts, _ := proxy["reality-opts"].(map[string]any)
	reality := len(realityOpts) > 0
	tls := nodeType == "trojan" || nodeType == "anytls" || quic || truthy(proxy["tls"]) || reality
	sni := ""
	if tls {
		if v := str(proxy["servername"]); v != "" {
			sni = v
		} else {
			sni = str(proxy["sni"])
		}
	}
	network := str(proxy["network"])
	if network == "" {
		switch {
		case quic:
			network = "quic"
		case nodeType == "wireguard":
			network = "udp"
		default:
			network = "tcp"
		}
	}
	details := make([]Detail, 0, len(detailPaths))
	for _, entry := range detailPaths {
		v := dig(proxy, entry.path)
		formatted := formatDetail(v)
		if v == nil || formatted == "" {
			continue
		}
		details = append(details, Detail{K: entry.key, V: formatted})
	}
	return Node{
		Name:    str(proxy["name"]),
		Type:    nodeType,
		Server:  str(proxy["server"]),
		Port:    fmt.Sprintf("%v", proxy["port"]),
		TLS:     tls,
		Reality: reality,
		SNI:     sni,
		UDP:     quic || truthy(proxy["udp"]),
		Network: network,
		Details: details,
	}
}

// ParseShareLinks decodes a subscription text file into name -> link. A name
// with more than one distinct link cannot establish provenance and is dropped.
func ParseShareLinks(raw string) (map[string]string, error) {
	text := raw
	if !strings.Contains(text, "://") {
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
		if err != nil {
			// Tolerate unpadded input before rejecting the file.
			if b, err2 := base64.RawStdEncoding.DecodeString(strings.Join(strings.Fields(text), "")); err2 == nil {
				decoded = b
			} else {
				return nil, err
			}
		}
		text = string(decoded)
	}
	grouped := make(map[string]map[string]struct{})
	for _, line := range strings.Split(text, "\n") {
		value := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if value == "" || !strings.Contains(value, "://") {
			continue
		}
		u, err := url.Parse(value)
		if err != nil {
			continue
		}
		name, err := url.QueryUnescape(strings.TrimPrefix(u.Fragment, "#"))
		if err != nil || name == "" {
			continue
		}
		if grouped[name] == nil {
			grouped[name] = make(map[string]struct{})
		}
		grouped[name][value] = struct{}{}
	}
	links := make(map[string]string, len(grouped))
	for name, values := range grouped {
		if len(values) != 1 {
			continue
		}
		for v := range values {
			links[name] = v
		}
	}
	return links, nil
}

// Sources holds the raw material used to attach credentials to archived nodes.
type Sources struct {
	Links   map[string]string
	Configs map[string][]ProxyConfig
}

// ReadNodeSources loads a Clash subscription and, when present, the share-link
// file that pairs a display name with its original link.
func ReadNodeSources(clashFile, linksFile string) (*Sources, error) {
	document, err := readClashFile(clashFile)
	if err != nil {
		return nil, err
	}
	proxies, ok := document["proxies"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid source configuration: %s", clashFile)
	}
	links := make(map[string]string)
	if linksFile != "" {
		if raw, err := os.ReadFile(linksFile); err == nil {
			if parsed, err := ParseShareLinks(string(raw)); err == nil {
				links = parsed
			}
		}
	}
	configs := make(map[string][]ProxyConfig, len(proxies))
	for _, item := range proxies {
		proxy, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if str(proxy["type"]) == "" || str(proxy["server"]) == "" || proxy["port"] == nil {
			return nil, fmt.Errorf("incomplete source configuration: %s", clashFile)
		}
		summary := SummarizeProxy(proxy)
		key, err := NodeIdentity(summary)
		if err != nil {
			return nil, err
		}
		configs[key] = append(configs[key], proxy)
	}
	return &Sources{Links: links, Configs: configs}, nil
}

// ReadShareSources loads a subscription that only publishes share links.
func ReadShareSources(file string) (*Sources, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	links, err := ParseShareLinks(string(raw))
	if err != nil || len(links) == 0 {
		return nil, fmt.Errorf("no usable source links: %s", file)
	}
	return &Sources{Links: links}, nil
}

func readClashFile(file string) (map[string]any, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return document, nil
}

// Source is the provenance attached to one archived node.
type Source struct {
	RawConfig ProxyConfig
	RawLink   string
}

// SourceForNode finds the credentials for a node. Ambiguous configurations are
// rejected rather than attached arbitrarily: old summaries omitted credentials,
// so several endpoints can share one summary identity.
func SourceForNode(n Node, sources *Sources, exactNames bool) *Source {
	if sources == nil {
		return nil
	}
	if sources.Configs == nil {
		rawLink := sources.Links[n.Name]
		if rawLink == "" || !LinkMatchesConfig(rawLink, n) {
			return nil
		}
		return &Source{RawLink: rawLink}
	}
	key, err := NodeIdentity(n)
	if err != nil {
		return nil
	}
	matches := sources.Configs[key]
	if exactNames {
		filtered := matches[:0:0]
		for _, proxy := range matches {
			if str(proxy["name"]) == n.Name {
				filtered = append(filtered, proxy)
			}
		}
		matches = filtered
	}
	variants := make(map[string]ProxyConfig, len(matches))
	order := make([]string, 0, len(matches))
	for _, proxy := range matches {
		normalized := connectionConfig(proxy)
		b, err := canonicalJSON(normalized)
		if err != nil {
			continue
		}
		k := string(b)
		if _, ok := variants[k]; !ok {
			order = append(order, k)
			variants[k] = proxy
		}
	}
	if len(variants) != 1 {
		return nil
	}
	rawConfig := variants[order[0]]
	source := &Source{RawConfig: rawConfig}
	if candidate, ok := sources.Links[str(rawConfig["name"])]; ok && LinkMatchesConfig(candidate, rawConfig) {
		source.RawLink = candidate
	}
	return source
}

// LinkMatchesConfig verifies that a share link describes the same endpoint and
// credentials as a Clash configuration before the link is stored as provenance.
func LinkMatchesConfig(link string, config any) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	proxy, ok := config.(ProxyConfig)
	if !ok {
		return false
	}
	nodeType := strings.ToLower(strings.TrimSuffix(u.Scheme, ":"))
	if nodeType == "hy2" {
		nodeType = "hysteria2"
	}
	if nodeType != str(proxy["type"]) {
		return false
	}
	if strings.ToLower(strings.Trim(u.Hostname(), "[]")) != strings.ToLower(str(proxy["server"])) {
		return false
	}
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = "443"
	}
	if port != fmt.Sprintf("%v", proxy["port"]) {
		return false
	}
	user, _ := url.QueryUnescape(u.User.Username())
	secret, _ := u.User.Password()
	password, _ := url.QueryUnescape(secret)
	if str(proxy["type"]) == "vless" && str(proxy["uuid"]) != "" {
		return user == str(proxy["uuid"])
	}
	switch str(proxy["type"]) {
	case "trojan", "hysteria2", "anytls":
		if str(proxy["password"]) != "" {
			expected := user
			if password != "" {
				expected = user + ":" + password
			}
			return expected == str(proxy["password"])
		}
	case "ss":
		if str(proxy["cipher"]) != "" && str(proxy["password"]) != "" {
			auth := user
			if password != "" {
				auth = user + ":" + password
			} else if decoded, err := decodeBase64(user); err == nil {
				auth = decoded
			}
			return auth == str(proxy["cipher"])+":"+str(proxy["password"])
		}
	}
	if str(proxy["password"]) == "" {
		return true
	}
	return password == str(proxy["password"])
}

// ReconcileResult is the outcome of attaching provenance to one snapshot.
type ReconcileResult struct {
	Snapshot Snapshot
	// Indexes maps the original node position to its position after pruning.
	Indexes map[int]int
	Changed bool
}

// ReconcileSnapshot attaches archived credentials and optionally drops nodes
// whose configuration is gone. Captured credentials describe their own
// detection round and are never overwritten by a later subscription.
func ReconcileSnapshot(s Snapshot, linked map[string]Source, prune bool) ReconcileResult {
	indexes := make(map[int]int, len(s.Nodes))
	nodes := make([]Node, 0, len(s.Nodes))
	changed := false
	for oldIndex, node := range s.Nodes {
		updated := node
		if node.RawConfig == nil && node.RawLink == "" {
			if source, ok := linked[SourceKey(node)]; ok {
				updated.RawConfig = source.RawConfig
				updated.RawLink = source.RawLink
			}
		}
		if prune && updated.RawLink == "" && len(updated.RawConfig) == 0 {
			changed = true
			continue
		}
		indexes[oldIndex] = len(nodes)
		if !sameNode(updated, node) {
			changed = true
		}
		nodes = append(nodes, updated)
	}
	out := s
	out.Nodes = nodes
	return ReconcileResult{Snapshot: out, Indexes: indexes, Changed: changed}
}

func sameNode(a, b Node) bool {
	left, errL := json.Marshal(a)
	right, errR := json.Marshal(b)
	if errL != nil || errR != nil {
		return false
	}
	return string(left) == string(right)
}

// SortedIdentities returns the identity of every node in a snapshot, in order.
// The projection keeps them so departure filtering never recomputes hashes.
func SortedIdentities(s Snapshot) []string {
	out := make([]string, len(s.Nodes))
	for i, n := range s.Nodes {
		out[i] = AggregateIdentity(n)
	}
	return out
}

// DetailKeys is exposed for tests that need deterministic detail ordering.
func DetailKeys() []string {
	keys := make([]string, 0, len(detailPaths))
	for _, entry := range detailPaths {
		keys = append(keys, entry.key)
	}
	sort.Strings(keys)
	return keys
}
