// End-to-end coverage for the HTTP contract. The JavaScript service defined the
// response shapes that the frontend and the deployment tooling rely on, so these
// tests assert the exact JSON key sets rather than selected fields: an extra key
// (a leaked projection marker, an internal id) is as much a regression as a
// missing one.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	sqlitestore "github.com/cytusc/node_list/backend/internal/store/sqlite"
)

// fixture is a running handler over a real SQLite store.
type fixture struct {
	t       *testing.T
	handler http.Handler
	store   *store.Store
}

// newFixture builds the store the same way main does, minus the schedulers, and
// inserts the given batches in order.
func newFixture(t *testing.T, batches ...domain.Snapshot) *fixture {
	t.Helper()
	return newFixtureSeeded(t, nil, batches...)
}

// newFixtureSeeded is newFixture with a hook to write the side tables
// (enrichments, retirements) between insertion and the store load.
func newFixtureSeeded(t *testing.T, seed func(context.Context, *sqlitestore.Adapter) error, batches ...domain.Snapshot) *fixture {
	t.Helper()
	ctx := context.Background()
	// A file-backed database, not ":memory:": the adapter pools connections and
	// each SQLite :memory: connection would get its own private database.
	adapter, err := sqlitestore.New(filepath.Join(t.TempDir(), "nodes.sqlite"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	for i, batch := range batches {
		payload, err := json.Marshal(batch)
		if err != nil {
			t.Fatalf("marshal batch %d: %v", i, err)
		}
		if err := adapter.Insert(ctx, fingerprint(i), payload, batch.CheckedAt, len(batch.Nodes)); err != nil {
			t.Fatalf("insert batch %d: %v", i, err)
		}
	}
	if seed != nil {
		if err := seed(ctx, adapter); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	st := store.New(adapter, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatalf("store.Reload: %v", err)
	}
	server := New(ServerConfig{Store: st})
	return &fixture{t: t, handler: server.Handler(), store: st}
}

func fingerprint(i int) string { return "batch-" + string(rune('a'+i)) }

// get performs one request and returns the status, the decoded object and the
// response headers.
func (f *fixture) get(path string) (int, map[string]json.RawMessage, http.Header) {
	f.t.Helper()
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	body := recorder.Body.Bytes()
	var decoded map[string]json.RawMessage
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			f.t.Fatalf("GET %s returned non-object JSON %q: %v", path, body, err)
		}
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		f.t.Fatalf("GET %s Content-Type = %q", path, got)
	}
	return recorder.Code, decoded, recorder.Header()
}

// keys returns the sorted JSON keys, so an unexpected key fails the test.
func keys(m map[string]json.RawMessage) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (f *fixture) requireKeys(path, want string, obj map[string]json.RawMessage) {
	f.t.Helper()
	if got := keys(obj); got != want {
		f.t.Fatalf("GET %s keys:\n got %s\nwant %s", path, got, want)
	}
}

func (f *fixture) object(path string, raw json.RawMessage) map[string]json.RawMessage {
	f.t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatalf("GET %s: decode nested object %q: %v", path, raw, err)
	}
	return out
}

func (f *fixture) array(path string, raw json.RawMessage) []json.RawMessage {
	f.t.Helper()
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatalf("GET %s: decode nested array %q: %v", path, raw, err)
	}
	return out
}

func (f *fixture) string(path string, raw json.RawMessage) string {
	f.t.Helper()
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatalf("GET %s: decode string %q: %v", path, raw, err)
	}
	return out
}

func (f *fixture) number(path string, raw json.RawMessage) float64 {
	f.t.Helper()
	var out float64
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatalf("GET %s: decode number %q: %v", path, raw, err)
	}
	return out
}

