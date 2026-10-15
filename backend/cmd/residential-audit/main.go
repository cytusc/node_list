// Command residential-audit reads archived candidates and measures
// their current tunnels without changing node, enrichment or retirement data.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

type candidate struct {
	node     domain.Node
	identity string
	result   record
}

type check struct {
	Target    string `json:"target"`
	Status    int    `json:"status,omitempty"`
	OK        bool   `json:"ok"`
	IP        string `json:"ip,omitempty"`
	ElapsedMs int64  `json:"elapsedMs"`
	Failure   string `json:"failure,omitempty"`
}

type record struct {
	Voting               *ipcheck.Result         `json:"voting,omitempty"`
	RecoveryEligible     bool                    `json:"recoveryEligible"`
	ID                   string                  `json:"id"`
	SourceSnapshotID     int                     `json:"sourceSnapshotId"`
	SourceNodeIndex      int                     `json:"sourceNodeIndex"`
	Name                 string                  `json:"name"`
	Type                 string                  `json:"type"`
	Retired              bool                    `json:"retired"`
	RetiredAt            string                  `json:"retiredAt,omitempty"`
	Reason               string                  `json:"reason,omitempty"`
	HistoricalIP         string                  `json:"historicalIP,omitempty"`
	HistoricalAttributes []string                `json:"historicalAttributes,omitempty"`
	Classification       string                  `json:"classification"`
	ClassificationReason string                  `json:"classificationReason,omitempty"`
	HistoricalCheckedAt  string                  `json:"historicalCheckedAt,omitempty"`
	ProbedAt             string                  `json:"probedAt,omitempty"`
	Checks               []check                 `json:"checks,omitempty"`
	VerifiedIP           string                  `json:"verifiedIP,omitempty"`
	IPConsensus          string                  `json:"ipConsensus,omitempty"`
	Verdict              *collector.IPPureResult `json:"verdict,omitempty"`
	ClassificationError  string                  `json:"classificationError,omitempty"`
	IPPureResidential    bool                    `json:"ippureResidential"`
	CrossCheck           *crossCheck             `json:"crossCheck,omitempty"`
	AdditionalEvidence   []crossCheck            `json:"additionalEvidence,omitempty"`
	EvidenceConflict     bool                    `json:"evidenceConflict,omitempty"`
	Reachable            bool                    `json:"reachable"`
	Residential          bool                    `json:"residential"`
	Error                string                  `json:"error,omitempty"`
}

