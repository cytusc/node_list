package postgres

import (
	"context"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

func (a *Adapter) ReadChanges(ctx context.Context, since int64) (store.Changes, error) {
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return store.Changes{}, err
	}
	defer tx.Rollback(ctx)
	return readChanges(ctx, tx, since)
}

func readChanges(ctx context.Context, tx pgx.Tx, since int64) (store.Changes, error) {
	c := store.Changes{Full: since < 0}
	var retiredRevision int64
	if err := tx.QueryRow(ctx, `SELECT source_revision, retired_revision FROM `+tableState).Scan(&c.Revision, &retiredRevision); err != nil {
		return c, err
	}
	if c.Revision == since {
		return c, nil
	}
	var runtimeErr error
	c.Runtime, runtimeErr = queryRuntime(ctx, tx)
	if runtimeErr != nil {
		return c, runtimeErr
	}
	// Per-batch versions survive publication. Another reader advancing the
	// projection must not turn this reader's incremental sync into a full load.
	if !c.Full {
		c.SnapshotIDs = make([]int, 0)
		rows, err := tx.Query(ctx, `SELECT snapshot_id FROM `+tableChanges+` WHERE revision > $1`, since)
		if err != nil {
			return c, err
		}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return c, err
			}
			c.SnapshotIDs = append(c.SnapshotIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return c, err
		}
	}
	var err error
	c.Batches, err = queryBatches(ctx, tx, c.SnapshotIDs)
	if err != nil {
		return c, err
	}
	c.Enrichments, err = queryEnrichments(ctx, tx, c.SnapshotIDs)
	if err != nil {
		return c, err
	}
	c.RetiredChanged = c.Full || retiredRevision > since
	if c.RetiredChanged {
		c.Retired, err = queryRetired(ctx, tx)
	}
	return c, err
}
