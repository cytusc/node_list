package service

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

type registryNode struct {
	Identity   string      `json:"identity"`
	Node       domain.Node `json:"node"`
	SnapshotID int         `json:"snapshot_id"`
	Index      int         `json:"node_index"`
	Historical bool        `json:"historical_residential"`
	SourceAt   time.Time   `json:"source_at"`
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
			identity := domain.AggregateIdentity(n)
			if identity == "" {
				continue
			}
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
			nodes[identity] = registryNode{identity, n, id, index, historical || old.Historical, stamp}
		}
		markers = append(markers, marker{id, revision})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
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
		_, err = tx.Exec(ctx, `INSERT INTO node_explorer_node_registry AS current(identity,node,snapshot_id,node_index,historical_residential,source_at)
 SELECT x.identity,x.node,x.snapshot_id,x.node_index,x.historical_residential,x.source_at
 FROM jsonb_to_recordset($1::jsonb) AS x(identity text,node jsonb,snapshot_id integer,node_index integer,historical_residential boolean,source_at timestamptz)
 LEFT JOIN node_explorer_node_registry old USING(identity)
 WHERE old.identity IS NULL OR ($2 OR x.source_at>old.source_at OR (x.source_at=old.source_at AND x.snapshot_id>=old.snapshot_id))
 AND (NOT old.available OR old.node<>x.node OR old.snapshot_id<>x.snapshot_id OR old.node_index<>x.node_index OR old.source_at<>x.source_at OR (x.historical_residential AND NOT old.historical_residential))
 ON CONFLICT(identity) DO UPDATE SET node=EXCLUDED.node,snapshot_id=EXCLUDED.snapshot_id,node_index=EXCLUDED.node_index,
 available=true,source_at=EXCLUDED.source_at,historical_residential=current.historical_residential OR EXCLUDED.historical_residential`, raw, full)
		if err != nil {
			return err
		}
	}
	if full {
		if _, err = tx.Exec(ctx, `UPDATE node_explorer_node_registry SET available=false WHERE available AND NOT(identity=ANY($1::text[]))`, ids); err != nil {
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
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	r.registryVersion, r.registryReady = version, true
	return nil
}
