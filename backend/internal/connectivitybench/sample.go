package connectivitybench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

type Candidate struct {
	Identity    string      `json:"identity"`
	Node        domain.Node `json:"node"`
	Retired     bool        `json:"retired"`
	Residential bool        `json:"residential"`
	Recent      bool        `json:"recent"`
	Due         bool        `json:"due"`
}
type Sample struct {
	CapturedAt  time.Time   `json:"captured_at"`
	Targets     []Candidate `json:"targets"`
	References  []Candidate `json:"references"`
	Residential []Candidate `json:"residential"`
}

func Token(identity string) string {
	h := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(h[:12])
}

// ReadCandidates opens no adapter and runs no migrations. Both connection
// startup and this transaction enforce read-only mode. The connection is closed
// before any probes start. No database handle is exposed to the executor.
func ReadCandidates(ctx context.Context) ([]Candidate, error) {
	cfg, err := readOnlyConfig()
	if err != nil {
		return nil, fmt.Errorf("invalid database environment")
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("read-only database connection failed")
	}
	defer conn.Close(context.Background())
	return readCandidates(ctx, conn)
}

func readOnlyConfig() (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "25000"
	cfg.RuntimeParams["application_name"] = "connectivity-bench-readonly"
	// PGOPTIONS is also processed by the server. Append the enforced setting so
	// an inherited '-c default_transaction_read_only=off' cannot defeat startup.
	cfg.RuntimeParams["options"] += " -c default_transaction_read_only=on"
	return cfg, nil
}
func readCandidates(ctx context.Context, conn *pgx.Conn) ([]Candidate, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	var readOnly string
	if err = tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
		return nil, fmt.Errorf("database read-only guard failed")
	}
	rows, err := tx.Query(ctx, `WITH due AS (SELECT DISTINCT t.identity FROM node_explorer_work w
 JOIN node_explorer_retired_nodes t ON t.identity=w.payload->>'identity'
 AND t.retired_at=w.payload->>'retiredAt' AND t.reason=w.payload->>'reason'
	 WHERE w.kind IN ('normal-recovery','recovery') AND w.state<>'done' AND w.due_at<=now())
 SELECT n.identity,n.node,t.identity IS NOT NULL,n.historical_residential,
 coalesce(n.available AND t.identity IS NULL AND c.payload->>'reachable'='true' AND c.checked_at>now()-interval '30 minutes',false),
	 d.identity IS NOT NULL AND n.available
 FROM node_explorer_node_registry n LEFT JOIN node_explorer_retired_nodes t USING(identity)
 LEFT JOIN node_explorer_connectivity c USING(identity) LEFT JOIN due d USING(identity)
 WHERE EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`)
	if err != nil {
		return nil, fmt.Errorf("candidate query failed: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var raw []byte
		if err = rows.Scan(&c.Identity, &raw, &c.Retired, &c.Residential, &c.Recent, &c.Due); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &c.Node); err != nil {
			return nil, fmt.Errorf("invalid stored node")
		}
		out = append(out, c)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, tx.Commit(ctx)
}

// Stratified uses proportional largest-remainder quotas, then stable hashed
// ordering within each protocol. Input row order cannot change the sample.
func Stratified(in []Candidate, limit int) []Candidate {
	groups := map[string][]Candidate{}
	for _, c := range in {
		k := strings.ToLower(c.Node.Type)
		groups[k] = append(groups[k], c)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	limit = min(limit, len(in))
	if limit <= 0 {
		return nil
	}
	quotas := map[string]int{}
	remaining := limit
	for _, k := range keys {
		quotas[k] = len(groups[k]) * limit / len(in)
		remaining -= quotas[k]
	}
	remainder := append([]string(nil), keys...)
	sort.SliceStable(remainder, func(i, j int) bool {
		return len(groups[remainder[i]])*limit%len(in) > len(groups[remainder[j]])*limit%len(in)
	})
	for i := 0; i < remaining; i++ {
		quotas[remainder[i]]++
	}
	var out []Candidate
	for _, k := range keys {
		g := groups[k]
		sort.Slice(g, func(i, j int) bool { return Token(g[i].Identity) < Token(g[j].Identity) })
		out = append(out, g[:quotas[k]]...)
	}
	// Interleave protocol groups deterministically across waves.
	sort.Slice(out, func(i, j int) bool { return Token(out[i].Identity) < Token(out[j].Identity) })
	return out
}
func MakeSample(all []Candidate, due bool) Sample {
	var recent, retired, residential, targets []Candidate
	for _, c := range all {
		if c.Recent {
			recent = append(recent, c)
			if c.Residential {
				residential = append(residential, c)
			}
		}
		if c.Retired {
			retired = append(retired, c)
		}
		if c.Due {
			targets = append(targets, c)
		}
	}
	refs := Stratified(recent, 32)
	if !due {
		targets = append(Stratified(recent, 128), Stratified(retired, 384)...)
	} else {
		targets = Stratified(targets, len(targets))
	}
	return Sample{CapturedAt: time.Now().UTC(), Targets: targets, References: refs, Residential: Stratified(residential, 16)}
}
