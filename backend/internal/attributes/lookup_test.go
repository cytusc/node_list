package attributes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

func TestFailedLookupPreservesSuccessfulComponents(t *testing.T) {
	old := domain.IPPureResult{IP: "8.8.8.8", Status: "success", Score: 10, CheckedAt: "old", Attributes: []string{"商业宽带"}}
	v := merge(Candidate{IP: old.IP, Previous: old, ClassificationMissing: true}, Result{RiskAttempted: true, RiskError: "timeout"}, time.Now())
	if v.Status != "success" || v.Score != 10 || v.CheckedAt != "old" || v.RiskQueryError != "timeout" {
		t.Fatalf("successful fields lost %+v", v)
	}
}
func TestRiskCompletionPreservesExistingClassification(t *testing.T) {
	old := domain.IPPureResult{IP: "8.8.8.8", Classification: &ipcheck.Result{IP: "8.8.8.8", Types: []ipcheck.TypeEvidence{{Provider: "ipinfo", UsageType: "商业宽带"}}}}
	v := merge(Candidate{IP: old.IP, Previous: old, RiskMissing: true}, Result{RiskAttempted: true, Risk: &collector.IPPureResult{IP: old.IP, Score: 7}}, time.Now())
	if v.Classification != old.Classification || v.Score != 7 || len(v.Attributes) == 0 {
		t.Fatalf("classification lost %+v", v)
	}
}

func TestLocationFallbackFillsOnlyMissingCountry(t *testing.T) {
	now := time.Now()
	got := merge(Candidate{IP: "58.186.168.3", CountryMissing: true}, Result{Country: "VN"}, now)
	if got.Country != "VN" {
		t.Fatalf("fallback country not saved: %+v", got)
	}

	previous := domain.IPPureResult{Country: "HK"}
	got = merge(Candidate{IP: "58.186.168.3", Previous: previous, CountryMissing: true}, Result{Country: "VN"}, now)
	if got.Country != "HK" {
		t.Fatalf("fallback overwrote existing country: %+v", got)
	}
}

func TestRiskCompletionReusesExistingCountry(t *testing.T) {
	for _, country := range []string{"", "HK"} {
		t.Run("upstream-country="+country, func(t *testing.T) {
			previous := domain.IPPureResult{IP: "58.186.168.3", Country: "VN"}
			got := merge(Candidate{IP: previous.IP, Previous: previous, RiskMissing: true}, Result{
				RiskAttempted: true,
				Risk:          &collector.IPPureResult{IP: previous.IP, Country: country, Score: 7},
			}, time.Now())
			if got.Country != "VN" || got.Status != "success" || got.Score != 7 {
				t.Fatalf("existing country lost while filling risk: %+v", got)
			}
		})
	}
}

func TestClientQueriesOnlyMissingComponentsAndCoolsDownRisk(t *testing.T) {
	var riskCalls, classCalls atomic.Int32
	riskServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { riskCalls.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer riskServer.Close()
	classification := ipcheck.NewClient("token", "")
	classification.Providers = []string{"ipinfo"}
	classification.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		classCalls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ip":"8.8.8.8","asn":{"type":"business"}}`))}, nil
	})}
	client := NewClient(classification, riskServer.URL)
	got := client.Lookup(context.Background(), Candidate{IP: "8.8.8.8", ClassificationMissing: true})
	if len(got.Evidence) != 1 || classCalls.Load() != 1 || riskCalls.Load() != 0 {
		t.Fatalf("classification-only queried risk: %+v", got)
	}
	got = client.Lookup(context.Background(), Candidate{IP: "8.8.8.8", RiskMissing: true})
	if !got.RiskAttempted || got.RiskError != "http-403" || riskCalls.Load() != 1 || classCalls.Load() != 1 {
		t.Fatalf("risk-only queried classification: %+v", got)
	}
	got = client.Lookup(context.Background(), Candidate{IP: "1.1.1.1", RiskMissing: true})
	if got.RiskAttempted || got.RiskError != "quota-cooldown" || riskCalls.Load() != 1 {
		t.Fatalf("global upstream cooldown ignored: %+v", got)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
