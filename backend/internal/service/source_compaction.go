package service

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Historical batches are ingestion envelopes, not a second node history store.
// Keep only the winning occurrence of each connection; runtime facts stay keyed
// by identity. This runs only when source revisions change, in the same transaction.
func compactRegistrySources(ctx context.Context, tx pgx.Tx) error {
	rows, e := tx.Query(ctx, `SELECT DISTINCT m.snapshot_id FROM node_explorer_snapshot_members m
 LEFT JOIN node_explorer_node_registry n ON n.identity=m.identity
 WHERE n.identity IS NULL OR n.snapshot_id<>m.snapshot_id OR n.node_index<>m.node_index ORDER BY m.snapshot_id`)
	if e != nil {
		return e
	}
	var ids []int
	for rows.Next() {
		var id int
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	for _, id := range ids {
		var indices []int
		e = tx.QueryRow(ctx, `SELECT coalesce(array_agg(m.node_index ORDER BY m.node_index),'{}'::int[])
 FROM node_explorer_snapshot_members m JOIN node_explorer_node_registry n
 ON n.identity=m.identity AND n.snapshot_id=m.snapshot_id AND n.node_index=m.node_index WHERE m.snapshot_id=$1`, id).Scan(&indices)
		if e != nil {
			return e
		}
		if e = remapSnapshotIndices(ctx, tx, id, indices); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE node_explorer_snapshots s SET node_count=cardinality($2::int[]),
 payload=jsonb_set(s.payload::jsonb,'{nodes}',coalesce((SELECT jsonb_agg(j.node ORDER BY j.idx)
 FROM jsonb_array_elements(s.payload::jsonb->'nodes') WITH ORDINALITY j(node,idx)
 WHERE (j.idx-1)=ANY($2::int[])),'[]'::jsonb))::text WHERE s.id=$1`, id, indices)
		if e != nil {
			return e
		}
	}
	return nil
}

func remapSnapshotIndices(ctx context.Context, tx pgx.Tx, id int, indices []int) error {
	for _, q := range []string{
		`DELETE FROM node_explorer_ippure_results WHERE snapshot_id=$1 AND NOT(node_index=ANY($2::int[]))`,
		`UPDATE node_explorer_ippure_results SET node_index=-node_index-1 WHERE snapshot_id=$1`,
		`UPDATE node_explorer_ippure_results e SET node_index=m.new_index-1 FROM unnest($2::int[]) WITH ORDINALITY m(old_index,new_index) WHERE e.snapshot_id=$1 AND e.node_index=-m.old_index-1`,
		`DELETE FROM node_explorer_snapshot_members WHERE snapshot_id=$1 AND NOT(node_index=ANY($2::int[]))`,
		`UPDATE node_explorer_snapshot_members SET node_index=-node_index-1 WHERE snapshot_id=$1`,
		`UPDATE node_explorer_snapshot_members s SET node_index=m.new_index-1 FROM unnest($2::int[]) WITH ORDINALITY m(old_index,new_index) WHERE s.snapshot_id=$1 AND s.node_index=-m.old_index-1`,
		`UPDATE node_explorer_node_registry n SET node_index=m.new_index-1 FROM unnest($2::int[]) WITH ORDINALITY m(old_index,new_index) WHERE n.snapshot_id=$1 AND n.node_index=m.old_index`,
	} {
		// Queries without $2 must not receive an extra bind parameter.
		var e error
		if !strings.Contains(q, "$2") {
			_, e = tx.Exec(ctx, q, id)
		} else {
			_, e = tx.Exec(ctx, q, id, indices)
		}
		if e != nil {
			return e
		}
	}
	return nil
}
