package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

type registryNode struct {
	Identity      string      `json:"identity"`
	ConnectionKey string      `json:"connection_key"`
	Node          domain.Node `json:"node"`
	SnapshotID    int         `json:"snapshot_id"`
	Index         int         `json:"node_index"`
	Historical    bool        `json:"historical_residential"`
	SourceAt      time.Time   `json:"source_at"`
}

// Only changed source batches are decoded. Runtime observations never cause
// another source fold. A persisted per-batch cursor also survives restarts.
func (r *Repository) syncRegistry(ctx context.Context) error {
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	var version int64
	if err := r.Pool.QueryRow(ctx, `SELECT coalesce(max(revision),0) FROM node_explorer_results_changes`).Scan(&version); err != nil {
		return err
	}
	if r.registryReady && version == r.registryVersion {
		return nil
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-registry'))`); err != nil {
		return err
	}
	var full bool
	// A rewritten/deleted old batch can remove the winning occurrence of a
	// connection. Reconcile all remaining sources in that uncommon case only.
	if err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM node_explorer_registry_batches)
 OR EXISTS(SELECT 1 FROM node_explorer_registry_batches b LEFT JOIN node_explorer_snapshots s ON s.id=b.snapshot_id
 LEFT JOIN node_explorer_results_changes c ON c.snapshot_id=b.snapshot_id
 WHERE s.id IS NULL OR b.revision<>coalesce(c.revision,0))`).Scan(&full); err != nil {
		return err
	}
	if !full {
		var pending bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_snapshots s
		LEFT JOIN node_explorer_registry_batches b ON b.snapshot_id=s.id WHERE b.snapshot_id IS NULL)`).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			if err = tx.Commit(ctx); err != nil {
				return err
			}
			r.registryVersion, r.registryReady = version, true
			return nil
		}
	}

	// Stabilize source payloads and index compaction against imports/reconciliation.
	if _, err = tx.Exec(ctx, `LOCK TABLE node_explorer_snapshots,node_explorer_ippure_results IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	// Read originals before folding/compacting. A later config-only round
	// must not erase an earlier source URI for the same complete connection.
	originals := map[string]string{}
	oldRows, e := tx.Query(ctx, `SELECT connection_key,node->>'rawLink' FROM node_explorer_node_registry
     WHERE connection_key<>'' AND coalesce(node->>'rawLink','')<>''
     UNION ALL SELECT connection_key,link FROM node_explorer_node_links WHERE origin='original' ORDER BY 1,2`)
	if e != nil {
		return e
	}
	for oldRows.Next() {
		var key, link string
		if e = oldRows.Scan(&key, &link); e != nil {
			oldRows.Close()
			return e
		}
		if _, ok := originals[key]; !ok {
			originals[key] = link
		}
	}
	oldRows.Close()
	if e = oldRows.Err(); e != nil {
		return e
	}
	type linkRecord struct {
		ID     string `json:"id"`
		Key    string `json:"connection_key"`
		Link   string `json:"link"`
		Origin string `json:"origin"`
	}
	records := map[string]linkRecord{}
	remember := func(key, link, origin string) {
		if key == "" || link == "" {
			return
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(key+"\x00"+origin+"\x00"+link)))
		records[digest] = linkRecord{digest, key, link, origin}
	}
	for key, link := range originals {
		remember(key, link, "original")
	}
	rows, err := tx.Query(ctx, `SELECT s.id,s.imported_at,s.payload,coalesce(c.revision,0)
 FROM node_explorer_snapshots s LEFT JOIN node_explorer_results_changes c ON c.snapshot_id=s.id
 LEFT JOIN node_explorer_registry_batches b ON b.snapshot_id=s.id
 WHERE $1 OR b.snapshot_id IS NULL ORDER BY s.id DESC`, full)
	if err != nil {
		return err
	}
	nodes := map[string]registryNode{}
	type marker struct {
		id       int
		revision int64
	}
	var markers []marker
	type member struct {
		SnapshotID int    `json:"snapshot_id"`
		Index      int    `json:"node_index"`
		Identity   string `json:"identity"`
	}
	var members []member
	for rows.Next() {
		var id int
		var imported string
		var raw []byte
		var revision int64
		if err = rows.Scan(&id, &imported, &raw, &revision); err != nil {
			rows.Close()
			return err
		}
		var snap domain.Snapshot
		if err = json.Unmarshal(raw, &snap); err != nil {
			rows.Close()
			return err
		}
		stamp, e := time.Parse(time.RFC3339Nano, snap.CheckedAt)
		if e != nil {
			stamp, e = time.Parse(time.RFC3339Nano, imported)
			if e != nil {
				stamp = time.Unix(0, 0).UTC()
			}
		}
		for index, n := range snap.Nodes {
			// Identity is calculated from the unchanged upstream record.
			identity := domain.AggregateIdentity(n)
			key := domain.ConnectionKey(n)
			if n.RawLink != "" {
				archiveKey := key
				if archiveKey == "" {
					archiveKey = "identity:" + identity
				}
				remember(archiveKey, n.RawLink, "original")
				originals[key] = n.RawLink
			}
			if n.RawLink == "" && key != "" {
				n.RawLink = originals[key]
			}
			if e := domain.EnsureNodeLink(&n); e != nil {
				log.Printf("registry: share-link export unavailable: %v", e)
			}
			remember(key, n.GeneratedLink, "generated")
			if identity == "" {
				continue
			}
			members = append(members, member{id, index, identity})
			old, exists := nodes[identity]
			historical := n.IPPure != nil && n.IPPure.Classification != nil && n.IPPure.Classification.State == "residential"
			if exists && (old.SourceAt.After(stamp) || (old.SourceAt.Equal(stamp) && old.SnapshotID >= id)) {
				if historical && !old.Historical {
					old.Historical = true
					nodes[identity] = old
				}
				continue
			}
			n.SnapshotID = &id
			n.Index = index
			n.CheckedAt = snap.CheckedAt
			speed, media := snap.SpeedTest, snap.MediaCheck
			n.SpeedTest = &speed
			n.MediaCheck = &media
			n.IPPure = nil
			n.Connectivity = nil
			n.Exit = nil
			n.Lifecycle = nil
			nodes[identity] = registryNode{Identity: identity, ConnectionKey: domain.ConnectionKey(n), Node: n, SnapshotID: id, Index: index, Historical: historical || old.Historical, SourceAt: stamp}
		}
		markers = append(markers, marker{id, revision})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	// Older batches may hold the original while the newest winning record
	// carries only a generated fallback. Finish reconciliation after the scan.
	for id, entry := range nodes {
		if entry.Node.RawLink == "" && entry.ConnectionKey != "" {
			entry.Node.RawLink = originals[entry.ConnectionKey]
			nodes[id] = entry
		}
	}
	// Archive every encountered URI before selecting winners or deleting old
	// snapshot members. This table is the durable provenance record.
	archive := &pgx.Batch{}
	for _, record := range records {
		archive.Queue(`INSERT INTO node_explorer_node_links(id,connection_key,link,origin) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, record.ID, record.Key, record.Link, record.Origin)
	}
	if archive.Len() > 0 {
		if err = tx.SendBatch(ctx, archive).Close(); err != nil {
			return err
		}
	}

	memberIDs := make([]int, len(markers))
	for i, m := range markers {
		memberIDs[i] = m.id
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_snapshot_members WHERE snapshot_id=ANY($1::int[])`, memberIDs); err != nil {
		return err
	}
	for start := 0; start < len(members); start += 1000 {
		raw, e := json.Marshal(members[start:min(start+1000, len(members))])
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_snapshot_members(snapshot_id,node_index,identity)
 SELECT snapshot_id,node_index,identity FROM jsonb_to_recordset($1::jsonb) AS x(snapshot_id int,node_index int,identity text)`, raw); err != nil {
			return err
		}
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	const chunk = 250
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := make([]registryNode, 0, end-start)
		for _, id := range ids[start:end] {
			batch = append(batch, nodes[id])
		}
		raw, e := json.Marshal(batch)
		if e != nil {
			return e
		}
		// Source records start untested; only node-liveness owns availability.
		_, err = tx.Exec(ctx, `INSERT INTO node_explorer_node_registry AS current(identity,node,snapshot_id,node_index,historical_residential,source_at,connection_key,first_seen_at,available)
 SELECT x.identity,x.node,x.snapshot_id,x.node_index,x.historical_residential,x.source_at,x.connection_key,now(),false
 FROM jsonb_to_recordset($1::jsonb) AS x(identity text,node jsonb,snapshot_id integer,node_index integer,historical_residential boolean,source_at timestamptz,connection_key text)
 LEFT JOIN node_explorer_node_registry old USING(identity)
 WHERE (old.identity IS NULL OR ($2 OR x.source_at>old.source_at OR (x.source_at=old.source_at AND x.snapshot_id>=old.snapshot_id))
 AND (old.node<>x.node OR old.snapshot_id<>x.snapshot_id OR old.node_index<>x.node_index OR old.source_at<>x.source_at OR old.connection_key<>x.connection_key OR (x.historical_residential AND NOT old.historical_residential)))
 ON CONFLICT(identity) DO UPDATE SET node=CASE
 WHEN current.connection_key<>'' AND current.connection_key=EXCLUDED.connection_key
 AND coalesce(current.node->>'rawLink','')<>'' AND coalesce(EXCLUDED.node->>'rawLink','')=''
 THEN EXCLUDED.node || jsonb_build_object('rawLink',current.node->>'rawLink')
 ELSE EXCLUDED.node END,snapshot_id=EXCLUDED.snapshot_id,node_index=EXCLUDED.node_index,
 source_at=EXCLUDED.source_at,connection_key=EXCLUDED.connection_key,historical_residential=current.historical_residential OR EXCLUDED.historical_residential`, raw, full)
		if err != nil {
			return err
		}
	}
	batch := &pgx.Batch{}
	for _, m := range markers {
		batch.Queue(`INSERT INTO node_explorer_registry_batches VALUES($1,$2) ON CONFLICT(snapshot_id) DO UPDATE SET revision=EXCLUDED.revision`, m.id, m.revision)
	}
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_registry_batches b WHERE NOT EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=b.snapshot_id)`); err != nil {
		return err
	}

	if err = compactRegistrySources(ctx, tx); err != nil {
		return err
	}
	// Compaction changes source revisions too; consume those changes in this fold.
	if _, err = tx.Exec(ctx, `UPDATE node_explorer_registry_batches b SET revision=c.revision FROM node_explorer_results_changes c WHERE c.snapshot_id=b.snapshot_id AND b.revision<>c.revision`); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT coalesce(max(revision),0) FROM node_explorer_results_changes`).Scan(&version); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	r.registryVersion, r.registryReady = version, true
	return nil
}
