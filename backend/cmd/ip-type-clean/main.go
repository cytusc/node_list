// Command ip-type-clean refreshes every distinct stored exit IP, including retired
// and historical records. It preserves probe metadata and backs up each changed row.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"time"

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
	only := flag.String("ip", "", "limit to one exit IP")
	output := flag.String("output", "ip-type-clean.jsonl", "append-only evidence and progress report (0600)")
	flag.Parse()
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
	}
	runID := time.Now().UTC().Format("20060102T150405.000000000Z")
	fmt.Printf("run=%s apply=%t records=%d uniqueIPs=%d invalidIPs=%d\n", runID, *apply, records, len(ips), invalid)
	client := ipcheck.NewClient(os.Getenv("IPINFO_TOKEN"), os.Getenv("IP_API_PRO_KEY"), os.Getenv("IPREGISTRY_API_KEY"), os.Getenv("ABUSEIPDB_API_KEY"))
	var total int64
	missing := 0
	for i, ip := range ips {
		if err := ctx.Err(); err != nil {
			return err
		}
		previous := byIP[ip]
		now := time.Now()
		var ev []ipcheck.Evidence
		if *refresh {
			ev = client.Lookup(ctx, ip)
		}
		// Preserve unexpired source evidence when the public API is unavailable.
		// Never reinterpret merged attributes as the IPPure source.
		var prior []ipcheck.Evidence
		if previous.Classification != nil && previous.Classification.IP == ip {
			prior = previous.Classification.Evidence
			if len(prior) == 0 {
				for _, t := range previous.Classification.Types {
					stamp := previous.Classification.CheckedAt
					if t.Provider == "ippure" {
						stamp = previous.CheckedAt
					}
					prior = append(prior, ipcheck.StoredTypeEvidence(ip, stamp, t))
				}
			}
		} else if previous.Classification == nil && len(previous.Attributes) == 1 {
			prior = append(prior, ipcheck.IPPureEvidence(ip, previous.Attributes, previous.CheckedAt))
		}
		// Current explicit evidence replaces the same provider. Failed requests
		// do not erase a still-valid successful result.
		for _, old := range prior {
			current := ipcheck.Score(ip, ev, now)
			usable := false
			for _, t := range current.Types {
				if t.Provider == old.Provider {
					usable = true
					break
				}
			}
			if !usable {
				for j := len(ev) - 1; j >= 0; j-- {
					if ev[j].Provider == old.Provider {
						ev = append(ev[:j], ev[j+1:]...)
					}
				}
				ev = append(ev, old)
			}
		}
		result := ipcheck.Score(ip, ev, now)
		e := entry{IP: ip, Evidence: ev, Classification: result, Attributes: result.Attributes(nil)}
		available := map[string]bool{}
		for _, t := range result.Types {
			available[t.Provider] = true
		}
		for _, provider := range []string{"ippure", "ipinfo", "ip-api", "ipregistry", "abuseipdb"} {
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
