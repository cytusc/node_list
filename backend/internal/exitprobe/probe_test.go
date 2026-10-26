package exitprobe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEchoRejectsInvalidOrNonPublicResponses(t *testing.T) {
	for _, body := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1", "::1", "::ffff:192.168.1.1", "2001:db8::1", "not-an-ip", strings.Repeat("x", 129), "8.8.8.8\n1.1.1.1"} {
		t.Run(body[:min(len(body), 30)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			if got := echo(context.Background(), server.Client(), server.URL); got != "" {
				t.Fatalf("accepted %s", got)
			}
		})
	}
	for _, body := range []string{"8.8.8.8\n", "2606:4700:4700::1111"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		got := echo(context.Background(), server.Client(), server.URL)
		server.Close()
		if got != strings.TrimSpace(body) {
			t.Fatalf("valid echo %q", got)
		}
	}
}
func TestRealMihomoExitUsesNodeAndNeverDirectFallback(t *testing.T) {
	binary := os.Getenv("LIVENESS_TEST_MIHOMO")
	if binary == "" {
		t.Skip("set LIVENESS_TEST_MIHOMO")
	}
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "8.8.8.8\n") }))
	defer target.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	var tunneled atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(405)
			return
		}
		dest, err := net.DialTimeout("tcp", target.Listener.Addr().String(), time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer dest.Close()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		tunneled.Add(1)
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		copied := make(chan struct{})
		go func() { _, _ = io.Copy(dest, rw); _ = dest.Close(); close(copied) }()
		_, _ = io.Copy(conn, dest)
		_ = conn.Close()
		<-copied
	}))
	defer upstream.Close()
	host, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	raw, _ := json.Marshal(map[string]any{"rawConfig": map[string]any{"type": "http", "server": host, "port": n}})
	p := ProcessProber{Binary: binary, Timeout: 3 * time.Second, Endpoints: []string{target.URL}, RootCAs: roots}
	r := p.Probe(context.Background(), raw)
	if r.IP != "8.8.8.8" || r.Error != "" || tunneled.Load() == 0 {
		t.Fatalf("probe bypassed node or failed: %+v tunnels=%d", r, tunneled.Load())
	}
	upstream.Close()
	r = p.Probe(context.Background(), raw)
	if r.IP != "" || r.Error == "" {
		t.Fatalf("dead node fell back to direct %+v", r)
	}
	// Caller cancellation must kill and join the private process promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	r = p.Probe(ctx, raw)
	if r.IP != "" || time.Since(started) > time.Second {
		t.Fatalf("cancelled probe %+v", r)
	}
}
