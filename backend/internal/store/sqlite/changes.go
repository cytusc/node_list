package sqlite

import (
	"context"
	"fmt"
	"github.com/cytusc/node_list/backend/internal/store"
)

func (a *Adapter) initChanges() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS cache_state (singleton INTEGER PRIMARY KEY CHECK(singleton=1), revision INTEGER NOT NULL, retired_revision INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO cache_state VALUES (1, 0, 0)`,
		`CREATE TABLE IF NOT EXISTS cache_changes (snapshot_id INTEGER PRIMARY KEY, revision INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS cache_changes_revision ON cache_changes(revision)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	for _, table := range []string{"snapshots", "ippure_results", "retired_nodes"} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			body := `UPDATE cache_state SET revision = revision + 1;`
			if table == "retired_nodes" {
				body += `UPDATE cache_state SET retired_revision = revision;`
			} else {
				ref := "NEW."
				if op == "DELETE" {
					ref = "OLD."
				}
				column := "snapshot_id"
				if table == "snapshots" {
					column = "id"
				}
				body += `INSERT INTO cache_changes(snapshot_id, revision) SELECT ` + ref + column + `, revision FROM cache_state WHERE singleton = 1 ON CONFLICT(snapshot_id) DO UPDATE SET revision=excluded.revision;`
			}
			stmt := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS cache_%s_%s AFTER %s ON %s BEGIN %s END`, table, op, op, table, body)
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ReadChanges uses a read transaction so payloads and versions describe exactly
// the same committed generation, including writes from the other process.
func (a *Adapter) ReadChanges(ctx context.Context, since int64) (store.Changes, error) {
	c := store.Changes{Full: since < 0}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer tx.Rollback()
	var retiredRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision, retired_revision FROM cache_state WHERE singleton=1`).Scan(&c.Revision, &retiredRevision); err != nil {
		return c, err
	}
	if c.Revision == since {
		return c, nil
	}
	args := []any{}
	batchWhere, enrichWhere := "", ""
	if !c.Full {
		args = append(args, since)
		batchWhere = ` WHERE id IN (SELECT snapshot_id FROM cache_changes WHERE revision > ?)`
		enrichWhere = ` WHERE snapshot_id IN (SELECT snapshot_id FROM cache_changes WHERE revision > ?)`
		rows, err := tx.QueryContext(ctx, `SELECT snapshot_id FROM cache_changes WHERE revision > ?`, since)
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
		err = rows.Err()
		rows.Close()
		if err != nil {
			return c, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, imported_at, payload FROM snapshots`+batchWhere, args...)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var r store.SnapshotRow
		if err := rows.Scan(&r.ID, &r.ImportedAt, &r.Payload); err != nil {
			rows.Close()
			return c, err
		}
		c.Batches = append(c.Batches, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT snapshot_id, node_index, payload FROM ippure_results`+enrichWhere, args...)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var r store.Enrichment
		if err := rows.Scan(&r.SnapshotID, &r.NodeIndex, &r.Payload); err != nil {
			rows.Close()
			return c, err
		}
		c.Enrichments = append(c.Enrichments, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, err
	}
	c.RetiredChanged = c.Full || retiredRevision > since
	if c.RetiredChanged {
		rows, err = tx.QueryContext(ctx, `SELECT identity, retired_at, reason FROM retired_nodes`)
		if err != nil {
			return c, err
		}
		for rows.Next() {
			var r store.RetiredRow
			if err := rows.Scan(&r.Identity, &r.RetiredAt, &r.Reason); err != nil {
				rows.Close()
				return c, err
			}
			c.Retired = append(c.Retired, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return c, err
		}
	}
	return c, tx.Commit()
}
