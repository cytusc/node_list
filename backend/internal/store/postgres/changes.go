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
	var built, retiredRevision int64
	if err := tx.QueryRow(ctx, `SELECT source_revision, built_revision, retired_revision FROM `+tableState).Scan(&c.Revision, &built, &retiredRevision); err != nil {
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
	// Older versions removed entries after publishing. A cursor behind that
	// boundary must reload once, rather than silently missing those changes.
	c.Full = c.Full || since < built
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
