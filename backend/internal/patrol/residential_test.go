package patrol

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type residentialTransport func(*http.Request) (*http.Response, error)

func (f residentialTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func residentialResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestCurrentResidentialVerification(t *testing.T) {
	for _, tc := range []struct {
		name, provider, kind, secondEcho string
		hostLeak                         bool
		want                             bool
	}{
		{"two sources", `{"ip":"8.8.8.8","is_hosting":false,"company":{"type":"isp"}}`, "isp", "8.8.8.8", false, true},
		{"VPN is independent", `{"ip":"8.8.8.8","is_hosting":false,"is_vpn":true,"company":{"type":"isp"}}`, "isp", "8.8.8.8", false, true},
		{"provider hosting conflict", `{"ip":"8.8.8.8","is_hosting":true,"company":{"type":"isp"}}`, "isp", "8.8.8.8", false, false},
		{"explicit non-hosting vote", `{"ip":"8.8.8.8","is_hosting":false,"company":{"type":"business"}}`, "isp", "8.8.8.8", false, true},
		{"reverse conflict", `{"ip":"8.8.8.8","is_hosting":false,"company":{"type":"isp"}}`, "hosting", "8.8.8.8", false, false},
		{"quota error", `{"error":"quota exceeded"}`, "isp", "8.8.8.8", false, false},
		{"rotating exit", `{}`, "isp", "1.1.1.1", false, false},
		{"host leak", `{}`, "isp", "8.8.8.8", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &http.Client{Transport: residentialTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "ipinfo.io" {
					return residentialResponse(tc.provider), nil
				}
				if r.URL.Host == "ip-api.com" {
					return residentialResponse(`{"status":"fail"}`), nil
				}
				if tc.hostLeak {
					return residentialResponse("8.8.8.8"), nil
				}
				return residentialResponse("9.9.9.9"), nil
			})}
			tunnel := &http.Client{Transport: residentialTransport(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "ip-basic"):
					return residentialResponse(`{"ok":true,"data":{"ip":"8.8.8.8","asn":{"number":15169,"organization":"Example","type":"` + tc.kind + `"},"traits":{"is_broadcast":false}}}`), nil
				case strings.Contains(r.URL.Path, "ip-risk"):
					return residentialResponse(`{"ok":true,"data":{"risk_score":10}}`), nil
				case r.URL.Host == "checkip.amazonaws.com":
					return residentialResponse(tc.secondEcho), nil
				default:
					return residentialResponse("8.8.8.8"), nil
				}
			})}
			verify := newResidentialVerifier("key", "https://fixture.invalid", host)
			ip, err := verify(context.Background(), tunnel)
			if (ip == "8.8.8.8") != tc.want {
				t.Fatalf("unexpected recovery verification: %q, error: %v", ip, err)
			}
		})
	}
}

func TestNoTokenStillBuildsThreeSourceRecovery(t *testing.T) {
	if NewResidentialVerifier("", "") == nil {
		t.Fatal("public IP-API and IPPure can form a majority without an IPinfo token")
	}
}
