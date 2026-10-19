package connectivitybench

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cytusc/node_list/backend/internal/transport"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDiagnosticProbePreservesProductionRequestContract(t *testing.T) {
	for _, status := range []int{200, 204, 301, 403, 429, 500} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.String() != "https://github.com/" || r.Header.Get("User-Agent") != "node-explorer-github-patrol" {
				t.Fatal("request differs from production")
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("body")), Request: r}, nil
		})}
		want, err := transport.ProbeHTTP(context.Background(), client)
		if err != nil {
			t.Fatal(err)
		}
		got, diagnostic := diagnosticHTTP(context.Background(), client)
		if got != want || (diagnostic == nil) != (status == 200) {
			t.Fatalf("status %d changed availability semantics", status)
		}
	}
}
