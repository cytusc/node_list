package main

import (
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"testing"
	"time"
)

func TestStoredEvidenceKeepsSourceAndFreshness(t *testing.T) {
	now := time.Now().UTC()
	v := saved{IP: "51.158.105.199", CheckedAt: now.Add(-25 * time.Hour).Format(time.RFC3339), Attributes: []string{"住宅IP", "机房IP"}, Classification: &ipcheck.Result{
		IP: "51.158.105.199", CheckedAt: now.Format(time.RFC3339), Types: []ipcheck.TypeEvidence{{Provider: "ippure", UsageType: "住宅IP"}, {Provider: "ip-api", UsageType: "机房IP"}, {Provider: "ipregistry", UsageType: "机房IP"}},
	}}
	r := ipcheck.Score(v.IP, storedEvidence(v), now)
	if len(r.Types) != 2 || r.Types[0].Provider == "ippure" || r.Attributes(nil)[0] != "机房IP" {
		t.Fatalf("stale IPPure source reused or summary leaked: %+v", r)
	}
	v.Classification.IP = "1.1.1.1"
	if len(storedEvidence(v)) != 0 {
		t.Fatal("evidence reused for another IP")
	}
	v.Classification = nil
	if len(storedEvidence(v)) != 0 {
		t.Fatal("merged labels treated as original IPPure evidence")
	}
}

func TestFailedStoredRefreshCannotEraseOtherHistoricalEvidence(t *testing.T) {
	now := time.Now().UTC()
	old := ipcheck.StoredTypeEvidence("8.8.8.8", now.Add(-time.Hour).Format(time.RFC3339), ipcheck.TypeEvidence{Provider: "ipregistry", UsageType: "机房IP"})
	failed := saved{IP: "8.8.8.8", Classification: &ipcheck.Result{IP: "8.8.8.8", Evidence: []ipcheck.Evidence{{Provider: "ipregistry", IP: "8.8.8.8", CheckedAt: now.Format(time.RFC3339), Outcome: "unknown"}}}}
	all := append([]ipcheck.Evidence{old}, storedEvidence(failed)...)
	if r := ipcheck.Score("8.8.8.8", all, now); len(r.Types) != 1 {
		t.Fatal("failed refresh erased unexpired evidence")
	}
}