// batchOne is a two-node batch with a successful IPPure enrichment, enough to
// exercise facets, filters, media and the aggregate attachment.
func batchOne() domain.Snapshot {
	human := 91.5
	return domain.Snapshot{
		CheckedAt:  "2026-10-14T18:00:49.000Z",
		SpeedTest:  true,
		MediaCheck: true,
		Nodes: []domain.Node{
			{
				Name: "US-01", Type: "ss", Server: "198.51.100.7", Port: "8443",
				TLS: true, UDP: true, Network: "tcp", Speed: 8192, Country: "US",
				RawLink: "ss://YWVzLTI1Ni1nY206c2VjcmV0@198.51.100.7:8443#US-01",
				Media:   []domain.Media{mustMedia(`{"platform":"netflix","tag":"US"}`)},
				IPPure: &domain.IPPureResult{
					CheckedAt: "2026-10-14T18:01:00.000Z", Attempts: 1,
					Status: "success", IP: "198.51.100.7", Source: "ippure",
					Attributes: []string{"residential"}, Country: "US", HumanPercent: &human,
				},
			},
			{
				Name: "JP-02", Type: "vless", Server: "203.0.113.9", Port: "443",
				TLS: true, UDP: false, Network: "ws", Speed: 2048, Country: "JP",
				RawConfig: domain.ProxyConfig{"type": "vless", "server": "203.0.113.9", "port": 443},
			},
		},
	}
}

// batchTwo is a later, smaller batch so the aggregate view has to fold two
// generations and the history list has more than one row.
func batchTwo() domain.Snapshot {
	return domain.Snapshot{
		CheckedAt: "2026-10-14T19:30:00.000Z",
		Nodes: []domain.Node{
			{
				Name: "DE-03", Type: "trojan", Server: "192.0.2.11", Port: "2087",
				TLS: true, UDP: true, Network: "grpc", Speed: 4096, Country: "DE",
			},
		},
	}
}

func mustMedia(raw string) domain.Media {
	var m domain.Media
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic(err)
	}
	return m
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	status, body, _ := f.get("/api/health")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	f.requireKeys("/api/health", "status", body)
	if got := f.string("/api/health", body["status"]); got != "ok" {
		t.Fatalf("status = %q", got)
	}
}

// An empty store must produce the same actionable 404 the JavaScript service
// returned, not an empty page.
func TestResultsEmptyStore(t *testing.T) {
	f := newFixture(t)
	status, body, _ := f.get("/api/results")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	f.requireKeys("/api/results", "code,error", body)
	if got := f.string("/api/results", body["code"]); got != "NO_RESULTS" {
		t.Fatalf("code = %q", got)
	}
}

// The unpaginated view is the newest batch verbatim: it carries the batch id and
// a null sourceWarning, and the nodes must not gain the aggregate-only fields.
func TestResultsUnpaginatedShape(t *testing.T) {
	f := newFixture(t, batchOne(), batchTwo())
	status, body, header := f.get("/api/results")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", header.Get("Cache-Control"))
	}
	// batchCount is absent because this is a single batch, not a folded view.
	f.requireKeys("/api/results",
		"checkedAt,id,ippureCollector,mediaCheck,nodes,sourceWarning,speedTest", body)
	if string(body["sourceWarning"]) != "null" {
		t.Fatalf("sourceWarning = %s, want null", body["sourceWarning"])
	}
	if got := f.number("/api/results", body["id"]); got != 2 {
		t.Fatalf("id = %v, want the newest batch 2", got)
	}
	nodes := f.array("/api/results", body["nodes"])
	if len(nodes) != 1 {
		t.Fatalf("newest batch has %d nodes, want 1", len(nodes))
	}
	node := f.object("/api/results", nodes[0])
	if got := keys(node); strings.Contains(got, "snapshotId") ||
		strings.Contains(got, "speedTest") || strings.Contains(got, "mediaCheck") ||
		strings.Contains(got, "checkedAt") {
		t.Fatalf("single-batch node leaked aggregate fields: %s", got)
	}
	if got := f.string("/api/results", node["name"]); got != "DE-03" {
		t.Fatalf("node name = %q", got)
	}
	collector := f.object("/api/results", body["ippureCollector"])
	f.requireKeys("/api/results ippureCollector", "enabled", collector)
}

