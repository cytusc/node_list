package api

import (
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"net/http"
	"net/url"
	"testing"
)

func TestResidentialFilterIncludesVotesWhenRiskQueryFails(t *testing.T) {
	snapshot := batchOne()
	snapshot.Nodes = snapshot.Nodes[:1]
	snapshot.Nodes[0].IPPure = &domain.IPPureResult{Status: "error", Attributes: []string{"住宅IP"}, Classification: &ipcheck.Result{Version: ipcheck.VotingVersion, State: "residential", ResidentialVotes: 2}}
	f := newFixture(t, snapshot)
	path := "/api/results?page=1&limit=50&ipAttributes=" + url.QueryEscape("住宅IP")
	status, body, _ := f.get(path)
	if status != http.StatusOK || f.number(path, body["total"]) != 1 {
		t.Fatalf("classified node filtered out: status=%d body=%v", status, body)
	}
}
