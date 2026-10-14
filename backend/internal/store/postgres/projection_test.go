package postgres

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// The ranking marker steers one-off projection rebuilds; it must never reach the
// API response, which embeds domain.Metadata directly.
func TestStoredMetadataKeepsRankingOutOfTheAPIShape(t *testing.T) {
	original := domain.Metadata{
		CheckedAt:  "2026-10-14T18:00:49.000Z",
		BatchCount: 3,
		SpeedTest:  true,
		MediaCheck: true,
		TotalNodes: 904,
		Summary:    domain.Summary{Countries: 42, MaxSpeed: 88.5, Median: 12.25},
		Facets: domain.Facets{
			Country:      []domain.Facet{{Value: "US", Label: "美国", Count: 120}},
			Type:         []domain.Facet{{Value: "ss", Label: "ss", Count: 500}},
			Network:      []domain.Facet{{Value: "tcp", Label: "tcp", Count: 900}},
			IPSource:     []domain.Facet{{Value: "ippure", Label: "IPPure", Count: 30}},
			IPAttributes: []domain.Facet{{Value: "residential", Label: "住宅", Count: 12}},
			Features:     []domain.Facet{{Value: "tls", Label: "TLS", Count: 44}},
			Media:        []domain.Facet{{Value: "netflix", Label: "Netflix", Count: 7}},
		},
	}
	raw, err := marshalStoredMetadata(original)
	if err != nil {
		t.Fatalf("marshalStoredMetadata: %v", err)
	}
	if !strings.Contains(string(raw), `"ranking":"`+rankingRevision+`"`) {
		t.Fatalf("persisted metadata is missing the ranking marker: %s", raw)
	}
	if strings.Contains(string(domainMetadataJSON(t, original)), "ranking") {
		t.Fatal("ranking leaked into the API metadata shape")
	}

	restored, err := unmarshalStoredMetadata(raw)
	if err != nil {
		t.Fatalf("unmarshalStoredMetadata: %v", err)
	}
	// Metadata holds slices (Facets), so DeepEqual rather than ==.
	if !reflect.DeepEqual(restored, original) {
		t.Fatalf("round trip lost fields:\n got %+v\nwant %+v", restored, original)
	}
}

// The marker is additive: every key the API expects must survive the wrapper, so
// a future field rename cannot silently drop out of the projection header.
func TestStoredMetadataIsAdditiveOverTheAPIKeys(t *testing.T) {
	original := domain.Metadata{
		CheckedAt:  "2026-10-14T18:00:49.000Z",
		BatchCount: 2,
		TotalNodes: 10,
	}
	raw, err := marshalStoredMetadata(original)
	if err != nil {
		t.Fatalf("marshalStoredMetadata: %v", err)
	}
	var stored, apiShape map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	if err := json.Unmarshal(domainMetadataJSON(t, original), &apiShape); err != nil {
		t.Fatalf("unmarshal api shape: %v", err)
	}
	if len(stored) != len(apiShape)+1 {
		t.Fatalf("stored keys %v should be the API keys plus ranking, got %v", keys(apiShape), keys(stored))
	}
	for key, want := range apiShape {
		got, ok := stored[key]
		if !ok {
			t.Fatalf("stored metadata dropped API key %q", key)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("key %q changed: got %v want %v", key, got, want)
		}
	}
}

// Rows written before this change carry no marker, so the comparison must treat
// a missing or null metadata column as "needs rebuild".
func TestRankingMarkerMissingOnLegacyRows(t *testing.T) {
	for _, raw := range []string{"null", "{}", `{"checkedAt":"x"}`} {
		var decoded struct {
			Ranking string `json:"ranking"`
		}
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if decoded.Ranking == rankingRevision {
			t.Fatalf("%s should not report the current ranking revision", raw)
		}
		// The SQL guard compares with coalesce(...,''), so the Go side must
		// agree that a blank marker is stale.
		if decoded.Ranking != "" {
			t.Fatalf("%s decoded to %q, want the zero value", raw, decoded.Ranking)
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func domainMetadataJSON(t *testing.T, m domain.Metadata) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return raw
}
