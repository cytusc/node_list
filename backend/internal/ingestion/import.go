package ingestion

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string
var ErrPartialBatch = errors.New("source batch partially accepted")
var ErrNoAdmissibleNodes = errors.New("source contains no representable nodes")

func (r *Repository) Prepare(ctx context.Context) error {
	_, err := r.Pool.Exec(ctx, schemaSQL)
	return err
}

type Importer struct {
	Repository *Repository
	Converter  Converter
	Source     string
}
type sourceItem struct {
	Name      string             `json:"name,omitempty"`
	RawLink   string             `json:"rawLink,omitempty"`
	RawConfig domain.ProxyConfig `json:"rawConfig,omitempty"`
}

func (i *Importer) Import(ctx context.Context, snap domain.SourceBatch) error {
	items := make([]sourceItem, 0, len(snap.Nodes))
	for _, n := range snap.Nodes {
		items = append(items, sourceItem{Name: n.Name, RawLink: n.RawLink, RawConfig: n.RawConfig})
	}
	return i.request(ctx, map[string]any{"items": items})
}
func (i *Importer) ImportText(ctx context.Context, text string) error {
	return i.request(ctx, map[string]any{"text": text})
}
func (i *Importer) request(ctx context.Context, request any) error {
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > maxInput {
		return errors.New("source input invalid or too large")
	}
	converted, err := i.Converter.Convert(ctx, request)
	if err != nil {
		return err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(append([]byte(converterVersion+"\x00"), raw...)))
	return i.Repository.Save(ctx, i.Source, digest, converted)
}

