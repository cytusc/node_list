package attributes

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type geolocationRoundTrip func(*http.Request) (*http.Response, error)

func (f geolocationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGeolocationClientReturnsMatchingCountryCode(t *testing.T) {
	client := NewGeolocationClient(&http.Client{Transport: geolocationRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "ipwho.is" || r.URL.Path != "/58.186.168.3" {
			t.Fatalf("unexpected geolocation URL: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true,"ip":"58.186.168.3","country_code":"vn"}`))}, nil
	})})
	country, err := client.Lookup(context.Background(), "58.186.168.3")
	if err != nil || country != "VN" {
		t.Fatalf("Lookup() = %q, %v", country, err)
	}
}

func TestGeolocationClientRejectsMismatchedIP(t *testing.T) {
	client := NewGeolocationClient(&http.Client{Transport: geolocationRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true,"ip":"1.1.1.1","country_code":"US"}`))}, nil
	})})
	if _, err := client.Lookup(context.Background(), "58.186.168.3"); err == nil {
		t.Fatal("mismatched IP response accepted")
	}
}
