package main

import (
	"context"
	"github.com/cytusc/node_list/backend/internal/patrol"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
)

func TestExitEvidenceDoesNotMistakeRotationOrDirectTrafficForOneProxyExit(t *testing.T) {
	for _, tc := range []struct {
		name                string
		checks              []check
		wantIP, wantOutcome string
	}{
		{"independent agreement", []check{{Target: "a", OK: true, IP: "8.8.8.8"}, {Target: "b", OK: true, IP: "8.8.8.8"}}, "8.8.8.8", "consistent"},
		{"dual stack agreement", []check{{Target: "a", OK: true, IP: "8.8.8.8"}, {Target: "b", OK: true, IP: "8.8.8.8"}, {Target: "c", OK: true, IP: "2001:4860:4860::8888"}}, "8.8.8.8", "consistent"},
		{"one provider retried", []check{{Target: "a", OK: true, IP: "8.8.8.8"}, {Target: "a", OK: true, IP: "8.8.8.8"}}, "", "insufficient-evidence"},
		{"rotating despite majority", []check{{Target: "a", OK: true, IP: "8.8.8.8"}, {Target: "b", OK: true, IP: "8.8.8.8"}, {Target: "c", OK: true, IP: "1.1.1.1"}}, "", "mixed-exits"},
		{"host leak", []check{{Target: "a", OK: true, IP: "89.106.76.82"}, {Target: "b", OK: true, IP: "89.106.76.82"}}, "", "host-egress"},
		{"private address", []check{{Target: "a", OK: true, IP: "192.168.1.1"}, {Target: "b", OK: true, IP: "192.168.1.1"}}, "", "non-public-ipv4"},
		{"error body contains IP", []check{{Target: "a", OK: true, IP: "8.8.8.8"}, {Target: "b", OK: false, IP: "8.8.8.8"}}, "", "insufficient-evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip, outcome := consensusExitIP(tc.checks, "89.106.76.82")
			if ip != tc.wantIP || outcome != tc.wantOutcome {
				t.Fatalf("got %q %q", ip, outcome)
			}
		})
	}
}