// Credentials must never reach the browser.
func TestResultsStripsCredentials(t *testing.T) {
	f := newFixture(t, batchOne())
	_, body, _ := f.get("/api/results")
	nodes := f.array("/api/results", body["nodes"])
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}
	for i, raw := range nodes {
		node := f.object("/api/results", raw)
		if got, ok := node["rawLink"]; ok && string(got) != `""` && string(got) != "null" {
			t.Fatalf("node[%d] leaked rawLink: %s", i, got)
		}
		if got, ok := node["rawConfig"]; ok && string(got) != "null" && string(got) != "{}" {
			t.Fatalf("node[%d] leaked rawConfig: %s", i, got)
		}
	}
}

// The paginated view carries the facet counts and the page window.
func TestResultsPaginatedShape(t *testing.T) {
	f := newFixture(t, batchOne())
	status, body, _ := f.get("/api/results?page=1&limit=1")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	f.requireKeys("/api/results?page=1&limit=1",
		"checkedAt,facets,id,ippureCollector,limit,mediaCheck,nodes,page,sourceWarning,speedTest,summary,total,totalNodes",
		body)
	if got := f.number("/api/results?page=1&limit=1", body["total"]); got != 2 {
		t.Fatalf("total = %v, want 2", got)
	}
	if got := f.number("/api/results?page=1&limit=1", body["totalNodes"]); got != 2 {
		t.Fatalf("totalNodes = %v, want 2", got)
	}
	if got := f.number("/api/results?page=1&limit=1", body["limit"]); got != 1 {
		t.Fatalf("limit = %v", got)
	}
	if got := f.number("/api/results?page=1&limit=1", body["page"]); got != 1 {
		t.Fatalf("page = %v", got)
	}
	if got := len(f.array("/api/results?page=1&limit=1", body["nodes"])); got != 1 {
		t.Fatalf("page holds %d nodes, want 1", got)
	}
	facets := f.object("/api/results?page=1&limit=1", body["facets"])
	f.requireKeys("/api/results?page=1&limit=1 facets",
		"country,features,ipAttributes,ipSource,media,network,type", facets)
	countries := f.array("/api/results?page=1&limit=1 facets.country", facets["country"])
	for _, raw := range countries {
		facet := f.object("/api/results?page=1&limit=1 facets.country", raw)
		if keys(facet) != "count,label,value" {
			t.Fatalf("facet keys = %s", keys(facet))
		}
	}
	// Two nodes, each with one country, ordered by count then first-seen.
	if len(countries) != 2 {
		t.Fatalf("country facets = %d, want 2", len(countries))
	}
}

// The aggregate view spans every batch: no snapshot id, and every node records
// the batch it came from.
func TestResultsAggregateShape(t *testing.T) {
	f := newFixture(t, batchOne(), batchTwo())
	status, body, _ := f.get("/api/results?view=aggregate")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	f.requireKeys("/api/results?view=aggregate",
		"batchCount,checkedAt,ippureCollector,mediaCheck,nodes,sourceWarning,speedTest", body)
	if got := f.number("/api/results?view=aggregate", body["batchCount"]); got != 2 {
		t.Fatalf("batchCount = %v, want 2", got)
	}
	nodes := f.array("/api/results?view=aggregate", body["nodes"])
	if len(nodes) != 3 {
		t.Fatalf("aggregate holds %d nodes, want 3", len(nodes))
	}
	seen := map[float64]bool{}
	for _, raw := range nodes {
		node := f.object("/api/results?view=aggregate", raw)
		if _, ok := node["snapshotId"]; !ok {
			t.Fatalf("aggregate node is missing snapshotId: %s", keys(node))
		}
		if _, ok := node["speedTest"]; !ok {
			t.Fatalf("aggregate node is missing speedTest: %s", keys(node))
		}
		if _, ok := node["mediaCheck"]; !ok {
			t.Fatalf("aggregate node is missing mediaCheck: %s", keys(node))
		}
		if _, ok := node["checkedAt"]; !ok {
			t.Fatalf("aggregate node is missing checkedAt: %s", keys(node))
		}
		seen[f.number("/api/results?view=aggregate", node["snapshotId"])] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("aggregate did not fold both batches: %v", seen)
	}
}

