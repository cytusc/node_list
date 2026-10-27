package liveness

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Optional real-kernel check, entirely loopback and without subscription secrets.
func TestRealPrivateKernels(t *testing.T) {
	for _, name := range []string{"xray", "mihomo"} {
		t.Run(name, func(t *testing.T) {
			binary := os.Getenv("LIVENESS_TEST_" + map[string]string{"xray": "XRAY", "mihomo": "MIHOMO"}[name])
			if binary == "" {
				t.Skip("set LIVENESS_TEST_XRAY / LIVENESS_TEST_MIHOMO")
			}
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/primary" {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(204)
			}))
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
			number, _ := strconv.Atoi(port)
			p := Proxy{"name": "probe", "type": "http", "server": host, "port": number}
			e := ProcessEngine{Name: name, Binary: binary, Target: target.URL + "/primary", BackupTarget: target.URL, Expected: 204, Timeout: 3 * time.Second, RootCAs: roots}
			r := e.Probe(context.Background(), p)
			if !r.Reachable || r.Status != "reachable" {
				t.Fatalf("private kernel proxy failed: %+v", r)
			}
			if tunneled.Load() < 2 || r.ProbeTarget != target.URL || r.Targets[e.Target] || !r.Targets[e.BackupTarget] {
				t.Fatal("backup did not use the upstream proxy")
			}
			upstream.Close()
			r = e.Probe(context.Background(), p)
			if r.Status != "unreachable" {
				t.Fatalf("failed tunnel classification: %+v", r)
			}
		})
	}
}

func TestRealXrayConfigurationMatrix(t *testing.T) {
	binary := os.Getenv("LIVENESS_TEST_XRAY")
	if binary == "" {
		t.Skip("set LIVENESS_TEST_XRAY")
	}
	for _, protocol := range []string{"http", "socks5", "ss", "vmess", "vless", "trojan"} {
		for _, network := range []string{"tcp", "ws", "grpc", "xhttp"} {
			if (protocol == "http" || protocol == "socks5" || protocol == "ss") && network != "tcp" {
				continue
			}
			t.Run(protocol+"/"+network, func(t *testing.T) {
				p := Proxy{"type": protocol, "server": "127.0.0.1", "port": 443, "network": network}
				switch protocol {
				case "vmess", "vless":
					p["uuid"] = "123e4567-e89b-12d3-a456-426614174000"
					p["tls"] = true
				case "trojan":
					p["password"] = "example"
				case "ss":
					p["password"] = "example"
					p["cipher"] = "aes-128-gcm"
				}
				out, reason := XrayOutbound(p)
				if reason != "" {
					t.Fatal(reason)
				}
				cfg, _ := json.Marshal(Proxy{"outbounds": []any{out}})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, "run", "-test", "-config", "stdin:")
				cmd.Stdin = bytes.NewReader(cfg)
				if result, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("configuration rejected: %v %s", err, result)
				}
			})
		}
	}
}
