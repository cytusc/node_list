// Read-through coverage. The API process must serve a PostgreSQL deployment
// without keeping the decoded batch set resident, so these tests pin two
// things: the views it answers are byte-identical to the ones the cached store
// produced, and the store it feeds the API really does retain nothing.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	sqlitestore "github.com/cytusc/node_list/backend/internal/store/sqlite"
)

// serveBody runs one request and returns the status and the raw body, so two
// handlers can be compared byte for byte.
func serveBody(handler http.Handler, path string) (int, string) {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code, recorder.Body.String()
}

// newReadThroughPair builds one database behind two handlers: the cached store
// the API used to construct, and the read-through store it constructs now.
func newReadThroughPair(t *testing.T) (http.Handler, http.Handler, *store.Store) {
	t.Helper()
	ctx := context.Background()
	adapter, err := sqlitestore.New(filepath.Join(t.TempDir(), "nodes.sqlite"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	batches := []domain.Snapshot{batchOne(), batchTwo()}
	for i, batch := range batches {
		payload, err := json.Marshal(batch)
		if err != nil {
			t.Fatalf("marshal batch %d: %v", i, err)
		}
		if err := adapter.Insert(ctx, fingerprint(i), payload, batch.CheckedAt, len(batch.Nodes)); err != nil {
			t.Fatalf("insert batch %d: %v", i, err)
		}
	}
	// An enrichment on the second node of batch 1 is exactly what hydrate
	// attaches, so the comparison covers the enrichment path too.
	enrichment := []byte(`{"status":"success","country":"JP","ip":"203.0.113.9"}`)
	if err := adapter.SaveIPPure(ctx, 1, 1, enrichment, domain.AggregateIdentity(batches[0].Nodes[1])); err != nil {
		t.Fatalf("save ippure: %v", err)
	}
	cached := store.New(adapter, nil)
	if err := cached.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	live := store.NewReadThrough(adapter)
	// The API loop calls Refresh; a read-through store must not refill a cache.
	live.SetRefresh(func(context.Context) error { return nil })
	if err := live.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// The source warning travels on the store, not on the removed cache.
	cached.SetSourceWarning(store.SourceWarningReadFailed)
	live.SetSourceWarning(store.SourceWarningReadFailed)
	return New(ServerConfig{Store: cached}).Handler(), New(ServerConfig{Store: live}).Handler(), live
}

// TestReadThroughMatchesCachedViews is the regression fence for the change: every
// view a read-through API can answer must be byte-identical to the cached one.
func TestReadThroughMatchesCachedViews(t *testing.T) {
	cachedHandler, liveHandler, _ := newReadThroughPair(t)
	for _, path := range []string{
		"/api/health",
		"/api/history",
		"/api/history?limit=1&offset=1",
		"/api/results",
		"/api/results?snapshot=1",
		"/api/results?snapshot=2",
		"/api/results?snapshot=99",
		"/api/results?snapshot=abc",
		"/api/results?page=1&limit=50",
		"/api/results?page=1&limit=1&q=us",
		"/api/results?page=2&limit=1",
		"/api/results?snapshot=1&page=1&limit=1",
		"/api/results?page=0",
		"/api/results?view=bogus",
	} {
		wantStatus, wantBody := serveBody(cachedHandler, path)
		gotStatus, gotBody := serveBody(liveHandler, path)
		if gotStatus != wantStatus || gotBody != wantBody {
			t.Fatalf("%s\n cached: %d %s\n   live: %d %s", path, wantStatus, wantBody, gotStatus, gotBody)
		}
	}
}

// TestReadThroughRetainsNoBatchSet proves the retention is gone while the same
// database still answers: the batches are there, the store is empty.
func TestReadThroughRetainsNoBatchSet(t *testing.T) {
	_, liveHandler, live := newReadThroughPair(t)
	ctx := context.Background()
	if row, err := live.Adapter().Get(ctx, nil); err != nil || row == nil {
		t.Fatalf("fixture database has no batch: row=%v err=%v", row, err)
	}
	status, body := serveBody(liveHandler, "/api/results")
	if status != http.StatusOK {
		t.Fatalf("live /api/results = %d %s", status, body)
	}
	if got := live.LatestSnapshotID(); got != 0 {
		t.Fatalf("read-through store kept batches: LatestSnapshotID = %d", got)
	}
	if snap, err := live.Get(nil); err != nil || snap != nil {
		t.Fatalf("read-through store cached a batch: snap=%v err=%v", snap, err)
	}
	if snap := live.Aggregate(false); snap != nil {
		t.Fatal("read-through store folded an aggregate")
	}
	if got := len(live.RetiredNodeIdentities()); got != 0 {
		t.Fatalf("read-through store kept %d retirements", got)
	}
	// Reload and Refresh are the two calls that used to fill the cache.
	if err := live.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := live.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := live.LatestSnapshotID(); got != 0 {
		t.Fatalf("Reload/Refresh refilled the cache: LatestSnapshotID = %d", got)
	}
	if snap := live.Aggregate(false); snap != nil {
		t.Fatal("Reload/Refresh refilled the aggregate")
	}
}

// TestReadThroughHidesRetiredNodes: hiding retired connections is a read-path
// rule, not a cache entry. The newest-batch view filters them, the explicit
// snapshot view keeps them, exactly as the cached store did.
func TestReadThroughHidesRetiredNodes(t *testing.T) {
	ctx := context.Background()
	adapter, err := sqlitestore.New(filepath.Join(t.TempDir(), "nodes.sqlite"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	batch := batchOne()
	payload, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}
	if err := adapter.Insert(ctx, fingerprint(0), payload, batch.CheckedAt, len(batch.Nodes)); err != nil {
		t.Fatalf("insert batch: %v", err)
	}
	if err := adapter.RetireNodes(ctx, []string{domain.AggregateIdentity(batch.Nodes[0])}, "2026-10-14T18:02:00.000Z", "test"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	live := store.NewReadThrough(adapter)
	handler := New(ServerConfig{Store: live}).Handler()

	status, body := serveBody(handler, "/api/results")
	if status != http.StatusOK {
		t.Fatalf("GET /api/results = %d %s", status, body)
	}
	var newest struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &newest); err != nil {
		t.Fatalf("decode newest batch: %v", err)
	}
	if len(newest.Nodes) != 1 || newest.Nodes[0].Name != "JP-02" {
		t.Fatalf("retired node still served: %+v", newest.Nodes)
	}

	status, body = serveBody(handler, "/api/results?snapshot=1")
	if status != http.StatusOK {
		t.Fatalf("GET /api/results?snapshot=1 = %d %s", status, body)
	}
	var explicit struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &explicit); err != nil {
		t.Fatalf("decode snapshot batch: %v", err)
	}
	if len(explicit.Nodes) != 2 {
		t.Fatalf("explicit snapshot view holds %d nodes, want the batch verbatim: 2", len(explicit.Nodes))
	}
}
