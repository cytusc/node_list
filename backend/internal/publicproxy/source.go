// Package publicproxy imports public proxy lists into the normal probe pipeline.
package publicproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

type Definition struct {
	Name, URL, Format string
}

func Definitions() []Definition {
	return []Definition{
		{"proxio-all", "https://raw.githubusercontent.com/proxio-io/proxy-list/main/all.json", "proxio"},
		{"Jakee-http", "https://raw.githubusercontent.com/Jakee8718/Free-Proxies/main/proxy/http.txt", "http"},
		{"xyzs996-all", "https://raw.githubusercontent.com/xyzs996/free-proxy-health-list/main/proxies/all/data.json", "xyzs996"},
		// fmw86 publishes already-screened residential/ISP entry addresses as
		// one "host:port" per line, split by protocol. The "residential" tag is
		// the upstream's claim, not evidence: these enter the normal probe
		// pipeline and only liveness/exit checks decide whether they work.
		{"fmw86-socks5", "https://raw.githubusercontent.com/fmw86/free-proxy-pool/main/socks5_residential.txt", "socks5"},
		{"fmw86-http", "https://raw.githubusercontent.com/fmw86/free-proxy-pool/main/http_residential.txt", "http"},
	}
}

// HTTPS in these lists means HTTP CONNECT, not TLS to the proxy itself.
// SOCKS4 has no supported dialer and must not be reinterpreted as SOCKS5.
func node(source, host string, port int, protocol, country string) *domain.Node {
	kind := strings.ToLower(protocol)
	switch kind {
	case "http", "https":
		kind = "http"
	case "socks5":
	default:
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || port < 1 || port > 65535 {
		return nil
	}
	host = ip.String()
	name := source + " " + kind + "://" + net.JoinHostPort(host, strconv.Itoa(port))
	config := domain.ProxyConfig{"name": name, "type": kind, "server": host, "port": port}
	n := domain.SummarizeProxy(config)
	n.BaseName, n.SubTag, n.RawConfig = name, source, config
	// An upstream claim is never an observed exit IP or classification.
	n.ClaimedCountry = country
	return &n
}

func Parse(def Definition, raw []byte) ([]domain.Node, error) {
	unique := map[string]domain.Node{}
	add := func(host string, port int, protocol, country string) {
		if n := node(def.Name, host, port, protocol, country); n != nil {
			raw, _ := json.Marshal(n.RawConfig)
			unique[string(raw)] = *n
		}
	}
	switch def.Format {
	case "proxio":
		var data struct {
			Count   int `json:"count"`
			Proxies []struct {
				IP        string   `json:"ip"`
				Port      int      `json:"port"`
				Protocols []string `json:"protocols"`
				Code      string   `json:"code"`
			} `json:"proxies"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		if data.Count != len(data.Proxies) {
			return nil, fmt.Errorf("incomplete proxio list: %d/%d", len(data.Proxies), data.Count)
		}
		for _, p := range data.Proxies {
			for _, protocol := range p.Protocols {
				add(p.IP, p.Port, protocol, p.Code)
			}
		}
	case "xyzs996":
		var data []struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			Country  string `json:"country"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		for _, p := range data {
			add(p.Host, p.Port, p.Protocol, p.Country)
		}
	case "http", "socks5":
		scanner := bufio.NewScanner(strings.NewReader(string(raw)))
		for scanner.Scan() {
			host, port, err := net.SplitHostPort(strings.TrimSpace(scanner.Text()))
			if err != nil {
				continue
			}
			number, err := strconv.Atoi(port)
			if err == nil {
				add(host, number, def.Format, "")
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown list format %q", def.Format)
	}
	if len(unique) == 0 {
		return nil, fmt.Errorf("%s returned no supported proxies", def.Name)
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	nodes := make([]domain.Node, 0, len(keys))
	for _, key := range keys {
		nodes = append(nodes, unique[key])
	}
	return nodes, nil
}

type Ingestor interface {
	Import(context.Context, domain.SourceBatch) error
}

type Source struct {
	OnResult func(error)
	def      Definition
	ingestor Ingestor
	client   *http.Client
	interval time.Duration
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	// last is the digest of the most recently imported body. Public lists are
	// republished on a fixed schedule; when the bytes are unchanged there is
	// nothing to convert or persist, so the expensive parse/import/transaction
	// run is skipped. A failed import is never recorded, so it retries.
	mu       sync.Mutex
	last     [32]byte
	haveLast bool
}

func New(def Definition, ingestor Ingestor, interval time.Duration) *Source {
	if interval < time.Minute {
		interval = 30 * time.Minute
	}
	return &Source{def: def, ingestor: ingestor, client: &http.Client{Timeout: 60 * time.Second}, interval: interval}
}

func (s *Source) Name() string { return s.def.Name }

// Refresh publishes only complete, nonempty responses, without importing any
// upstream connectivity, exit or classification evidence. Workers verify them.
func (s *Source) Refresh(ctx context.Context) (result error) {
	defer func() {
		if s.OnResult != nil {
			s.OnResult(result)
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.def.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "node-explorer")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list request failed: HTTP %d", resp.StatusCode)
	}
	const maxSize = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return err
	}
	if len(raw) > maxSize {
		return fmt.Errorf("list exceeds 32 MiB")
	}
	// Lists are republished on a schedule, so most polls return identical
	// bytes. Parsing them again would spawn the converter and open a batch
	// transaction that only bumps fetched_at, so skip unchanged bodies. A
	// failed import leaves the digest unrecorded and is retried next tick.
	digest := sha256.Sum256(raw)
	s.mu.Lock()
	unchanged := s.haveLast && s.last == digest
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	nodes, err := Parse(s.def, raw)
	if err != nil {
		return err
	}
	if err := s.ingestor.Import(ctx, domain.SourceBatch{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Nodes: nodes}); err != nil {
		return err
	}
	s.mu.Lock()
	s.last, s.haveLast = digest, true
	s.mu.Unlock()
	log.Printf("public proxy source %s: imported %d nodes", s.Name(), len(nodes))
	return nil
}

func (s *Source) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
				log.Printf("public proxy source %s: refresh failed: %v", s.Name(), err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Source) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}
