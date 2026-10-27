// Package qilan imports the Qilan panel's member list on a schedule.
package qilan

import (
	"context"
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
	City         string `json:"city"`
	Org          string `json:"org"`
	IPScore      any    `json:"ip_score"`
	Native       bool   `json:"is_native"`
	Black        bool   `json:"is_black"`
	TrafficRate  any    `json:"traffic_rate"`
	FailCount    any    `json:"fail_count"`
}

// Conversion and validation belong to the third-party converter, never this HTTP adapter.
func normalizeNode(node qilanNode) *domain.Node {
	name := node.Name
	if name == "" {
		name = fmt.Sprintf("Qilan %v", node.ID)
	}
	return &domain.Node{Name: name, RawLink: node.Link, SubTag: "qilan"}
}

// FetchNodes crawls the member list. A truncated round is never published:
// it would mark every missing connection as departed and burn probe budget.
func FetchNodes(ctx context.Context, client *http.Client, session, userAgent string, sleep func(time.Duration)) (domain.SourceBatch, error) {
	if strings.TrimSpace(session) == "" {
		return domain.SourceBatch{}, fmt.Errorf("Qilan session cookie is empty")
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
				return domain.SourceBatch{}, err
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
				return domain.SourceBatch{}, err
			}
			if payload.Data == nil {
				return domain.SourceBatch{}, fmt.Errorf("Qilan returned an invalid node page")
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
				return domain.SourceBatch{}, fmt.Errorf(
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
					return domain.SourceBatch{}, ctx.Err()
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
			key := fmt.Sprintf("%v:%s", item.ID, item.Link)
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
			return domain.SourceBatch{}, fmt.Errorf("Qilan pagination exceeded %d pages", maxPages)
		}
		select {
		case <-ctx.Done():
			return domain.SourceBatch{}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if total != nil && *total-received > missingTolerance(*total) {
		return domain.SourceBatch{}, fmt.Errorf(
			"Qilan returned an incomplete node page set (%d/%d); incomplete fetch discarded", received, *total)
	}
	if len(nodes) == 0 {
		return domain.SourceBatch{}, fmt.Errorf("Qilan returned no usable nodes")
	}
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := domain.SourceBatch{Nodes: make([]domain.Node, 0, len(keys))}
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
	ingestor    Ingestor
	session     string
	sessionFile string
	userAgent   string
	interval    time.Duration
	retention   float64
	onError     func(error)

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	pending  bool
	lastSize int
}

// Ingestor is the subset of the import pipeline the source needs.
type Ingestor interface {
	Import(ctx context.Context, snap domain.SourceBatch) error
}

// SourceConfig configures the Qilan source.
type SourceConfig struct {
	Session      string
	SessionFile  string
	UserAgent    string
	Interval     time.Duration
	MinRetention float64
	OnError      func(error)
}

// NewSource builds a Qilan source.
func NewSource(ingestor Ingestor, cfg SourceConfig) *Source {
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
		ingestor:    ingestor,
		session:     cfg.Session,
		sessionFile: cfg.SessionFile,
		userAgent:   cfg.UserAgent,
		interval:    cfg.Interval,
		retention:   cfg.MinRetention,
		onError:     cfg.OnError,
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
		return s.handleFailure(err)
	}
	batch, err := FetchNodes(ctx, nil, session, s.userAgent, nil)
	if err != nil {
		return s.handleFailure(err)
	}
	// Upstream can understate `total` itself, so what matters is not absolute
	// size but the step: a real panel does not shed 40% of its members between
	// two rounds.
	s.mu.Lock()
	lastSize := s.lastSize
	collapsed := lastSize > 0 && len(batch.Nodes) < int(float64(s.lastSize)*s.retention)
	s.mu.Unlock()
	if collapsed {
		return s.handleFailure(fmt.Errorf(
			"Qilan member count collapsed from %d to %d; round discarded", lastSize, len(batch.Nodes)))
	}
	if err := s.ingestor.Import(ctx, batch); err != nil {
		return s.handleFailure(err)
	}
	s.mu.Lock()
	s.lastSize = len(batch.Nodes)
	s.mu.Unlock()
	return nil
}

func (s *Source) handleFailure(err error) error {
	if s.onError != nil {
		s.onError(err)
	}
	return err
}

// Start runs the source until the context is cancelled.
func (s *Source) Start(ctx context.Context) {
	s.mu.Lock()
	if s.closed || s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
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
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
