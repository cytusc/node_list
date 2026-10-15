// Command ip-type-clean refreshes every distinct stored exit IP, including retired
// and historical records. It preserves probe metadata and backs up each changed row.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/jackc/pgx/v5"
)

type saved struct {
	IP             string          `json:"ip"`
	CheckedAt      string          `json:"checkedAt"`
	Attributes     []string        `json:"attributes"`
	Classification *ipcheck.Result `json:"classification"`
}
type entry struct {
	IP             string             `json:"ip"`
	Evidence       []ipcheck.Evidence `json:"evidence"`
	Classification ipcheck.Result     `json:"classification"`
	Attributes     []string           `json:"attributes"`
	Updated        int64              `json:"updated"`
	Unavailable    []string           `json:"unavailable,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	apply := flag.Bool("apply", false, "update records; default is read-only")
	refresh := flag.Bool("refresh", true, "query providers; false only normalizes stored source evidence")
	queryProxy := flag.String("query-proxy", "", "dedicated HTTP proxy used for fresh IPPure and provider queries")
	concurrency := flag.Int("concurrency", 4, "concurrent fresh IPPure lookups (1..16)")
	only := flag.String("ip", "", "limit to one exit IP")
	output := flag.String("output", "ip-type-clean.jsonl", "append-only evidence and progress report (0600)")
	flag.Parse()
	if *concurrency < 1 || *concurrency > 16 {
		return fmt.Errorf("concurrency must be 1..16")
	}
	var queryHTTP *http.Client
	if *refresh {
		proxyURL, err := url.Parse(*queryProxy)
		if err != nil || proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
			return fmt.Errorf("fresh queries require --query-proxy pointing to a dedicated working proxy")
		}
		queryHTTP = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), MaxIdleConns: 64, MaxIdleConnsPerHost: 16}, Timeout: 15 * time.Second}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db, err := pgx.Connect(ctx, "")
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close(context.Background())
	var locked bool
	if err := db.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext('node-list-ip-type-clean'))").Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("another IP type cleaning process is running")
	}
	rows, err := db.Query(ctx, `SELECT payload FROM node_explorer_ippure_results WHERE ($1='' OR payload::jsonb->>'ip'=$1) ORDER BY snapshot_id,node_index`, *only)
	if err != nil {
		return err
	}
	byIP := map[string]saved{}
	history := map[string][]ipcheck.Evidence{}
	records := 0
	invalid := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var v saved
		if err := json.Unmarshal(raw, &v); err != nil {
			rows.Close()
			return err
		}
		if v.IP == "" {
			continue
		}
		if _, err := netip.ParseAddr(v.IP); err != nil {
			invalid++
			continue
		}
		records++
		history[v.IP] = append(history[v.IP], storedEvidence(v)...)
		old, ok := byIP[v.IP]
		if !ok || v.CheckedAt > old.CheckedAt {
			byIP[v.IP] = v
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	ips := make([]string, 0, len(byIP))
	for ip := range byIP {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	file, err := os.OpenFile(*output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	if *apply {
		_, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS node_explorer_ip_type_clean_backup (
   run_id TEXT NOT NULL, snapshot_id INTEGER NOT NULL, node_index INTEGER NOT NULL,
   payload TEXT NOT NULL, PRIMARY KEY(run_id,snapshot_id,node_index))`)
		if err != nil {
			return err
		}
		// Avoid a full enrichment-table scan for every distinct exit IP. Build
		// concurrently so regular collection and API reads can keep running.
		if _, err := db.Exec(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS node_explorer_ippure_exit_ip_idx ON node_explorer_ippure_results ((payload::jsonb->>'ip'))`); err != nil {
			return err
		}
	}
	runID := time.Now().UTC().Format("20060102T150405.000000000Z")
	fmt.Printf("run=%s apply=%t records=%d uniqueIPs=%d invalidIPs=%d\n", runID, *apply, records, len(ips), invalid)
	client := ipcheck.NewClient(os.Getenv("IPINFO_TOKEN"), os.Getenv("IP_API_PRO_KEY"), os.Getenv("IPREGISTRY_API_KEY"), os.Getenv("ABUSEIPDB_API_KEY"))
	client.FreshSweep = true
	if *refresh {
		client.HTTP = queryHTTP
		fmt.Printf("Priming IP-API batches (%d IPs, <=100 IPs/request, <=15 requests/minute)\n", len(ips))
		if err := client.PrimeIPAPIBatch(ctx, ips); err != nil {
			fmt.Printf("IP-API batch unavailable; continuing with individual requests\n")
		}
	}
	type pureLookup struct{ evidence ipcheck.Evidence }
	pureResults := make(map[string]pureLookup)
	var pureMu sync.Mutex
	var pureWG sync.WaitGroup
	pureSlots := make(chan struct{}, *concurrency)
	if *refresh {
		pureClient := collector.NewIPPureClient(queryHTTP, os.Getenv("IPPURE_API_ORIGIN"), 10*time.Second)
		for _, target := range ips {
			pureWG.Add(1)
			go func(ip string) {
				defer pureWG.Done()
				select {
				case pureSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-pureSlots }()
				queryCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				pure, _ := collector.QueryIPPureType(queryCtx, pureClient, ip)
				pureMu.Lock()
				pureResults[ip] = pureLookup{pure}
				count := len(pureResults)
				pureMu.Unlock()
				if count%25 == 0 || count == len(ips) {
					fmt.Printf("IPPure fresh=%d/%d\n", count, len(ips))
				}
			}(target)
		}
		pureWG.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	var total int64
	missing := 0
	for i, ip := range ips {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := time.Now()
		var ev []ipcheck.Evidence
		if *refresh {
			ev = client.Lookup(ctx, ip)
		}
		if *refresh {
			ev = append(ev, pureResults[ip].evidence)
		} else {
			ev = ipcheck.Score(ip, history[ip], now).Evidence
		}
		result := ipcheck.Score(ip, ev, now)
		e := entry{IP: ip, Evidence: ev, Classification: result, Attributes: result.Attributes(nil)}
		available := map[string]bool{}
		for _, t := range result.Types {
			available[t.Provider] = true
		}
		for _, provider := range []string{"ippure", "ipinfo", "ip-api", "ipregistry", "abuseipdb", "ipapi", "ip2location"} {
			if !available[provider] {
				e.Unavailable = append(e.Unavailable, provider)
			}
		}
		if len(result.Types) == 0 {
			missing++
		}
		if *apply {
			e.Updated, err = update(ctx, db, runID, ip, result)
			if err != nil {
				return fmt.Errorf("update %s: %w", ip, err)
			}
			total += e.Updated
		}
		if err := encoder.Encode(e); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		fmt.Printf("%d/%d ip=%s attributes=%v sources=%d updated=%d unavailable=%v\n", i+1, len(ips), ip, e.Attributes, len(result.Types), e.Updated, e.Unavailable)
	}
	fmt.Printf("COMPLETE run=%s uniqueIPs=%d updated=%d noUsableSources=%d report=%s\n", runID, len(ips), total, missing, *output)
	return nil
}

func update(ctx context.Context, db *pgx.Conn, runID, ip string, result ipcheck.Result) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	// Lock current rows after network queries. Preserve every unrelated JSON field,
	// and skip records whose exit IP was concurrently replaced by the collector.
	rows, err := tx.Query(ctx, `SELECT snapshot_id,node_index,payload FROM node_explorer_ippure_results WHERE payload::jsonb->>'ip'=$1 FOR UPDATE`, ip)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, index int
		raw       string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.index, &r.raw); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rawResult, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	for _, r := range all {
		var old saved
		if err := json.Unmarshal([]byte(r.raw), &old); err != nil {
			return 0, err
		}
		attrs, err := json.Marshal(result.Attributes(old.Attributes))
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO node_explorer_ip_type_clean_backup VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, runID, r.id, r.index, r.raw)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `UPDATE node_explorer_ippure_results SET payload=jsonb_set(jsonb_set(payload::jsonb,'{classification}',$3::jsonb),'{attributes}',$4::jsonb)::text WHERE snapshot_id=$1 AND node_index=$2`, r.id, r.index, string(rawResult), string(attrs))
		if err != nil {
			return 0, err
		}
	}
	return int64(len(all)), tx.Commit(ctx)
}

// Historical rows can hold newer provider evidence than the most recent risk
// query. Resolve each provider independently across all rows for this exit IP.
func storedEvidence(previous saved) []ipcheck.Evidence {
	if previous.Classification != nil && previous.Classification.IP == previous.IP {
		if len(previous.Classification.Evidence) > 0 {
			var usable []ipcheck.Evidence
			for _, e := range previous.Classification.Evidence {
				if e.ASNType != "" || e.UsageType != "" || e.CompanyType != "" {
					usable = append(usable, e)
				}
			}
			return usable
		}
		var result []ipcheck.Evidence
		for _, t := range previous.Classification.Types {
			stamp := previous.Classification.CheckedAt
			if t.Provider == "ippure" {
				stamp = previous.CheckedAt
			}
			result = append(result, ipcheck.StoredTypeEvidence(previous.IP, stamp, t))
		}
		return result
	}
	if previous.Classification == nil && len(previous.Attributes) == 1 {
		return []ipcheck.Evidence{ipcheck.IPPureEvidence(previous.IP, previous.Attributes, previous.CheckedAt)}
	}
	return nil
}
