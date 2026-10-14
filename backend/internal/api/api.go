// Package api serves the HTTP surface the frontend and the smoke tests use.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/store/sqlite"
)

// CollectorStatus is the enrichment collector health shown next to results.
// CollectorStatus mirrors the JavaScript collector?.status(). A disabled
// collector serialises as just {"enabled":false} because the optional fields
// were undefined, so they are omitted instead of zero.
type CollectorStatus struct {
	Enabled     bool   `json:"enabled"`
	Running     bool   `json:"running,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
	Transport   string `json:"transport,omitempty"`
	Queued      int    `json:"queued,omitempty"`
	Probed      int64  `json:"probed,omitempty"`
	LastRun     string `json:"lastRunAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Server is the HTTP surface.
type Server struct {
	store       *store.Store
	projector   *postgres.Projector
	collector   func() CollectorStatus
	distDir     string
	hasPostgres bool
}

// ServerConfig wires the handlers.
type ServerConfig struct {
	Store       *store.Store
	Projector   *postgres.Projector
	Collector   func() CollectorStatus
	DistDir     string
	HasPostgres bool
}

// New builds the HTTP handler.
func New(cfg ServerConfig) *Server {
	return &Server{
		store:       cfg.Store,
		projector:   cfg.Projector,
		collector:   cfg.Collector,
		distDir:     cfg.DistDir,
		hasPostgres: cfg.HasPostgres,
	}
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.health)
	mux.HandleFunc("/api/history", s.history)
	mux.HandleFunc("/api/results", s.results)
	mux.HandleFunc("/api/downloads", notFound("订阅下载暂不可用。"))
	mux.HandleFunc("/api/download", notFound("订阅下载暂不可用。"))
	mux.Handle("/api/", http.HandlerFunc(notFound("接口不存在。")))
	if s.distDir != "" {
		files := http.FileServer(http.Dir(s.distDir))
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				notFound("接口不存在。")(w, r)
				return
			}
			files.ServeHTTP(w, r)
		}))
	}
	return mux
}

func notFound(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": message})
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	items, total, err := s.store.History(r.Context(), limit, offset)
	if err != nil {
		writeUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	rawID := query.Get("snapshot")
	aggregate := query.Get("view") == "aggregate"
	if query.Has("view") && !aggregate {
		writeError(w, http.StatusBadRequest, "检测结果视图无效。", "")
		return
	}
	if aggregate && rawID != "" {
		writeError(w, http.StatusBadRequest, "检测结果视图无效。", "")
		return
	}
	var id *int
	if rawID != "" {
		parsed, err := strconv.Atoi(rawID)
		if err != nil || parsed < 1 || !strings.EqualFold(strconv.Itoa(parsed), rawID) {
			writeError(w, http.StatusBadRequest, "检测批次无效。", "")
			return
		}
		id = &parsed
	}
	parsed, err := parseResultsQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "节点分页、筛选或排序参数无效。", "")
		return
	}
	databasePage := aggregate && parsed != nil && s.hasPostgres && s.projector != nil
	if !databasePage {
		// Non-aggregate views read the newest batch, so the request drives a
		// refresh when the upstream files changed.
		if err := s.store.Refresh(r.Context()); err != nil {
			fmt.Printf("results refresh failed: %v\n", err)
		}
	}
	status := CollectorStatus{Enabled: false}
	if s.collector != nil {
		status = s.collector()
	}
	if databasePage {
		page, err := s.projector.QueryPage(r.Context(), &postgres.Query{
			Page:         parsed.Page,
			Limit:        parsed.Limit,
			Search:       parsed.Search,
			Countries:    parsed.Countries,
			Types:        parsed.Types,
			Networks:     parsed.Networks,
			Features:     parsed.Features,
			IPSources:    parsed.IPSources,
			IPAttributes: parsed.IPAttributes,
			Media:        parsed.Media,
			MinSpeed:     parsed.MinSpeed,
			Sort:         parsed.Sort,
			Direction:    parsed.Direction,
		})
		if err != nil || page == nil {
			writeUnavailable(w)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			postgres.Result
			Collector CollectorStatus `json:"ippureCollector"`
		}{*page, status})
		return
	}
	// Without a SQL projection the aggregate view has to be folded in memory,
	// exactly as the JavaScript store did. Retired connections stay hidden.
	var snapshot *domain.Snapshot
	if aggregate {
		snapshot = s.store.Aggregate(false)
		if snapshot != nil {
			// Aggregate returns a shared snapshot; the caller mutates it below.
			copied := snapshot.Clone()
			snapshot = &copied
		}
	} else {
		snapshot, err = s.store.Get(id)
		if err != nil {
			writeUnavailable(w)
			return
		}
	}
	if snapshot == nil {
		if id != nil {
			writeError(w, http.StatusNotFound, "该历史批次不存在。", "SNAPSHOT_NOT_FOUND")
			return
		}
		writeError(w, http.StatusNotFound,
			"尚未找到检测结果，请确认 subs-check 已完成至少一轮检测。", "NO_RESULTS")
		return
	}
	public := publicSnapshot(*snapshot)
	if parsed != nil {
		writeJSON(w, http.StatusOK, paginate(public, parsed, status))
		return
	}
	writeJSON(w, http.StatusOK, snapshotResponse{
		ID:            public.ID,
		CheckedAt:     public.CheckedAt,
		SpeedTest:     public.SpeedTest,
		MediaCheck:    public.MediaCheck,
		Nodes:         public.Nodes,
		BatchCount:    public.BatchCount,
		SourceWarning: sourceWarningPtr(public.SourceWarning),
		Collector:     status,
	})
}