// Every rejected parameter must be a 400 with the user-facing message, matching
// the JavaScript validation. The filter and sort parameters are only validated
// once pagination is in play, so those cases carry page=1.
func TestResultsRejectsBadQueries(t *testing.T) {
	f := newFixture(t, batchOne(), batchTwo())
	const invalid = "节点分页、筛选或排序参数无效。"
	cases := []struct {
		path    string
		message string
	}{
		{"/api/results?page=0", invalid},
		{"/api/results?page=-1", invalid},
		{"/api/results?limit=0", invalid},
		{"/api/results?limit=999", invalid},
		{"/api/results?page=1&sort=bogus", invalid},
		{"/api/results?page=1&direction=sideways", invalid},
		{"/api/results?page=1&features=bogus", invalid},
		{"/api/results?page=1&minSpeed=-1", invalid},
		{"/api/results?page=1&minSpeed=abc", invalid},
		{"/api/results?view=bogus", "检测结果视图无效。"},
		{"/api/results?view=aggregate&snapshot=1", "检测结果视图无效。"},
		{"/api/results?snapshot=abc", "检测批次无效。"},
		{"/api/results?snapshot=0", "检测批次无效。"},
		{"/api/results?snapshot=01", "检测批次无效。"},
		{"/api/results?snapshot=-3", "检测批次无效。"},
	}
	for _, tc := range cases {
		status, body, _ := f.get(tc.path)
		if status != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want 400", tc.path, status)
		}
		f.requireKeys(tc.path, "error", body)
		if got := f.string(tc.path, body["error"]); got != tc.message {
			t.Fatalf("GET %s: error = %q, want %q", tc.path, got, tc.message)
		}
	}
}

// Without page or limit the JavaScript parser returns null before validating
// anything, so a filter is ignored and the unpaginated snapshot is served. The
// quirk is observable from the frontend, so it is pinned here.
func TestResultsIgnoresFiltersWithoutPagination(t *testing.T) {
	f := newFixture(t, batchOne())
	for _, path := range []string{
		"/api/results?sort=bogus",
		"/api/results?direction=sideways",
		"/api/results?features=bogus",
		"/api/results?minSpeed=-1",
		"/api/results?country=US",
	} {
		status, body, _ := f.get(path)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", path, status)
		}
		if _, ok := body["nodes"]; !ok {
			t.Fatalf("GET %s: expected the unpaginated snapshot, got %s", path, keys(body))
		}
		if _, ok := body["facets"]; ok {
			t.Fatalf("GET %s: unpaginated view must not carry facets", path)
		}
	}
}

// A batch id that is well formed but absent is a 404 with its own code.
func TestResultsMissingSnapshot(t *testing.T) {
	f := newFixture(t, batchOne())
	status, body, _ := f.get("/api/results?snapshot=99999")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	f.requireKeys("/api/results?snapshot=99999", "code,error", body)
	if got := f.string("/api/results?snapshot=99999", body["code"]); got != "SNAPSHOT_NOT_FOUND" {
		t.Fatalf("code = %q", got)
	}
}

