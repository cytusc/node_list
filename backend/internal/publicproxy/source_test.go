package publicproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestParseProtocolsAndValidation(t *testing.T) {
	tests := []struct {
		format, raw string
		count       int
	}{
		{"proxio", `{"count":3,"proxies":[{"ip":"203.0.113.1","port":8080,"protocols":["HTTP","HTTPS","SOCKS5","CONNECT80"],"code":"JP"},{"ip":"203.0.113.2","port":1080,"protocols":["SOCKS4"]},{"ip":"bad","port":80,"protocols":["HTTP"]}]}`, 2},
		{"xyzs996", `[{"host":"203.0.113.1","port":8080,"protocol":"http","country":"JP","networkType":"residential"},{"host":"203.0.113.1","port":8080,"protocol":"https","country":"JP"},{"host":"203.0.113.2","port":1080,"protocol":"socks4"},{"host":"203.0.113.1","port":8080,"protocol":"socks5"}]`, 2},
		{"http", "203.0.113.1:8080\r\n203.0.113.1:8080\n[2001:db8::1]:80\nbad:80\n203.0.113.2:0\n203.0.113.2:65536\n<html>error</html>\n", 2},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			def := Definition{Name: "test", Format: tt.format}
			nodes, err := Parse(def, []byte(tt.raw))
			if err != nil || len(nodes) != tt.count {
				t.Fatalf("got %d nodes, %v", len(nodes), err)
			}
			for _, n := range nodes {
				if n.TLS || n.IP != "" || n.Country != "" || n.IPPure != nil || n.Connectivity != nil || n.Exit != nil {
					t.Fatalf("unverified evidence imported: %+v", n)
				}
				if n.SubTag != "test" || n.RawConfig == nil {
					t.Fatalf("missing provenance: %+v", n)
				}
				if _, err := domain.ShareLinkProxy(n.RawLink, n.Name); len(n.RawConfig) == 0 && err != nil {
					t.Fatal(err)
				}
				if n.Type != "http" && n.Type != "socks5" {
					t.Fatalf("unsupported type %q", n.Type)
				}
			}
			again, _ := Parse(def, []byte(tt.raw))
			if !reflect.DeepEqual(nodes, again) {
				t.Fatal("unstable ordering")
			}
		})
	}
}

func TestRejectInvalidLists(t *testing.T) {
	for _, tt := range []struct{ format, raw string }{
		{"proxio", `{"count":2,"proxies":[{"ip":"203.0.113.1","port":80,"protocols":["HTTP"]}]}`},
		{"proxio", `{"proxies":`}, {"xyzs996", `[]`}, {"xyzs996", `null`},
		{"xyzs996", `<html>error</html>`}, {"http", ""}, {"http", "not a proxy"},
	} {
		if _, err := Parse(Definition{Name: "test", Format: tt.format}, []byte(tt.raw)); err == nil {
			t.Errorf("accepted %s: %s", tt.format, tt.raw)
		}
	}
}

type recordingIngestor struct {
	snapshots []domain.Snapshot
	err       error
}

func (i *recordingIngestor) Import(_ context.Context, s domain.Snapshot) error {
	if i.err != nil {
		return i.err
	}
	i.snapshots = append(i.snapshots, s)
	return nil
}

func TestRefreshRetainsPreviousOnFailureAndRecovers(t *testing.T) {
	status, body := http.StatusOK, "203.0.113.1:8080\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	i := &recordingIngestor{}
	s := New(Definition{"test", server.URL, "http"}, i, time.Minute)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []struct {
		status int
		body   string
	}{{503, "unavailable"}, {200, "<html>error</html>"}, {200, ""}} {
		status, body = failure.status, failure.body
		if err := s.Refresh(context.Background()); err == nil {
			t.Fatal("accepted failed fetch")
		}
		if len(i.snapshots) != 1 {
			t.Fatal("failure published a snapshot")
		}
	}
	status, body = 200, "203.0.113.2:3128\n"
	i.err = errors.New("database unavailable")
	if err := s.Refresh(context.Background()); !errors.Is(err, i.err) {
		t.Fatalf("import failure lost: %v", err)
	}
	i.err = nil
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(i.snapshots) != 2 || i.snapshots[1].Nodes[0].Server != "203.0.113.2" {
		t.Fatal("did not recover")
	}
	if _, err := time.Parse(time.RFC3339Nano, i.snapshots[1].CheckedAt); err != nil {
		t.Fatal(err)
	}
}

func TestCloseCancelsFetchAndJoins(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	s := New(Definition{"test", server.URL, "http"}, &recordingIngestor{}, time.Minute)
	s.Start(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("source did not fetch on start")
	}
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not cancel fetch")
	}
}

// Explicitly opt in so ordinary tests stay offline and reproducible.
func TestLiveSources(t *testing.T) {
	if os.Getenv("PUBLIC_PROXY_LIVE_TEST") != "1" {
		t.Skip("set PUBLIC_PROXY_LIVE_TEST=1 to verify upstream formats")
	}
	for _, def := range Definitions() {
		t.Run(def.Name, func(t *testing.T) {
			i := &recordingIngestor{}
			s := New(def, i, time.Minute)
			if err := s.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, n := range i.snapshots[0].Nodes {
				if _, err := domain.ShareLinkProxy(n.RawLink, n.Name); len(n.RawConfig) == 0 && err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("parsed and imported %d supported proxies", len(i.snapshots[0].Nodes))
		})
	}
}