// publicSnapshot strips credentials: raw links and configurations must never
// reach the browser.
func publicSnapshot(s domain.Snapshot) domain.Snapshot {
	if len(s.Nodes) == 0 {
		return s
	}
	nodes := make([]domain.Node, len(s.Nodes))
	copy(nodes, s.Nodes)
	for i := range nodes {
		nodes[i].RawLink = ""
		nodes[i].RawConfig = nil
	}
	s.Nodes = nodes
	return s
}

// resultsResponse is the shape the results view expects: snapshot metadata,
// facet counts and one page of nodes.
type resultsResponse struct {
	ID         int            `json:"id,omitempty"`
	CheckedAt  string         `json:"checkedAt,omitempty"`
	BatchCount int            `json:"batchCount,omitempty"`
	SpeedTest  bool           `json:"speedTest"`
	MediaCheck bool           `json:"mediaCheck"`
	TotalNodes int            `json:"totalNodes"`
	Summary    domain.Summary `json:"summary"`
	Facets     domain.Facets  `json:"facets"`
	Nodes      []domain.Node  `json:"nodes"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	Limit      int            `json:"limit"`
	// SourceWarning is always serialised: the front end distinguishes "the
	// upstream files read cleanly" (null) from a missing field.
	SourceWarning *string         `json:"sourceWarning"`
	Collector     CollectorStatus `json:"ippureCollector"`
}

// snapshotResponse is the unpaginated view: the newest batch verbatim, with the
// batch id and the source warning the JavaScript store attaches on the way out.
// The aggregate view spans every batch, so it carries no id.
type snapshotResponse struct {
	ID            int             `json:"id,omitempty"`
	CheckedAt     string          `json:"checkedAt,omitempty"`
	SpeedTest     bool            `json:"speedTest"`
	MediaCheck    bool            `json:"mediaCheck"`
	Nodes         []domain.Node   `json:"nodes"`
	BatchCount    int             `json:"batchCount,omitempty"`
	SourceWarning *string         `json:"sourceWarning"`
	Collector     CollectorStatus `json:"ippureCollector"`
}

// sourceWarningPtr converts the sentinel empty warning into the JSON null the
// browser expects, keeping a real warning as a string.
func sourceWarningPtr(warning string) *string {
	if warning == "" {
		return nil
	}
	return &warning
}

// paginate filters, sorts and pages a snapshot in memory. Used by the SQLite
// deployment and by historical batch views, which the SQL projection does not
// materialise.
func paginate(s domain.Snapshot, q *resultsQuery, status CollectorStatus) resultsResponse {
	metadata := domain.ResultsMetadata(s)
	matches := make([]domain.Node, 0, len(s.Nodes))
	for _, node := range s.Nodes {
		if matchAll(node, s, q) {
			matches = append(matches, node)
		}
	}
	ordered := domain.Snapshot{
		CheckedAt:  s.CheckedAt,
		SpeedTest:  s.SpeedTest,
		MediaCheck: s.MediaCheck,
		Nodes:      matches,
	}
	paged := sqlite.Paginate(ordered, &sqlite.Query{
		Page: q.Page, Limit: q.Limit, Sort: q.Sort, Direction: q.Direction,
	})
	page := q.Page
	if max := (len(matches) + q.Limit - 1) / q.Limit; max < 1 {
		page = 1
	} else if page > max {
		page = max
	}
	return resultsResponse{
		ID:            s.ID,
		CheckedAt:     s.CheckedAt,
		BatchCount:    s.BatchCount,
		SpeedTest:     s.SpeedTest,
		MediaCheck:    s.MediaCheck,
		TotalNodes:    len(s.Nodes),
		Summary:       metadata.Summary,
		Facets:        metadata.Facets,
		Nodes:         paged.Nodes,
		Total:         len(matches),
		Page:          page,
		Limit:         q.Limit,
		SourceWarning: sourceWarningPtr(s.SourceWarning),
		Collector:     status,
	}
}

// matchAll applies every active filter to one node.
func matchAll(node domain.Node, s domain.Snapshot, q *resultsQuery) bool {
	if q.Search != "" {
		found := false
		for _, v := range domain.SearchValues(node) {
			if strings.Contains(v, q.Search) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(q.Countries) > 0 && !containsString(q.Countries, orUnknown(node.Country)) {
		return false
	}
	if len(q.Types) > 0 && !containsString(q.Types, orUnknown(node.Type)) {
		return false
	}
	if len(q.Networks) > 0 && !containsString(q.Networks, orUnknown(node.Network)) {
		return false
	}
	for _, feature := range q.Features {
		switch feature {
		case "tls":
			if !node.TLS {
				return false
			}
		case "reality":
			if !node.Reality {
				return false
			}
		case "udp":
			if !node.UDP {
				return false
			}
		}
	}
	if len(q.IPSources) > 0 {
		if node.IPPure == nil || node.IPPure.Status != "success" ||
			!containsString(q.IPSources, node.IPPure.Source) {
			return false
		}
	}
	if len(q.IPAttributes) > 0 {
		if node.IPPure == nil || node.IPPure.Status != "success" {
			return false
		}
		found := false
		for _, value := range node.IPPure.Attributes {
			if containsString(q.IPAttributes, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if s.SpeedTest && node.Speed < q.MinSpeed*1024 {
		return false
	}
	if s.MediaCheck {
		for _, platform := range q.Media {
			if !domain.HasMedia(node, platform) {
				return false
			}
		}
	}
	return true
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// resultsQuery is the parsed query string.
type resultsQuery struct {
	Page         int
	Limit        int
	Search       string
	Countries    []string
	Types        []string
	Networks     []string
	Features     []string
	IPSources    []string
	IPAttributes []string
	Media        []string
	MinSpeed     float64
	Sort         string
	Direction    string
}

// errInvalidQuery marks a malformed query string.
var errInvalidQuery = errors.New("invalid results query")

// parseResultsQuery mirrors the JavaScript validation: absent pagination means
// "no query", and every other parameter must be a string.
func parseResultsQuery(r *http.Request) (*resultsQuery, error) {
	values := r.URL.Query()
	if !values.Has("page") && !values.Has("limit") {
		return nil, nil
	}
	page, err := positiveInt(values.Get("page"), 1)
	if err != nil {
		return nil, errInvalidQuery
	}
	limit, err := positiveInt(values.Get("limit"), 50)
	if err != nil || limit > 200 {
		return nil, errInvalidQuery
	}
	for _, key := range []string{
		"q", "country", "type", "network", "features", "ipSource",
		"ipAttributes", "media", "minSpeed", "sort", "direction",
	} {
		if values.Has(key) && values.Get(key) == "" {
			// An explicit empty value is accepted and means "no filter".
			continue
		}
	}
	sortKey := values.Get("sort")
	if sortKey != "" && !containsString(domain.ResultSortKeys, sortKey) {
		return nil, errInvalidQuery
	}
	direction := values.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		return nil, errInvalidQuery
	}
	minSpeed := 0.0
	if raw := values.Get("minSpeed"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed < 0 {
			return nil, errInvalidQuery
		}
		minSpeed = parsed
	}
	features := splitList(values.Get("features"))
	for _, feature := range features {
		if feature != "tls" && feature != "reality" && feature != "udp" {
			return nil, errInvalidQuery
		}
	}
	return &resultsQuery{
		Page:         page,
		Limit:        limit,
		Search:       strings.ToLower(strings.TrimSpace(values.Get("q"))),
		Countries:    splitList(values.Get("country")),
		Types:        splitList(values.Get("type")),
		Networks:     splitList(values.Get("network")),
		Features:     features,
		IPSources:    splitList(values.Get("ipSource")),
		IPAttributes: splitList(values.Get("ipAttributes")),
		Media:        splitList(values.Get("media")),
		MinSpeed:     minSpeed,
		Sort:         sortKey,
		Direction:    direction,
	}, nil
}

func splitList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func positiveInt(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		return 0, errInvalidQuery
	}
	return parsed, nil
}

func parsePaging(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "历史分页参数无效。", "")
			return 0, 0, false
		}
		limit = parsed
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "历史分页参数无效。", "")
			return 0, 0, false
		}
		offset = parsed
	}
	return limit, offset, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	body := map[string]any{"error": message}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}

func writeUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable,
		"检测结果暂时无法读取，请稍后刷新重试。", "READ_FAILED")
}

// Serve runs the HTTP server until the context is cancelled.
func Serve(ctx context.Context, handler http.Handler, addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdown <- server.Shutdown(stopCtx)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-shutdown
}