// Filters narrow the set, and the unknown bucket is selectable. Speeds are
// kilobytes per second: minSpeed is scaled by 1024 before comparing.
func TestResultsFilters(t *testing.T) {
	snapshot := batchOne()
	snapshot.Nodes = append(snapshot.Nodes, domain.Node{
		Name: "XX-04", Type: "ss", Server: "192.0.2.44", Port: "80", Speed: 512,
	})
	f := newFixture(t, snapshot)

	cases := []struct {
		path  string
		total float64
	}{
		{"/api/results?page=1&limit=50", 3},
		{"/api/results?page=1&limit=50&country=US", 1},
		{"/api/results?page=1&limit=50&country=unknown", 1},
		{"/api/results?page=1&limit=50&type=vless", 1},
		{"/api/results?page=1&limit=50&network=ws", 1},
		{"/api/results?page=1&limit=50&features=tls,udp", 1},
		{"/api/results?page=1&limit=50&features=tls,udp,reality", 0},
		{"/api/results?page=1&limit=50&q=jp", 1},
		{"/api/results?page=1&limit=50&minSpeed=5", 1},
		{"/api/results?page=1&limit=50&ipSource=ippure", 1},
		{"/api/results?page=1&limit=50&ipAttributes=residential", 1},
		{"/api/results?page=1&limit=50&media=netflix", 1},
		{"/api/results?page=1&limit=50&media=hulu", 0},
	}
	for _, tc := range cases {
		status, body, _ := f.get(tc.path)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want 200", tc.path, status)
		}
		if got := f.number(tc.path, body["total"]); got != tc.total {
			t.Fatalf("GET %s: total = %v, want %v", tc.path, got, tc.total)
		}
	}
}

// An unknown /api path keeps the JSON 404 body rather than hitting the file
// server, which is what the frontend probes rely on.
func TestUnknownAPIPaths(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/nope", "/api/downloads", "/api/download"} {
		status, body, _ := f.get(path)
		if status != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404", path, status)
		}
		if _, ok := body["error"]; !ok {
			t.Fatalf("GET %s: body = %v", path, body)
		}
	}
}

// History is newest first and reports the batch sizes.
func TestHistory(t *testing.T) {
	f := newFixture(t, batchOne(), batchTwo())
	status, body, header := f.get("/api/history")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", header.Get("Cache-Control"))
	}
	f.requireKeys("/api/history", "items,total", body)
	if got := f.number("/api/history", body["total"]); got != 2 {
		t.Fatalf("total = %v, want 2", got)
	}
	items := f.array("/api/history", body["items"])
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	first := f.object("/api/history", items[0])
	f.requireKeys("/api/history item", "checkedAt,id,importedAt,nodeCount", first)
	if got := f.number("/api/history", first["id"]); got != 2 {
		t.Fatalf("newest history id = %v, want 2", got)
	}
	if got := f.number("/api/history", first["nodeCount"]); got != 1 {
		t.Fatalf("newest nodeCount = %v, want 1", got)
	}
	if got := f.string("/api/history", first["importedAt"]); !strings.HasSuffix(got, "Z") || len(got) != 24 {
		t.Fatalf("importedAt = %q, want the JavaScript ISO shape", got)
	}
}

func TestHistoryRejectsBadPaging(t *testing.T) {
	f := newFixture(t, batchOne())
	for _, path := range []string{"/api/history?limit=0", "/api/history?limit=201", "/api/history?offset=-1"} {
		status, body, _ := f.get(path)
		if status != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, want 400", path, status)
		}
		if got := f.string(path, body["error"]); got != "历史分页参数无效。" {
			t.Fatalf("GET %s: error = %q", path, got)
		}
	}
}

// The source warning becomes a string as soon as ingestion fails, and null again
// once it recovers.
func TestSourceWarningRoundTrip(t *testing.T) {
	f := newFixture(t, batchOne())
	f.store.SetSourceWarning("READ_FAILED")
	_, body, _ := f.get("/api/results")
	if got := f.string("/api/results", body["sourceWarning"]); got != "READ_FAILED" {
		t.Fatalf("sourceWarning = %q", got)
	}
	f.store.SetSourceWarning("")
	_, body, _ = f.get("/api/results")
	if string(body["sourceWarning"]) != "null" {
		t.Fatalf("sourceWarning = %s, want null", body["sourceWarning"])
	}
}

