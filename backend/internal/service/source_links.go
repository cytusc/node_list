package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Recover original strings still present in a subscription, including nodes
// outside the latest detection round. Full config/credential matching is required:
// matching only IP/port or display name can attach somebody else's credentials.
func (r *Repository) recoverSourceLinks(ctx context.Context, sources *domain.Sources) (int, error) {
	if sources == nil || len(sources.Links) == 0 {
		return 0, nil
	}
	var links []string
	for _, link := range sources.Links {
		links = append(links, link)
	}
	sort.Strings(links)
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); err != nil {
		return 0, err
	}
	// Save every upstream URI before attempting conversion or matching.
	// Unsupported future protocols and ambiguous entries must remain recoverable;
	// an unassigned archive key never authorizes attaching credentials to a node.
	archive := &pgx.Batch{}
	for _, link := range links {
		key := domain.ConnectionKey(domain.Node{RawLink: link})
		if key == "" {
			key = fmt.Sprintf("unmatched:%x", sha256.Sum256([]byte(link)))
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key+"\x00original\x00"+link)))
		archive.Queue(`INSERT INTO node_explorer_node_links(id,connection_key,link,origin) VALUES($1,$2,$3,'original') ON CONFLICT(id) DO NOTHING`, digest, key, link)
	}
	if err = tx.SendBatch(ctx, archive).Close(); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id,payload FROM node_explorer_snapshots WHERE node_count>0 ORDER BY id FOR UPDATE`)
	if err != nil {
		return 0, err
	}
	type repair struct {
		id      int
		payload []byte
	}
	var repairs []repair
	restored := 0
	for rows.Next() {
		var id int
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		var snap domain.Snapshot
		if err = json.Unmarshal(raw, &snap); err != nil {
			rows.Close()
			return 0, err
		}
		changed := false
		for i, n := range snap.Nodes {
			if n.RawLink != "" || len(n.RawConfig) == 0 {
				continue
			}
			for _, link := range links {
				if domain.LinkMatchesConfig(link, n.RawConfig) {
					snap.Nodes[i].RawLink = link
					changed = true
					restored++
					break
				}
			}
		}
		if changed {
			payload, e := json.Marshal(snap)
			if e != nil {
				rows.Close()
				return 0, e
			}
			repairs = append(repairs, repair{id, payload})
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, v := range repairs {
		if _, err = tx.Exec(ctx, `UPDATE node_explorer_snapshots SET payload=$2 WHERE id=$1`, v.id, string(v.payload)); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return restored, nil
}
