package ipcheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBatchCacheRejectsUnrequestedAddressesAndSkipsIndividualLookup(t *testing.T) {
	c := NewClient("", "")
	calls := 0
	c.HTTP = &http.Client{Transport: fixtureTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "POST" || req.URL.Path != "/batch" {
			t.Fatalf("unexpected individual request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"status":"success","query":"8.8.8.8","hosting":true},{"status":"success","query":"1.1.1.1","hosting":true}]`))}, nil
	})}
	if err := c.PrimeIPAPIBatch(context.Background(), []string{"8.8.8.8"}); err != nil {
		t.Fatal(err)
	}
	if e := c.lookup(context.Background(), "ip-api", "8.8.8.8"); e.UsageType != "hosting" || calls != 1 {
		t.Fatalf("batch not reused: %+v", e)
	}
	if _, ok := c.cache["ip-api:1.1.1.1"]; ok {
		t.Fatal("unrequested address cached")
	}
}