// A retired connection disappears from the single-batch view. The identity must
// be derived from the stored node: the public payload has its credentials
// stripped, and hashing that would produce a different, non-matching identity.
func TestRetiredNodesHidden(t *testing.T) {
	ctx := context.Background()
	batch := batchOne()
	f := newFixture(t, batch)
	_, body, _ := f.get("/api/results")
	before := len(f.array("/api/results", body["nodes"]))
	if before != 2 {
		t.Fatalf("fixture served %d nodes, want 2", before)
	}

	identity := domain.AggregateIdentity(batch.Nodes[0])
	if err := f.store.Adapter().RetireNodes(ctx, []string{identity},
		"2026-10-14T20:00:00.000Z", "test"); err != nil {
		t.Fatalf("RetireNodes: %v", err)
	}
	if err := f.store.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	_, body, _ = f.get("/api/results")
	if after := len(f.array("/api/results", body["nodes"])); after != before-1 {
		t.Fatalf("retiring one node left %d of %d visible", after, before)
	}
}

// The aggregate view must classify every node by its verified exit region, the
// same way the single-batch view does. Source-claimed regions regularly
// disagree with the exit IP, so skipping the classification silently shows the
// wrong country and drops the claimed-versus-actual badge. The aggregate
// bypasses hydrate, which is where the single-batch path applies it.
func TestAggregateAppliesVerifiedExitCountry(t *testing.T) {
	batch := domain.Snapshot{
		CheckedAt: "2026-10-14T18:00:49.000Z",
		Nodes: []domain.Node{
			{Name: "DE-01", Type: "ss", Server: "192.0.2.1", Port: "443", Country: "DE", Speed: 1024},
			{Name: "JP-02", Type: "ss", Server: "192.0.2.2", Port: "443", Country: "JP", Speed: 1024},
			{Name: "ZZ-03", Type: "ss", Server: "192.0.2.3", Port: "443", Speed: 1024},
		},
	}
	exits := map[int]string{0: "US", 2: "BR"}
	f := newFixtureSeeded(t, func(ctx context.Context, adapter *sqlitestore.Adapter) error {
		for index, country := range exits {
			payload, err := json.Marshal(domain.IPPureResult{
				CheckedAt: "2026-10-14T18:01:00.000Z", Attempts: 1,
				Status: "success", Country: country,
			})
			if err != nil {
				return err
			}
			if err := adapter.SaveIPPure(ctx, 1, index, payload,
				domain.AggregateIdentity(batch.Nodes[index])); err != nil {
				return err
			}
		}
		return nil
	}, batch)

	for _, path := range []string{"/api/results?view=aggregate&page=1&limit=50", "/api/results"} {
		_, body, _ := f.get(path)
		byName := map[string]map[string]json.RawMessage{}
		for _, raw := range f.array(path, body["nodes"]) {
			node := f.object(path, raw)
			byName[f.string(path, node["name"])] = node
		}
		if len(byName) != 3 {
			t.Fatalf("GET %s: served %d nodes, want 3", path, len(byName))
		}
		// The claimed region is preserved next to the verified one.
		mismatch := byName["DE-01"]
		if got := f.string(path, mismatch["country"]); got != "US" {
			t.Fatalf("GET %s: DE-01 country = %q, want the verified US", path, got)
		}
		if got := f.string(path, mismatch["claimedCountry"]); got != "DE" {
			t.Fatalf("GET %s: DE-01 claimedCountry = %q, want DE", path, got)
		}
		// A matching claim keeps its country and gains no badge.
		same := byName["JP-02"]
		if got := f.string(path, same["country"]); got != "JP" {
			t.Fatalf("GET %s: JP-02 country = %q, want JP", path, got)
		}
		if _, ok := same["claimedCountry"]; ok {
			t.Fatalf("GET %s: JP-02 gained a claimedCountry badge", path)
		}
		// An absent claim is filled in from the exit IP.
		absent := byName["ZZ-03"]
		if got := f.string(path, absent["country"]); got != "BR" {
			t.Fatalf("GET %s: ZZ-03 country = %q, want BR", path, got)
		}
		if _, ok := absent["claimedCountry"]; ok {
			t.Fatalf("GET %s: ZZ-03 gained a claimedCountry badge", path)
		}
	}
}