// Save commits nodes and URI evidence in one transaction.
// It never mutates availability, incarnation, exit, attributes or work tables.
func (r *Repository) Save(ctx context.Context, source, digest string, result Conversion) error {
	if source == "" || len(source) > 100 || result.Version != converterVersion || result.Total < 0 || result.Total > 20000 || len(result.Nodes)+len(result.Rejected) != result.Total {
		return errors.New("invalid source batch")
	}
	positions := map[int]bool{}
	for _, n := range result.Nodes {
		positions[n.Index] = true
	}
	if len(positions) != len(result.Nodes) {
		return errors.New("duplicate converted position")
	}
	for _, rejected := range result.Rejected {
		if rejected.Index < 0 || rejected.Index >= result.Total || positions[rejected.Index] {
			return errors.New("invalid rejection position")
		}
		switch rejected.Code {
		case "invalid", "incomplete", "unsupported", "unrepresentable":
		default:
			return errors.New("invalid rejection code")
		}
		positions[rejected.Index] = true
	}
	// Public source lists can contain tens of thousands of nodes. Saving their
	// records is intentionally transactional, so allow enough time for the full
	// batch instead of rolling it back while the per-node rows are persisted.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout = '5min'`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('source-ingest-write'))`); err != nil {
		return err
	}
	status := "complete"
	if len(result.Rejected) > 0 {
		status = "partial"
	}
	if len(result.Nodes) == 0 {
		status = "rejected"
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT status FROM node_explorer_source_batches WHERE source=$1 AND digest=$2`, source, digest).Scan(&existing)
	if err == nil {
		if _, err = tx.Exec(ctx, `UPDATE node_explorer_source_batches SET fetched_at=now() WHERE source=$1 AND digest=$2`, source, digest); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		if existing == "rejected" {
			return ErrNoAdmissibleNodes
		}
		if existing == "partial" {
			return ErrPartialBatch
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	nodes := make([]domain.Node, len(result.Nodes))
	seen := map[int]bool{}
	for index, n := range result.Nodes {
		if n.Index < 0 || n.Index >= result.Total || seen[n.Index] || len(n.Fingerprint) != 64 || n.RawLink == "" && n.GeneratedLink == "" {
			return errors.New("invalid converted node")
		}
		seen[n.Index] = true
		sum := sha256.Sum256([]byte(converterVersion + "\x00" + n.Canonical))
		if hex.EncodeToString(sum[:]) != n.Fingerprint {
			return errors.New("invalid connection fingerprint")
		}
		node := domain.SummarizeProxy(n.Config)
		node.RawConfig = n.Config
		node.RawLink = n.RawLink
		node.GeneratedLink = n.GeneratedLink
		node.Name = n.Name
		node.SubTag = source
		nodes[index] = node
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_batches(source,digest,version,status,total,accepted,rejected) VALUES($1,$2,$3,$4,$5,$6,$7)`, source, digest, result.Version, status, result.Total, len(nodes), len(result.Rejected))
	if err != nil {
		return err
	}
	// Resolve identities and persist the batch with a few multi-row
	// statements instead of one query per node. A batch used to issue up
	// to five statements per node (connections lookup, legacy adoption,
	// registry insert, connections insert, rawLink update, occurrence
	// insert); it now issues one connections lookup, one adoption lookup
	// per genuinely new fingerprint, and one multi-row statement per
	// table.
	fps := make([]string, len(result.Nodes))
	for i, n := range result.Nodes {
		fps[i] = n.Fingerprint
	}
	type knownConn struct {
		identity, canonical string
	}
	known := map[string]knownConn{}
	connRows, err := tx.Query(ctx, `SELECT fingerprint,identity,canonical FROM node_explorer_source_connections WHERE version=$1 AND fingerprint=ANY($2)`, result.Version, fps)
	if err != nil {
		return err
	}
	for connRows.Next() {
		var fp, id, canonical string
		if err = connRows.Scan(&fp, &id, &canonical); err != nil {
			connRows.Close()
			return err
		}
		known[fp] = knownConn{id, canonical}
	}
	connRows.Close()
	if err = connRows.Err(); err != nil {
		return err
	}
	type resolved struct {
		id          string
		preExisting bool
	}
	byFP := map[string]resolved{}
	adopted := map[string]bool{}
	type newFP struct {
		fingerprint, canonical, identity string
		payload                          []byte
	}
	var fresh []newFP
	rawLinkSeen := map[string]bool{}
	var rawLinks [][2]string
	for index, n := range result.Nodes {
		r, ok := byFP[n.Fingerprint]
		if !ok {
			r.id = "src1:" + n.Fingerprint
			if k, knownFP := known[n.Fingerprint]; knownFP {
				if k.canonical != n.Canonical {
					return errors.New("connection fingerprint collision")
				}
				r.id = k.identity
				r.preExisting = true
			} else {
				// Adopt only one byte-equivalent strict-config legacy
				// record. Never merge legacy rows.
				var material map[string]json.RawMessage
				if json.Unmarshal([]byte(n.Canonical), &material) != nil {
					return errors.New("invalid canonical configuration")
				}
				if _, opaque := material["uri"]; !opaque {
					rows, e := tx.Query(ctx, `SELECT r.identity FROM node_explorer_node_registry r WHERE md5(((r.node->'rawConfig')-'name'-'sub_tag')::text)=md5($1::jsonb::text) AND (r.node->'rawConfig')-'name'-'sub_tag'=$1::jsonb AND NOT EXISTS(SELECT 1 FROM node_explorer_source_connections c WHERE c.identity=r.identity) ORDER BY r.identity LIMIT 2`, string(material["config"]))
					if e != nil {
						return e
					}
					var candidates []string
					for rows.Next() {
						var candidate string
						if e = rows.Scan(&candidate); e != nil {
							rows.Close()
							return e
						}
						// The connections for this batch are not inserted
						// yet, so a legacy identity adopted by an earlier
						// fingerprint in this batch is still invisible to
						// the NOT EXISTS check; exclude it here.
						if !adopted[candidate] {
							candidates = append(candidates, candidate)
						}
					}
					rows.Close()
					if e = rows.Err(); e != nil {
						return e
					}
					if len(candidates) == 1 {
						r.id = candidates[0]
						r.preExisting = true
						adopted[r.id] = true
					}
				}
				payload, e := json.Marshal(nodes[index])
				if e != nil {
					return e
				}
				fresh = append(fresh, newFP{n.Fingerprint, n.Canonical, r.id, payload})
			}
			byFP[n.Fingerprint] = r
		}
		// Only touch the registry row when the raw link is actually being
		// added; the previous form rewrote every node's row on every new
		// batch even when nothing changed. A fresh src1 identity is
		// inserted with the raw link already in its payload, so it needs
		// no update.
		if n.RawLink != "" && r.preExisting && !rawLinkSeen[r.id] {
			rawLinkSeen[r.id] = true
			rawLinks = append(rawLinks, [2]string{r.id, n.RawLink})
		}
	}
	// Registry rows must exist before connections and occurrences reference them.
	if len(fresh) > 0 {
		var sb strings.Builder
		sb.WriteString(`INSERT INTO node_explorer_node_registry(identity,node,available) VALUES `)
		args := make([]any, 0, len(fresh)*2)
		for i, f := range fresh {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d,$%d,false)", i*2+1, i*2+2)
			args = append(args, f.identity, f.payload)
		}
		sb.WriteString(` ON CONFLICT(identity) DO NOTHING`)
		if _, err = tx.Exec(ctx, sb.String(), args...); err != nil {
			return err
		}
		var sb2 strings.Builder
		sb2.WriteString(`INSERT INTO node_explorer_source_connections(version,fingerprint,canonical,identity) VALUES `)
		args2 := make([]any, 0, len(fresh)*4)
		for i, f := range fresh {
			if i > 0 {
				sb2.WriteString(",")
			}
			fmt.Fprintf(&sb2, "($%d,$%d,$%d,$%d)", i*4+1, i*4+2, i*4+3, i*4+4)
			args2 = append(args2, result.Version, f.fingerprint, f.canonical, f.identity)
		}
		if _, err = tx.Exec(ctx, sb2.String(), args2...); err != nil {
			return err
		}
	}
	if len(rawLinks) > 0 {
		var sb strings.Builder
		sb.WriteString(`UPDATE node_explorer_node_registry r SET node=r.node || jsonb_build_object('rawLink',v.rawlink) FROM (VALUES `)
		args := make([]any, 0, len(rawLinks)*2)
		for i, rl := range rawLinks {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "($%d,$%d)", i*2+1, i*2+2)
			args = append(args, rl[0], rl[1])
		}
		sb.WriteString(`) AS v(identity,rawlink) WHERE r.identity=v.identity AND coalesce(r.node->>'rawLink','')='' AND v.rawlink<>''`)
		if _, err = tx.Exec(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	// Source occurrences are no longer written: nothing reads
	// node_explorer_source_occurrences (no Go code, no PHP, no SQL view
	// uses it), and each batch wrote one row per node. The table and its
	// existing rows are kept.
	// Source links are no longer written: nothing reads
	// node_explorer_source_links (it was link history only), and each
	// batch wrote up to two rows per node. The table and its existing
	// rows are kept.
	// Rejections are no longer recorded: nothing reads
	// node_explorer_source_rejections, and the rejection counts are already
	// kept on the batch row.
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if len(nodes) == 0 {
		return ErrNoAdmissibleNodes
	}
	if status == "partial" {
		return ErrPartialBatch
	}
	return nil
}
