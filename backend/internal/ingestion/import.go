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

func (i *Importer) Import(ctx context.Context, snap domain.Snapshot) error {
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

// Save commits nodes, source occurrences and URI evidence in one transaction.
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
		if existing == "complete" {
			if _, err = tx.Exec(ctx, `UPDATE node_explorer_source_occurrences SET current=(digest=$2) WHERE source=$1`, source, digest); err != nil {
				return err
			}
		}
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
	if status == "complete" {
		if _, err = tx.Exec(ctx, `UPDATE node_explorer_source_occurrences SET current=false WHERE source=$1 AND current`, source); err != nil {
			return err
		}
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
	snapshotID := 0
	if len(nodes) > 0 {
		payload, err := json.Marshal(domain.Snapshot{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Nodes: nodes})
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO node_explorer_snapshots(fingerprint,checked_at,imported_at,node_count,payload) VALUES($1,$2,$2,$3,$4) RETURNING id`, "source-v1:"+source+":"+digest, time.Now().UTC().Format(time.RFC3339Nano), len(nodes), string(payload)).Scan(&snapshotID)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_batches(source,digest,snapshot_id,version,status,total,accepted,rejected) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, source, digest, snapshotID, result.Version, status, result.Total, len(nodes), len(result.Rejected))
	if err != nil {
		return err
	}
	for index, n := range result.Nodes {
		id := "src1:" + n.Fingerprint
		var canonical string
		err = tx.QueryRow(ctx, `SELECT identity,canonical FROM node_explorer_source_connections WHERE version=$1 AND fingerprint=$2`, result.Version, n.Fingerprint).Scan(&id, &canonical)
		if err == nil && canonical != n.Canonical {
			return errors.New("connection fingerprint collision")
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// Adopt only one byte-equivalent strict-config legacy record. Never merge legacy rows.
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
					candidates = append(candidates, candidate)
				}
				rows.Close()
				if e = rows.Err(); e != nil {
					return e
				}
				if len(candidates) == 1 {
					id = candidates[0]
				}
			}
			payload, e := json.Marshal(nodes[index])
			if e != nil {
				return e
			}
			_, err = tx.Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index,available) VALUES($1,$2,$3,$4,false) ON CONFLICT(identity) DO NOTHING`, id, payload, snapshotID, index)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_connections(version,fingerprint,canonical,identity) VALUES($1,$2,$3,$4)`, result.Version, n.Fingerprint, n.Canonical, id)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(id, "src1:") {
				if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_identity_aliases VALUES($1,$2,$3,$1) ON CONFLICT DO NOTHING`, id, result.Version, n.Fingerprint); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		}
		// Keep the first complete config and URI. A config-only repeat cannot erase an original.
		_, err = tx.Exec(ctx, `UPDATE node_explorer_node_registry SET node=CASE WHEN coalesce(node->>'rawLink','')='' AND $2<>'' THEN node || jsonb_build_object('rawLink',$2::text) ELSE node END WHERE identity=$1`, id, n.RawLink)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_snapshot_members(snapshot_id,node_index,identity) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, snapshotID, index, id); err != nil {
			return err
		}
		// Repair only a missing reader location, preserving every valid existing location.
		if _, err = tx.Exec(ctx, `UPDATE node_explorer_node_registry r SET snapshot_id=$2,node_index=$3 WHERE identity=$1 AND NOT EXISTS(SELECT 1 FROM node_explorer_snapshot_members m WHERE m.snapshot_id=r.snapshot_id AND m.node_index=r.node_index AND m.identity=r.identity)`, id, snapshotID, index); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_occurrences(source,digest,position,identity,name) VALUES($1,$2,$3,$4,$5)`, source, digest, n.Index, id, n.Name); err != nil {
			return err
		}
		for _, link := range []struct{ value, origin, status string }{{n.OriginalEvidence, "original", map[bool]string{true: "conflict", false: n.LinkStatus}[n.Conflict]}, {n.GeneratedLink, "generated", n.LinkStatus}} {
			if link.value != "" {
				if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_links(source,digest,position,link,origin,status,version) VALUES($1,$2,$3,$4,$5,$6,$7)`, source, digest, n.Index, link.value, link.origin, link.status, result.Version); err != nil {
					return err
				}
			}
		}
	}
	for _, rejected := range result.Rejected {
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_source_rejections VALUES($1,$2,$3,$4)`, source, digest, rejected.Index, rejected.Code); err != nil {
			return err
		}
	}
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
