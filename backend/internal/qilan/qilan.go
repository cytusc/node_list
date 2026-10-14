// Package qilan imports the Qilan panel's member list on a schedule.
package qilan

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/notify"
	"github.com/cytusc/node_list/backend/internal/store"
)

const (
	baseURL     = "https://www.qilan.de"
	pageSize    = 200
	maxPages    = 60
	attemptsPer = 3
)

// missingTolerance tolerates a few rows vanishing between the count and the last
// page; anything larger means the crawl was truncated and must not publish.
func missingTolerance(total int) int {
	if t := int(float64(total) * 0.05); t > 5 {
		return t
	}
	return 5
}

type qilanNode struct {
	ID           any    `json:"id"`
	Name         string `json:"name"`
	Protocol     string `json:"protocol"`
	Server       string `json:"server"`
	Port         any    `json:"port"`
	Link         string `json:"link"`
	Ping         any    `json:"ping"`
	Status       string `json:"status"`
	IPType       string `json:"ip_type"`
	ISP          string `json:"isp"`
	ASN          any    `json:"asn"`
	Region       string `json:"region"`
	Network      string `json:"network"`
	LastCheck    string `json:"last_check"`
	FeaturesList []any  `json:"features_list"`
}

func linkSettings(link string) (tls bool, sni string) {
	if strings.HasPrefix(link, "vmess://") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(link, "vmess://"))
		if err != nil {
			return false, ""
		}
		var config map[string]any
		if err := json.Unmarshal(raw, &config); err != nil {
			return false, ""
		}
		sni, _ := config["sni"].(string)
		if sni == "" {
			sni, _ = config["host"].(string)
		}
		return truthy(config["tls"]), sni
	}
	u, err := url.Parse(link)
	if err != nil {
		return false, ""
	}
	nodeType := strings.TrimSuffix(u.Scheme, ":")
	security := u.Query().Get("security")
	tls = nodeType == "trojan" || nodeType == "hy2" || nodeType == "hysteria2" ||
		nodeType == "anytls" || security == "tls" || security == "reality"
	sni = u.Query().Get("sni")
	if sni == "" {
		sni = u.Query().Get("serverName")
	}
	return tls, sni
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// normalizeNode converts one panel row into the stored node shape.
func normalizeNode(node qilanNode) *domain.Node {
	if node.ID == nil || node.Protocol == "" || node.Server == "" ||
		node.Port == nil || node.Link == "" {
		return nil
	}
	name := node.Name
	if name == "" {
		name = fmt.Sprintf("Qilan %v", node.ID)
	}
	tls, sni := linkSettings(node.Link)
	details := []domain.Detail{{K: "source-id", V: scalar(node.ID)}}
	if ping, ok := toFloat(node.Ping); ok && ping > 0 {
		details = append(details, domain.Detail{K: "latency", V: fmt.Sprintf("%d ms", int64(ping))})
	}
	if node.Status != "" {
		details = append(details, domain.Detail{K: "status", V: node.Status})
	}
	if node.IPType != "" {
		details = append(details, domain.Detail{K: "ip-type", V: node.IPType})
	}
	if node.ISP != "" {
		details = append(details, domain.Detail{K: "isp", V: node.ISP})
	}
	if asn := scalar(node.ASN); asn != "" {
		details = append(details, domain.Detail{K: "asn", V: asn})
	}
	if len(node.FeaturesList) > 0 {
		parts := make([]string, 0, len(node.FeaturesList))
		for _, item := range node.FeaturesList {
			if s := scalar(item); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			details = append(details, domain.Detail{K: "features", V: strings.Join(parts, ", ")})
		}
	}
	network := strings.ToLower(node.Network)
	if network == "" {
		network = "tcp"
	}
	out := &domain.Node{
		Name:     name + " [Qilan]",
		BaseName: name,
		Type:     strings.ToLower(node.Protocol),
		Server:   node.Server,
		Port:     scalar(node.Port),
		TLS:      tls,
		Network:  network,
		SubTag:   "Qilan",
		Details:  details,
		RawLink:  node.Link,
	}
	if sni != "" {
		out.SNI = sni
	}
	if node.Region != "" {
		out.Country = node.Region
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// FetchSnapshot crawls the member list. A truncated round is never published:
// it would mark every missing connection as departed and burn probe budget.
func FetchSnapshot(ctx context.Context, client *http.Client, session, userAgent string, sleep func(time.Duration)) (domain.Snapshot, error) {
	if strings.TrimSpace(session) == "" {
		return domain.Snapshot{}, fmt.Errorf("Qilan session cookie is empty")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	nodes := make(map[string]struct {
		node        domain.Node
		checkedTime int64
	})
	var total *int
	received := 0
	newest := int64(0)
	for page := 1; ; page++ {
		var items []qilanNode
		for attempt := 0; attempt < attemptsPer; attempt++ {
			endpoint, err := url.Parse(baseURL + "/api/nodes")
			if err != nil {
				return domain.Snapshot{}, err
			}
			query := endpoint.Query()
			query.Set("page", strconv.Itoa(page))
			query.Set("limit", strconv.Itoa(pageSize))
			endpoint.RawQuery = query.Encode()

			var payload struct {
				Total *int        `json:"total"`
				Data  []qilanNode `json:"data"`
			}
			if err := fetchJSON(ctx, client, endpoint.String(), session, userAgent, &payload); err != nil {
				return domain.Snapshot{}, err
			}
			if payload.Data == nil {
				return domain.Snapshot{}, fmt.Errorf("Qilan returned an invalid node page")
			}
			if payload.Total != nil {
				total = payload.Total
			}
			items = payload.Data
			usable := false
			for _, item := range items {
				if strings.TrimSpace(item.Link) != "" {
					usable = true
					break
				}
			}
			if len(items) > 0 && !usable {
				return domain.Snapshot{}, fmt.Errorf(
					"Qilan session rejected: nodes returned without share links (stale session?)")
			}
			expected := pageSize
			if total != nil {
				if remaining := *total - received; remaining < pageSize {
					expected = remaining
				}
				if expected < 0 {
					expected = 0
				}
			}
			if len(items) >= expected {
				break
			}
			if attempt+1 < attemptsPer {
				select {
				case <-ctx.Done():
					return domain.Snapshot{}, ctx.Err()
				case <-time.After(time.Duration(300*pow3(attempt)) * time.Millisecond):
				}
			}
		}
		received += len(items)
		for _, item := range items {
			node := normalizeNode(item)
			if node == nil {
				continue
			}
			checked := int64(0)
			if t, err := time.Parse(time.RFC3339, item.LastCheck); err == nil {
				checked = t.UnixMilli()
				if checked > newest {
					newest = checked
				}
			}
			key := domain.AggregateIdentity(*node)
			if previous, ok := nodes[key]; !ok || checked > previous.checkedTime {
				nodes[key] = struct {
					node        domain.Node
					checkedTime int64
				}{*node, checked}
			}
		}
		if len(items) < pageSize {
			break
		}
		if total != nil && received >= *total {
			break
		}
		if page >= maxPages {
			return domain.Snapshot{}, fmt.Errorf("Qilan pagination exceeded %d pages", maxPages)
		}
		select {
		case <-ctx.Done():
			return domain.Snapshot{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if total != nil && *total-received > missingTolerance(*total) {
		return domain.Snapshot{}, fmt.Errorf(
			"Qilan returned an incomplete node page set (%d/%d); previous snapshot retained", received, *total)
	}
	if len(nodes) == 0 {
		return domain.Snapshot{}, fmt.Errorf("Qilan returned no usable nodes")
	}
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := domain.Snapshot{SpeedTest: false, MediaCheck: false, Nodes: make([]domain.Node, 0, len(keys))}
	for _, key := range keys {
		out.Nodes = append(out.Nodes, nodes[key].node)
	}
	if newest > 0 {
		out.CheckedAt = time.UnixMilli(newest).UTC().Format(time.RFC3339)
	} else {
		out.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return out, nil
}

func pow3(n int) int {
	result := 1
	for i := 0; i < n; i++ {
		result *= 3
	}
	return result
}

func fetchJSON(ctx context.Context, client *http.Client, endpoint, session, userAgent string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", "session="+strings.TrimSpace(session))
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
	}()
	if response.StatusCode == 403 {
		return fmt.Errorf("Qilan rejected the session cookie (HTTP 403)")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Qilan request failed (HTTP %d)", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(out); err != nil {
		return fmt.Errorf("Qilan returned an invalid node page")
	}
	return nil
}

// LoadSession reads the session cookie from the environment or a mounted file.
// The file is re-read every round, so a rotated cookie needs no restart.
func LoadSession(session, sessionFile string) (string, error) {
	if strings.TrimSpace(session) != "" {
		return strings.TrimSpace(session), nil
	}
	if sessionFile == "" {
		return "", fmt.Errorf("Configure QILAN_SESSION or QILAN_SESSION_FILE")
	}
	raw, err := os.ReadFile(sessionFile)
	if err == nil && strings.TrimSpace(string(raw)) != "" {
		return strings.TrimSpace(string(raw)), nil
	}
	return "", fmt.Errorf("Qilan session file is missing or empty")
}

// Source refreshes the Qilan panel on an interval.
type Source struct {
	store       *store.Store
	ingestor    Ingestor
	session     string
	sessionFile string
	userAgent   string
	interval    time.Duration
	retention   float64
	notifier    func(context.Context, string) error
	onError     func(error)

	mu       sync.Mutex
	closed   bool
	pending  bool
	lastSize int
	broken   bool
	shrunken bool
}

// Ingestor is the subset of the import pipeline the source needs.
type Ingestor interface {
	Import(ctx context.Context, snap domain.Snapshot, sources *domain.Sources) error
}

// SourceConfig configures the Qilan source.
type SourceConfig struct {
	Session      string
	SessionFile  string
	UserAgent    string
	Interval     time.Duration
	MinRetention float64
	Webhook      string
}

// NewSource builds a Qilan source.
func NewSource(st *store.Store, ingestor Ingestor, cfg SourceConfig) *Source {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Minute
	}
	if cfg.MinRetention <= 0 {
		cfg.MinRetention = 0.6
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0"
	}
	return &Source{
		store:       st,
		ingestor:    ingestor,
		session:     cfg.Session,
		sessionFile: cfg.SessionFile,
		userAgent:   cfg.UserAgent,
		interval:    cfg.Interval,
		retention:   cfg.MinRetention,
		notifier:    notify.Feishu(cfg.Webhook),
		onError:     func(err error) { fmt.Printf("Qilan source refresh failed: %v\n", err) },
	}
}

// Refresh runs one round. Concurrent calls share the in-flight round.
func (s *Source) Refresh(ctx context.Context) error {
	s.mu.Lock()
	if s.closed || s.pending {
		s.mu.Unlock()
		return nil
	}
	s.pending = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.pending = false
		s.mu.Unlock()
	}()

	session, err := LoadSession(s.session, s.sessionFile)
	if err != nil {
		return s.handleFailure(ctx, err)
	}
	snapshot, err := FetchSnapshot(ctx, nil, session, s.userAgent, nil)
	if err != nil {
		return s.handleFailure(ctx, err)
	}
	// Upstream can understate `total` itself, so what matters is not absolute
	// size but the step: a real panel does not shed 40% of its members between
	// two rounds.
	s.mu.Lock()
	collapsed := s.lastSize > 0 && len(snapshot.Nodes) < int(float64(s.lastSize)*s.retention)
	s.mu.Unlock()
	if collapsed {
		return s.handleFailure(ctx, fmt.Errorf(
			"Qilan member count collapsed from %d to %d; round discarded", s.lastSize, len(snapshot.Nodes)))
	}
	if err := s.ingestor.Import(ctx, snapshot, nil); err != nil {
		return s.handleFailure(ctx, err)
	}
	s.mu.Lock()
	s.lastSize = len(snapshot.Nodes)
	wasBroken, wasShrunken := s.broken, s.shrunken
	s.broken, s.shrunken = false, false
	s.mu.Unlock()
	if wasBroken {
		_ = s.notifier(ctx, fmt.Sprintf(
			"✅ Qilan 登录状态已恢复，节点快照同步继续。\n时间: %s", time.Now().UTC().Format(time.RFC3339)))
	}
	if wasShrunken {
		_ = s.notifier(ctx, fmt.Sprintf(
			"✅ Qilan 抓取已恢复正常，本轮入库 %d 个节点。\n时间: %s",
			len(snapshot.Nodes), time.Now().UTC().Format(time.RFC3339)))
	}
	return nil
}

// handleFailure logs the error and escalates the two failure modes that degrade
// silently: an expired session and a throttled upstream that under-reports.
func (s *Source) handleFailure(ctx context.Context, err error) error {
	if s.onError != nil {
		s.onError(err)
	}
	message := err.Error()
	s.mu.Lock()
	sessionFailure := containsFold(message, "session") || containsFold(message, "403")
	wasBroken := s.broken
	if sessionFailure {
		s.broken = true
	}
	collapsed := containsFold(message, "member count collapsed")
	wasShrunken := s.shrunken
	if collapsed {
		s.shrunken = true
	}
	s.mu.Unlock()
	if sessionFailure && !wasBroken {
		_ = s.notifier(ctx, fmt.Sprintf(
			"⚠️ Qilan 登录状态失效，节点快照同步已暂停。\n原因: %s\n时间: %s\n请更新 QILAN_SESSION（session.txt）后等待自动恢复。",
			message, time.Now().UTC().Format(time.RFC3339)))
	} else if collapsed && !wasShrunken {
		_ = s.notifier(ctx, fmt.Sprintf(
			"⚠️ Qilan 本轮成员数骤降，已判定为抓取降级并丢弃该轮（保留上一快照）。\n原因: %s\n时间: %s",
			message, time.Now().UTC().Format(time.RFC3339)))
	}
	return err
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// Start runs the source until the context is cancelled.
func (s *Source) Start(ctx context.Context) {
	go func() {
		_ = s.Refresh(ctx)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.Refresh(ctx)
			}
		}
	}()
}

// Close stops the source.
func (s *Source) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}
