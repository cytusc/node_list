package domain

import (
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"testing"
)

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
	if restored.IPPure.Classification == nil || restored.IPPure.Classification.ResidentialVotes != 2 {
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