type report struct {
	Scope                string   `json:"scope,omitempty"`
	Offset               int      `json:"offset"`
	TotalCandidates      int      `json:"totalCandidates"`
	StartedAt            string   `json:"startedAt"`
	CompletedAt          string   `json:"completedAt"`
	InventoryOnly        bool     `json:"inventoryOnly"`
	Candidates           int      `json:"candidates"`
	Retired              int      `json:"retired"`
	Reachable            int      `json:"reachable"`
	Residential          int      `json:"residential"`
	IPPureResidential    int      `json:"ippureResidential"`
	RetiredResidential   int      `json:"retiredResidential"`
	UniqueResidentialIPs int      `json:"uniqueResidentialIPs"`
	Records              []record `json:"records"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	output := flag.String("output", "residential-audit.json", "credential-free JSON report")
	input := flag.String("input", "", "cross-check an existing audit report without probing nodes or reading the database")
	evidence := flag.String("evidence", "", "previous audit report for per-provider evidence and two-round recovery checks")
	inventory := flag.Bool("inventory", false, "only list archived candidates")
	scope := flag.String("scope", "all", "candidate scope: all or historical-residential")
	limit := flag.Int("limit", 200, "maximum nodes to probe per batch; 0 means all (inventory is unlimited)")
	offset := flag.Int("offset", 0, "skip this many candidates in stable ID order")
	retiredOnly := flag.Bool("retired-only", false, "probe retired candidates only")
	ids := flag.String("ids", "", "comma-separated report IDs to recheck")
	concurrency := flag.Int("concurrency", 8, "nodes per isolated proxy wave (1-32)")
	flag.Parse()
	if (*scope != "all" && *scope != "historical-residential") || *limit < 0 || *offset < 0 {
		return fmt.Errorf("invalid scope, limit or offset")
	}
	if *concurrency < 1 || *concurrency > 32 {
		return fmt.Errorf("concurrency must be 1-32")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	var previous *report
	if *evidence != "" {
		raw, err := os.ReadFile(*evidence)
		if err != nil {
			return err
		}
		previous = &report{}
		if err := json.Unmarshal(raw, previous); err != nil {
			return err
		}
	}
	if *input != "" {
		raw, err := os.ReadFile(*input)
		if err != nil {
			return err
		}
		var r report
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.InventoryOnly {
			return fmt.Errorf("inventory has no current exit evidence")
		}
		for i := range r.Records {
			r.Records[i].VerifiedIP, r.Records[i].IPConsensus = consensusExitIP(r.Records[i].Checks, os.Getenv("AUDIT_HOST_IP"))
		}
		crossCheckReport(ctx, &r)
		mergeClassificationEvidence(&r, previous)
		markRecoveryEligibility(&r, previous)
		return save(*output, r)
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return fmt.Errorf("database configuration invalid")
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "120000"
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database connection failed")
	}
	defer db.Close(context.Background())
	rows, err := db.Query(ctx, `SELECT s.id,(n.ordinality-1)::int,n.node::text,
 COALESCE(e.payload::jsonb, '{}'::jsonb)::text
 FROM node_explorer_snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(s.payload::jsonb->'nodes') WITH ORDINALITY AS n(node,ordinality)
 LEFT JOIN node_explorer_ippure_results e ON e.snapshot_id=s.id AND e.node_index=n.ordinality-1
 WHERE $1='all' OR (e.payload::jsonb->>'status'='success' AND e.payload::jsonb->'attributes' ? '住宅IP')`, *scope)
	if err != nil {
		return fmt.Errorf("read candidate history: %w", err)
	}
	byID := map[string]candidate{}
	for rows.Next() {
		var raw, enrichment []byte
		var snapshotID, nodeIndex int
		if err := rows.Scan(&snapshotID, &nodeIndex, &raw, &enrichment); err != nil {
			rows.Close()
			return err
		}
		var n domain.Node
		var old domain.IPPureResult
		if err := json.Unmarshal(raw, &n); err != nil {
			rows.Close()
			return fmt.Errorf("decode archived node: %w", err)
		}
		if err := json.Unmarshal(enrichment, &old); err != nil {
			rows.Close()
			return err
		}
		id := domain.AggregateIdentity(n)
		if id == "" {
			continue
		}
		if prev, ok := byID[id]; ok && (prev.result.SourceSnapshotID > snapshotID || (prev.result.SourceSnapshotID == snapshotID && prev.result.HistoricalCheckedAt >= old.CheckedAt)) {
			continue
		}
		hash := sha256.Sum256([]byte(id))
		byID[id] = candidate{node: n, identity: id, result: record{ID: hex.EncodeToString(hash[:]), SourceSnapshotID: snapshotID, SourceNodeIndex: nodeIndex, Name: n.Name, Type: n.Type, HistoricalIP: old.IP, HistoricalAttributes: old.Attributes, Classification: "unknown", HistoricalCheckedAt: old.CheckedAt}}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	rows, err = db.Query(ctx, "SELECT identity,retired_at,reason FROM node_explorer_retired_nodes")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, retiredAt, reason string
		if err := rows.Scan(&id, &retiredAt, &reason); err != nil {
			rows.Close()
			return err
		}
		if c, ok := byID[id]; ok {
			c.result.Retired = true
			c.result.RetiredAt = retiredAt
			c.result.Reason = reason
			byID[id] = c
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	candidates := make([]candidate, 0, len(byID))
	selected := map[string]bool{}
	if *ids != "" {
		for _, id := range strings.Split(*ids, ",") {
			selected[strings.TrimSpace(id)] = true
		}
	}
	for _, c := range byID {
		if len(selected) > 0 && !selected[c.result.ID] {
			continue
		}
		if !*retiredOnly || c.result.Retired {
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].result.ID < candidates[j].result.ID })
	totalCandidates := len(candidates)
	if *offset >= len(candidates) {
		candidates = nil
	} else {
		candidates = candidates[*offset:]
	}
	if !*inventory && *limit > 0 && len(candidates) > *limit {
		candidates = candidates[:*limit]
	}
	r := report{StartedAt: time.Now().UTC().Format(time.RFC3339), InventoryOnly: *inventory, Candidates: len(candidates), Scope: *scope, Offset: *offset, TotalCandidates: totalCandidates}
	router := transport.NewRouter(transport.NewNativeEngine()).WithFallback(transport.NewMihomoEngine([]transport.MihomoConfig{{Controller: os.Getenv("AUDIT_CONTROLLER"), ProxyURL: os.Getenv("AUDIT_PROXY_URL")}}, 15*time.Second))
	asnCache := collector.NewASNCache()
	for offset := 0; offset < len(candidates); offset += *concurrency {
		wave := candidates[offset:min(offset+*concurrency, len(candidates))]
		if !*inventory {
			if os.Getenv("AUDIT_CONTROLLER") == "" || os.Getenv("AUDIT_PROXY_URL") == "" {
				return fmt.Errorf("AUDIT_CONTROLLER and AUDIT_PROXY_URL must point to a dedicated audit instance")
			}
			if os.Getenv("AUDIT_CONTROLLER") == os.Getenv("IPPURE_CONTROLLER") || os.Getenv("AUDIT_CONTROLLER") == os.Getenv("GITHUB_PATROL_CONTROLLER") {
				return fmt.Errorf("audit must use its own controller")
			}
			nodes := make([]domain.Node, len(wave))
			for i := range wave {
				nodes[i] = wave[i].node
			}
			for engine, indexes := range router.Group(nodes) {
				ns := make([]domain.Node, len(indexes))
				for i, j := range indexes {
					ns[i] = nodes[j]
				}
				paths, release, err := engine.Reserve(ctx, ns)
				if err != nil {
					for _, j := range indexes {
						wave[j].result.Error = "proxy reservation failed"
					}
					continue
				}
				var wg sync.WaitGroup
				for i, j := range indexes {
					out := paths[i]
					if out == nil || out.Error != nil {
						wave[j].result.Error = "proxy configuration rejected"
						continue
					}
					wg.Add(1)
					go func(j int, client *http.Client) { defer wg.Done(); probe(ctx, client, &wave[j].result, asnCache) }(j, out.Client)
				}
				wg.Wait()
				if release != nil {
					release()
				}
			}
		}
		for _, c := range wave {
			r.Records = append(r.Records, c.result)
			if c.result.Retired {
				r.Retired++
			}
			if c.result.Reachable {
				r.Reachable++
			}
			if c.result.Residential {
				r.Residential++
				if c.result.Retired {
					r.RetiredResidential++
				}
			}
		}
		r.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		ips := map[string]bool{}
		for _, v := range r.Records {
			if v.Residential {
				ips[v.VerifiedIP] = true
			}
		}
		r.UniqueResidentialIPs = len(ips)
		if err := save(*output, r); err != nil {
			return err
		}
		recount(&r)
		fmt.Fprintf(os.Stderr, "checked=%d/%d reachable=%d ippureResidential=%d\n", len(r.Records), r.Candidates, r.Reachable, r.IPPureResidential)
	}
	if !*inventory {
		crossCheckReport(ctx, &r)
		mergeClassificationEvidence(&r, previous)
		markRecoveryEligibility(&r, previous)
	}
	return save(*output, r)
}

func save(path string, r report) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(path+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func probe(ctx context.Context, client *http.Client, r *record, asnCache *collector.ASNCache) {
	r.ProbedAt = time.Now().UTC().Format(time.RFC3339)
	targets := []struct {
		url    string
		status int
		marker string
	}{
		{"https://github.com/", 200, "<html"},
		{"https://www.gstatic.com/generate_204", 204, ""},
		{"https://www.cloudflare.com/cdn-cgi/trace", 200, "ip="},
		{"https://api.ipify.org", 200, ""},
		{"https://checkip.amazonaws.com/", 200, ""},
	}
	r.Checks = make([]check, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, url string, status int, marker string) {
			defer wg.Done()
			started := time.Now()
			c := check{Target: url}
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
			req.Header.Set("User-Agent", "node-explorer-residential-audit")
			resp, err := client.Do(req)
			if err != nil {
				c.Failure = failure(err)
			} else {
				c.Status = resp.StatusCode
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
				resp.Body.Close()
				c.OK = readErr == nil && c.Status == status && (marker == "" || strings.Contains(strings.ToLower(string(body)), marker))
				if i == 2 {
					for _, line := range strings.Split(string(body), "\n") {
						if strings.HasPrefix(line, "ip=") {
							c.IP = strings.TrimPrefix(line, "ip=")
						}
					}
				}
				if i >= 3 {
					c.IP = strings.TrimSpace(string(body))
					if net.ParseIP(c.IP) == nil {
						c.IP = ""
						c.OK = false
					}
				}
			}
			if c.IP != "" && net.ParseIP(c.IP) == nil {
				c.IP = ""
				c.OK = false
			}
			c.ElapsedMs = time.Since(started).Milliseconds()
			r.Checks[i] = c
		}(i, t.url, t.status, t.marker)
	}
	wg.Wait()
	successes := 0
	for _, c := range r.Checks {
		if c.OK {
			successes++
		}
	}
	r.Reachable = successes >= 2
	r.VerifiedIP, r.IPConsensus = consensusExitIP(r.Checks, os.Getenv("AUDIT_HOST_IP"))
	if !r.Reachable || r.VerifiedIP == "" {
		return
	}
	origin := os.Getenv("IPPURE_API_ORIGIN")
	if origin == "" {
		origin = "https://api.123169.xyz"
	}
	v, err := collector.QueryIPPure(ctx, collector.NewIPPureClient(client, origin, 10*time.Second), r.VerifiedIP, asnCache)
	if err != nil {
		r.ClassificationError = "current exit classification failed"
		return
	}
	r.Verdict = v
	for _, a := range v.Attributes {
		if a == "住宅IP" {
			r.IPPureResidential = true
		}
	}
}

// Count independent providers, not attempts, so retrying one echo cannot
// manufacture agreement. A rotating exit is reachable but needs separate review.
func consensusExitIP(checks []check, hostIP string) (string, string) {
	ips := map[string]int{}
	providers := map[string]bool{}
	for _, c := range checks {
		if !c.OK || c.IP == "" || providers[c.Target] {
			continue
		}
		ip := net.ParseIP(c.IP)
		if ip == nil {
			continue
		}
		providers[c.Target] = true
		// A dual-stack tunnel can legitimately expose one IPv4 and one IPv6.
		// Compare IPv4 observations with each other; IPPure only scores IPv4.
		if ip.To4() == nil {
			continue
		}
		ips[ip.String()]++
	}
	if len(ips) > 1 {
		return "", "mixed-exits"
	}
	for raw, count := range ips {
		if count < 2 {
			return "", "insufficient-evidence"
		}
		ip := net.ParseIP(raw)
		if raw == hostIP {
			return "", "host-egress"
		}
		if ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return "", "non-public-ipv4"
		}
		return raw, "consistent"
	}
	return "", "insufficient-evidence"
}

func failure(err error) string {
	var n interface{ Timeout() bool }
	if v, ok := err.(interface{ Timeout() bool }); ok {
		n = v
	}
	if n != nil && n.Timeout() {
		return "timeout"
	}
	return "connection or TLS failure"
}
