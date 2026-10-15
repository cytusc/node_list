package collector

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
)

func TestClassificationUsesMajorityEvenWhenIPPureFails(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attrs       []string
		status      string
		wantUnknown int
	}{
		{"minority hosting", []string{"机房IP"}, "success", 0},
		{"IPPure unavailable", nil, "error", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ipcheck.NewClient("", "", "fixture-registry-key")
			c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"ip":"8.8.8.8","asn":{"type":"isp"}}`
				if r.URL.Host == "ip-api.com" {
					body = `{"status":"success","query":"8.8.8.8","hosting":false}`
				} else if r.URL.Host == "api.ipregistry.co" {
					body = `{"ip":"8.8.8.8","connection":{"type":"isp","asn":15169}}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			s := &Scheduler{classification: c}
			result := &domain.IPPureResult{IP: "8.8.8.8", Status: tc.status, Attributes: tc.attrs, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
			s.classify(context.Background(), result, tc.attrs)
			if result.Classification == nil || result.Classification.State != "residential" || result.Classification.ResidentialVotes != 2 || result.Classification.UnknownVotes != tc.wantUnknown+2 {
				t.Fatalf("unexpected classification: %+v", result.Classification)
			}
			wantAttrs := "住宅IP"
			if strings.Join(result.Attributes, ",") != wantAttrs || result.Status != tc.status {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}