func TestClassificationRequiresMatchingAddressAndExplicitNonHostingEvidence(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"Scaleway conflict", `{"data":{"ip":"8.8.8.8","asn":{"type":"hosting"},"privacy":{"hosting":true}}}`, "provider-conflict"},
		{"ISP with VPN remains residential", `{"data":{"ip":"8.8.8.8","asn":{"type":"isp"},"company":{"type":"isp"},"privacy":{"hosting":false,"vpn":true}}}`, "non-hosting"},
		{"missing hosting evidence", `{"data":{"ip":"8.8.8.8","asn":{"type":"isp"}}}`, "supports-residential"},
		{"wrong query response", `{"data":{"ip":"1.1.1.1","asn":{"type":"isp"},"privacy":{"hosting":false}}}`, "unknown"},
		{"conflicting hosting flags", `{"data":{"ip":"8.8.8.8","asn":{"type":"isp"},"privacy":{"hosting":false},"is_hosting":true}}`, "provider-conflict"},
		{"hosting company on ISP ASN", `{"data":{"ip":"8.8.8.8","asn":{"type":"isp"},"company":{"type":"hosting"},"privacy":{"hosting":false}}}`, "provider-conflict"},
		{"rate limit response", `{"error":"rate limited"}`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := parseCrossCheck([]byte(tc.raw), "8.8.8.8", "now")
			if r.Outcome != tc.want {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestIPAPIClassificationDoesNotCountMissingHostingOrMismatchedResponses(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"status":"success","query":"8.8.8.8","hosting":true}`, "provider-conflict"},
		{`{"status":"success","query":"8.8.8.8","hosting":false,"proxy":true}`, "non-hosting"},
		{`{"status":"success","query":"8.8.8.8"}`, "unknown"},
		{`{"status":"success","query":"1.1.1.1","hosting":false}`, "unknown"},
		{`{"status":"fail","message":"quota reached"}`, "unknown"},
	} {
		r := parseIPAPICrossCheck([]byte(tc.raw), "8.8.8.8", "now")
		if r.Outcome != tc.want {
			t.Fatalf("input %s got %+v", tc.raw, r)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSingleBlockedSiteDoesNotHideWorkingTunnelAndRotationIsUnconfirmed(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, "8.8.8.8"
		switch r.URL.Host {
		case "github.com":
			status = 403
			body = "denied"
		case "www.gstatic.com":
			status = 204
			body = ""
		case "www.cloudflare.com":
			body = "ip=1.1.1.1\n"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	var r record
	probe(context.Background(), client, &r, collector.NewASNCache())
	if !r.Reachable || r.VerifiedIP != "" || r.Residential || r.IPConsensus != "mixed-exits" {
		t.Fatalf("unexpected result %+v", r)
	}
}

func TestMinorityHostingEvidenceDoesNotVetoResidentialMajority(t *testing.T) {
	old := &report{Records: []record{{VerifiedIP: "8.8.8.8", CrossCheck: &crossCheck{Provider: "ip-api", IP: "8.8.8.8", CheckedAt: time.Now().UTC().Format(time.RFC3339), Outcome: "provider-conflict"}}}}
	r := report{Records: []record{supportedRecord("8.8.8.8"), supportedRecord("1.1.1.1")}}
	mergeClassificationEvidence(&r, old)
	if !r.Records[0].Residential || !r.Records[0].EvidenceConflict || !r.Records[1].Residential || r.Residential != 2 {
		t.Fatalf("majority or minority evidence lost: %+v", r)
	}
	mergeClassificationEvidence(&r, old)
	if len(r.Records[0].AdditionalEvidence) != 1 {
		t.Fatal("repeated evidence was not deduplicated")
	}
}

func TestClassificationDoesNotReuseVerdictForDifferentExit(t *testing.T) {
	r := report{Records: []record{{
		Reachable: true, VerifiedIP: "8.8.8.8", Residential: true,
		Verdict: &collector.IPPureResult{IP: "1.1.1.1", Attributes: []string{"住宅IP"}},
	}}}
	crossCheckReport(context.Background(), &r)
	if r.Residential != 0 || r.IPPureResidential != 0 || r.Records[0].Residential || r.Records[0].IPPureResidential {
		t.Fatal("classification reused for a different current exit")
	}
}

func supportedRecord(ip string) record {
	no := false
	now := time.Now().UTC().Format(time.RFC3339)
	return record{ID: ip, Reason: patrol.PatrolRetireReason, Retired: true, RetiredAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339), Reachable: true, VerifiedIP: ip, ProbedAt: now,
		Verdict:    &collector.IPPureResult{IP: ip, Attributes: []string{"住宅IP"}},
		CrossCheck: &crossCheck{IP: ip, Provider: "ipinfo", CheckedAt: now, Hosting: &no, CompanyType: "isp", Outcome: "supports-residential"}}
}

func TestClassificationStatesAndStaleEvidence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		mutate   func(*record)
		state    string
		eligible bool
	}{
		{"two sources", func(v *record) {}, "residential", true},
		{"no independent source", func(v *record) { v.CrossCheck = nil }, "unknown", false},
		{"old tunnel", func(v *record) { v.ProbedAt = now.Add(-25 * time.Hour).Format(time.RFC3339) }, "unknown", false},
		{"old provider", func(v *record) { v.CrossCheck.CheckedAt = now.Add(-25 * time.Hour).Format(time.RFC3339) }, "unknown", false},
		{"hosting conflict", func(v *record) { v.CrossCheck.Outcome = "provider-conflict" }, "disputed", false},
		{"no ASN residential", func(v *record) { v.Verdict = nil }, "unknown", false},
		{"inverse conflict", func(v *record) { v.Verdict.Attributes = []string{"机房IP"} }, "disputed", false},
		{"old IP", func(v *record) { v.CrossCheck.IP = "1.1.1.1" }, "unknown", false},
		{"old weak legacy positive", func(v *record) { v.CrossCheck.CompanyType = "" }, "residential", true},
		{"hosting supported", func(v *record) {
			v.Verdict.Attributes = []string{"商业宽带"}
			v.CrossCheck.Outcome = "provider-conflict"
		}, "unknown", false},
		{"stale hosting hold", func(v *record) {
			v.Verdict = nil
			v.CrossCheck.Outcome = "provider-conflict"
			v.CrossCheck.CheckedAt = now.Add(-25 * time.Hour).Format(time.RFC3339)
		}, "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := supportedRecord("8.8.8.8")
			tc.mutate(&v)
			classifyRecord(&v, now)
			if v.Classification != tc.state || v.Residential != tc.eligible {
				t.Fatalf("got %+v", v)
			}
		})
	}
}

func TestRecoveryRequiresTwoDistinctFreshRoundsAndSameRetirement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*record)
		want   bool
	}{
		{"two qualified rounds", func(v *record) {}, true},
		{"same probe", func(v *record) { v.ProbedAt = time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339) }, false},
		{"new exit", func(v *record) { v.VerifiedIP = "1.1.1.1" }, false},
		{"new retirement", func(v *record) { v.RetiredAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339) }, false},
		{"legacy no timestamp", func(v *record) { v.ProbedAt = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := supportedRecord("8.8.8.8")
			old.ProbedAt = time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339)
			current := supportedRecord("8.8.8.8")
			current.RetiredAt = old.RetiredAt
			tc.mutate(&current)
			r := report{Records: []record{current}}
			prior := report{Records: []record{old}}
			mergeClassificationEvidence(&r, &prior)
			markRecoveryEligibility(&r, &prior)
			if r.Records[0].RecoveryEligible != tc.want {
				t.Fatalf("got %+v", r.Records[0])
			}
		})
	}
}

func TestLatestUnknownSurvivesAuditEvidenceMerge(t *testing.T) {
	now := time.Now().UTC()
	current := supportedRecord("8.8.8.8")
	current.CrossCheck = nil
	current.AdditionalEvidence = []crossCheck{{Provider: "ipinfo", IP: "8.8.8.8", CheckedAt: now.Format(time.RFC3339Nano), Outcome: "unknown"}}
	old := supportedRecord("8.8.8.8")
	old.CrossCheck.CheckedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
	r := report{Records: []record{current}}
	mergeClassificationEvidence(&r, &report{Records: []record{old}})
	if r.Records[0].Residential || r.Records[0].Classification != "unknown" {
		t.Fatalf("old success replaced new abstention: %+v", r.Records[0])
	}
}
