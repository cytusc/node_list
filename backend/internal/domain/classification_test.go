package domain

import (
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"reflect"
	"testing"
)

func TestStoredZeroResidentialTieIsReclassifiedWithoutRefreshingEvidence(t *testing.T) {
	raw := []byte(`{"status":"success","ip":"104.28.159.17","checkedAt":"2026-10-15T09:00:03Z","attributes":["住宅IP"],"classification":{"version":7,"ip":"104.28.159.17","checkedAt":"2026-10-15T09:00:15Z","state":"residential","types":[{"provider":"ippure","usageType":"商业宽带"},{"provider":"ipregistry","usageType":"机房IP","companyType":"机房IP"}]}}`)
	var r IPPureResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.Classification.State != "unknown" || !reflect.DeepEqual(r.Attributes, []string{"商业宽带"}) {
		t.Fatalf("old residential label retained: %+v", r)
	}
	if r.Classification.Version != 7 || r.CheckedAt != "2026-10-15T09:00:03Z" || r.Classification.CheckedAt != "2026-10-15T09:00:15Z" || len(r.Classification.Types) != 2 {
		t.Fatalf("source evidence or timestamps changed: %+v", r)
	}
}

func TestClassificationSurvivesRiskFailureAndJSON(t *testing.T) {
	n := Node{IPPure: &IPPureResult{Status: "error", Attributes: []string{"住宅IP"}, Classification: &ipcheck.Result{Version: ipcheck.VotingVersion, State: "residential", ResidentialVotes: 2, UnknownVotes: 1}}}
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var restored Node
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.IPPure.Classification == nil || restored.IPPure.Classification.State != "residential" {
		t.Fatalf("classification lost: %s", raw)
	}
	if SortValue(restored, "ipAttributes") != "住宅IP" {
		t.Fatal("classified attributes missing from sort")
	}
	meta := ResultsMetadata(Snapshot{Nodes: []Node{restored}})
	if len(meta.Facets.IPAttributes) != 1 || meta.Facets.IPAttributes[0].Value != "住宅IP" || meta.Facets.IPAttributes[0].Count != 1 {
		t.Fatalf("wrong facets: %+v", meta.Facets)
	}
	if len(meta.Facets.IPSource) != 0 {
		t.Fatal("failed risk source should not appear")
	}
}
