// Package exitprobe owns exit-IP observations for available nodes only.
// It never writes availability, connectivity, attributes or shared work queues.
package exitprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/proxyprocess"
)

type Result struct {
	IP        string    `json:"ip,omitempty"`
	Endpoint  string    `json:"endpoint,omitempty"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	Millis    int64     `json:"probeMillis"`
}
type Prober interface {
	Probe(context.Context, json.RawMessage) Result
}
type ProcessProber struct {
	Binary    string
	Timeout   time.Duration
	Endpoints []string
	RootCAs   *x509.CertPool
}

func (p ProcessProber) Probe(parent context.Context, raw json.RawMessage) (r Result) {
	start := time.Now()
	defer func() { r.CheckedAt = time.Now().UTC(); r.Millis = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithTimeout(parent, p.Timeout)
	defer cancel()
	config, err := proxyprocess.Decode(raw)
	if err != nil {
		r.Error = "invalid_config"
		return
	}
	tr, closeProcess, _, err := proxyprocess.Mihomo(ctx, p.Binary, config)
	if err != nil {
		r.Error = err.Error()
		return
	}
	defer closeProcess()
	if p.RootCAs != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	endpoints := p.Endpoints
	if len(endpoints) == 0 {
		endpoints = []string{"https://api.ipify.org", "https://checkip.amazonaws.com", "https://icanhazip.com"}
	}
	for _, endpoint := range endpoints {
		attempt, stop := context.WithTimeout(ctx, 5*time.Second)
		ip := echo(attempt, client, endpoint)
		stop()
		if ip != "" {
			r.IP = ip
			r.Endpoint = endpoint
			return
		}
		if ctx.Err() != nil {
			break
		}
	}
	r.Error = "echo_failed"
	return
}

func echo(ctx context.Context, client *http.Client, endpoint string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	res, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 129))
	if err != nil || len(b) > 128 {
		return ""
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil || !publicIP(ip) {
		return ""
	}
	return ip.Unmap().String()
}

// Echo providers must return a public address, never a local proxy/listener IP.
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		if netip.MustParsePrefix(p).Contains(ip) {
			return false
		}
	}
	return true
}
