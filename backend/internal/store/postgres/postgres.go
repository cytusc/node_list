// Package postgres implements the store adapter and the results projection on
// PostgreSQL using pgx.
//
// The previous JavaScript service kept a pool of three connections and ran
// every projection rebuild inside a single worker thread with one more
// connection. Reads were therefore serialised behind the rebuild no matter how
// much concurrency the collector asked for. pgx multiplexes many goroutines
// over a properly sized pool, so the API and the collector no longer contend.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

// Table names. They are part of the deployed schema and must stay identical.
const (
	tableSnapshots   = "node_explorer_snapshots"
	tableEnrichments = "node_explorer_ippure_results"
	tableRetired     = "node_explorer_retired_nodes"
	tableState       = "node_explorer_results_state"
	tableChanges     = "node_explorer_results_changes"
	tableCurrent     = "node_explorer_current_nodes"
)

// lockKey scopes the projection advisory lock to this schema and projection
// version so concurrent services cannot rebuild against each other.
const lockKey = "hashtext(current_schema()), hashtext('node-explorer-results-v1')"

// Config is the PostgreSQL connection configuration.
type Config struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	PoolMax  int
}

// ConfigFromEnv reads the standard PostgreSQL environment variables.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Host:     os.Getenv("PGHOST"),
		Database: os.Getenv("PGDATABASE"),
		User:     os.Getenv("PGUSER"),
		Password: os.Getenv("PGPASSWORD"),
		Port:     5432,
		PoolMax:  0,
	}
	if cfg.Host == "" {
		return cfg, errors.New("PGHOST is not set")
	}
	if cfg.Database == "" || cfg.User == "" {
		return cfg, errors.New("PostgreSQL requires PGDATABASE, PGUSER and PGPASSWORD")
	}
	if v := os.Getenv("PGPORT"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.Port); err != nil || cfg.Port <= 0 {
			return cfg, fmt.Errorf("invalid PGPORT %q", v)
		}
	}
	if v := os.Getenv("PGPOOL_MAX"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &cfg.PoolMax); err != nil || cfg.PoolMax <= 0 {
			return cfg, fmt.Errorf("invalid PGPOOL_MAX %q", v)
		}
	}
	return cfg, nil
}

func (c Config) connString() string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s",
		quoteConnValue(c.Host), c.Port, quoteConnValue(c.Database), quoteConnValue(c.User), quoteConnValue(c.Password))
}

func quoteConnValue(value string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
}

// Adapter persists snapshots and enrichment.
type Adapter struct {
	pool *pgxpool.Pool
}

// NewAdapter connects and ensures the schema exists.
func NewAdapter(ctx context.Context, cfg Config) (*Adapter, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.connString())
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}
	maxConns := int32(cfg.PoolMax)
	if maxConns <= 0 {
		maxConns = 12
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	// A hung statement must never hold a connection forever.
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = "30000"
	poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	a := &Adapter{pool: pool}
	if err := a.initSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return a, nil
}

func (a *Adapter) initSchema(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + tableSnapshots + ` (
      id SERIAL PRIMARY KEY,
      fingerprint TEXT NOT NULL UNIQUE,
      checked_at TEXT,
      imported_at TEXT NOT NULL,
      node_count INTEGER NOT NULL,
      payload TEXT NOT NULL
    )`,
		`CREATE TABLE IF NOT EXISTS ` + tableEnrichments + ` (
      snapshot_id INTEGER NOT NULL REFERENCES ` + tableSnapshots + `(id),
      node_index INTEGER NOT NULL, payload TEXT NOT NULL,
      PRIMARY KEY (snapshot_id, node_index)
    )`,
		`CREATE TABLE IF NOT EXISTS ` + tableRetired + ` (
      identity TEXT PRIMARY KEY,
      retired_at TEXT NOT NULL,
      reason TEXT NOT NULL
    )`,
	}
	for _, stmt := range statements {
		if _, err := a.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	return (&Projector{pool: a.pool}).initSchema(ctx)
}

// Pool exposes the connection pool to the projection, which needs transaction
// control the adapter interface does not model.
func (a *Adapter) Pool() *pgxpool.Pool { return a.pool }

// Insert stores a detection batch. The fingerprint identifies the batch, but a
// content guard also catches the case where a different implementation
// serialised the same nodes into a different hash: without it, a rewrite would
// silently duplicate the whole history.
func (a *Adapter) Insert(ctx context.Context, fingerprint string, payload []byte, checkedAt string, nodeCount int) error {
	if checkedAt != "" {
		existing, err := a.latestPayloadByCheckedAt(ctx, checkedAt, nodeCount)
		if err == nil && existing != nil {
			if sameSnapshotContent(existing, payload) {
				return nil
			}
		}
	}
	_, err := a.pool.Exec(ctx,
		`INSERT INTO `+tableSnapshots+`
		(fingerprint, checked_at, imported_at, node_count, payload)
		SELECT $1, $2, $3, $4, $5
		WHERE NOT EXISTS (SELECT 1 FROM `+tableSnapshots+` WHERE fingerprint = $1)
		ON CONFLICT (fingerprint) DO NOTHING`,
		fingerprint, nullIfEmpty(checkedAt), store.NowTimestamp(),
		nodeCount, string(payload))
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

func (a *Adapter) latestPayloadByCheckedAt(ctx context.Context, checkedAt string, nodeCount int) ([]byte, error) {
	var payload []byte
	err := a.pool.QueryRow(ctx,
		`SELECT payload FROM `+tableSnapshots+`
		WHERE checked_at = $1 AND node_count = $2 ORDER BY id DESC LIMIT 1`,
		checkedAt, nodeCount).Scan(&payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return payload, nil
}

// sameSnapshotContent compares two batches ignoring credentials, which are
// provenance rather than detected content.
func sameSnapshotContent(left, right []byte) bool {
	var a, b domain.Snapshot
	if err := json.Unmarshal(left, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(right, &b); err != nil {
		return false
	}
	if a.SpeedTest != b.SpeedTest || a.MediaCheck != b.MediaCheck || len(a.Nodes) != len(b.Nodes) {
		return false
	}
	for i := range a.Nodes {
		x, y := a.Nodes[i], b.Nodes[i]
		x.ClearCredentials()
		y.ClearCredentials()
		xb, _ := json.Marshal(x)
		yb, _ := json.Marshal(y)
		if string(xb) != string(yb) {
			return false
		}
	}
	return true
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Get returns one batch, or the newest when id is nil.
func (a *Adapter) Get(ctx context.Context, id *int) (*store.SnapshotRow, error) {
	query := `SELECT id, imported_at, payload FROM ` + tableSnapshots
	args := []any{}
	if id != nil {
		query += " WHERE id = $1"
		args = append(args, *id)
	} else {
		query += " ORDER BY id DESC LIMIT 1"
	}
	row := store.SnapshotRow{}
	err := a.pool.QueryRow(ctx, query, args...).Scan(&row.ID, &row.ImportedAt, &row.Payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// LatestSnapshotID returns the newest batch id.
func (a *Adapter) LatestSnapshotID(ctx context.Context) (int, error) {
	var id int
	err := a.pool.QueryRow(ctx, `SELECT id FROM `+tableSnapshots+` ORDER BY id DESC LIMIT 1`).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

// All returns every batch, newest first.
func (a *Adapter) All(ctx context.Context) ([]store.SnapshotRow, error) {
	rows, err := a.pool.Query(ctx,
		`SELECT id, imported_at, payload FROM `+tableSnapshots+` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.SnapshotRow, 0, 256)
	for rows.Next() {
		var row store.SnapshotRow
		if err := rows.Scan(&row.ID, &row.ImportedAt, &row.Payload); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// History returns one page of batches and the total count.
func (a *Adapter) History(ctx context.Context, limit, offset int) ([]store.HistoryItem, int, error) {
	items := make([]store.HistoryItem, 0, limit)
	rows, err := a.pool.Query(ctx,
		`SELECT id, checked_at, imported_at, node_count FROM `+tableSnapshots+`
		ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item store.HistoryItem
		var checked *string
		if err := rows.Scan(&item.ID, &checked, &item.ImportedAt, &item.NodeCount); err != nil {
			return nil, 0, err
		}
		if checked != nil {
			item.CheckedAt = *checked
		}
		item.ImportedAt = store.NormalizeTimestamp(item.ImportedAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM `+tableSnapshots).Scan(&total); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Enrichments returns stored IPPure results, for one batch or all of them.
func (a *Adapter) Enrichments(ctx context.Context, id *int) ([]store.Enrichment, error) {
	query := `SELECT snapshot_id, node_index, payload FROM ` + tableEnrichments
	args := []any{}
	if id != nil {
		query += " WHERE snapshot_id = $1"
		args = append(args, *id)
	}
	rows, err := a.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.Enrichment, 0, 1024)
	for rows.Next() {
		var item store.Enrichment
		if err := rows.Scan(&item.SnapshotID, &item.NodeIndex, &item.Payload); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SaveIPPure writes one probe result unless the connection was retired.
func (a *Adapter) SaveIPPure(ctx context.Context, snapshotID, index int, payload []byte, identity string) error {
	_, err := a.pool.Exec(ctx,
		`INSERT INTO `+tableEnrichments+`
		SELECT $1::integer, $2::integer, $3::text
		WHERE NOT EXISTS (SELECT 1 FROM `+tableRetired+` WHERE identity = $4)
		ON CONFLICT (snapshot_id, node_index) DO UPDATE SET payload = excluded.payload`,
		snapshotID, index, string(payload), identity)
	if err != nil {
		return fmt.Errorf("save ippure result: %w", err)
	}
	return nil
}

// RetireNodes soft-deletes connections in one transaction.
func (a *Adapter) RetireNodes(ctx context.Context, identities []string, retiredAt, reason string) error {
	if len(identities) == 0 {
		return nil
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	batch := &pgx.Batch{}
	for _, identity := range identities {
		batch.Queue(
			`INSERT INTO `+tableRetired+`(identity, retired_at, reason) VALUES ($1, $2, $3)
			ON CONFLICT (identity) DO NOTHING`, identity, retiredAt, reason)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("retire nodes: %w", err)
	}
	return tx.Commit(ctx)
}

// RetiredNodes lists every soft-deleted connection.
func (a *Adapter) RetiredNodes(ctx context.Context) ([]store.RetiredRow, error) {
	rows, err := a.pool.Query(ctx,
		`SELECT identity, retired_at, reason FROM `+tableRetired)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.RetiredRow, 0, 1024)
	for rows.Next() {
		var row store.RetiredRow
		if err := rows.Scan(&row.Identity, &row.RetiredAt, &row.Reason); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// RestoreNode undoes one retirement by exact match.
func (a *Adapter) RestoreNode(ctx context.Context, identity, retiredAt, reason string) (int64, error) {
	tag, err := a.pool.Exec(ctx,
		`DELETE FROM `+tableRetired+` WHERE identity = $1 AND retired_at = $2 AND reason = $3`,
		identity, retiredAt, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Reconcile rewrites archived batches with newly discovered provenance.
func (a *Adapter) Reconcile(ctx context.Context, linked map[string]domain.Source, prune bool) error {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`LOCK TABLE `+tableSnapshots+`, `+tableEnrichments+` IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, payload FROM `+tableSnapshots+` ORDER BY id`)
	if err != nil {
		return err
	}
	type rowPayload struct {
		id      int
		payload []byte
	}
	batches := make([]rowPayload, 0, 256)
	for rows.Next() {
		var rp rowPayload
		if err := rows.Scan(&rp.id, &rp.payload); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, rp)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	enrichRows, err := tx.Query(ctx,
		`SELECT snapshot_id, node_index, payload FROM `+tableEnrichments)
	if err != nil {
		return err
	}
	defer enrichRows.Close()
	bySnapshot := make(map[int][]store.Enrichment, 256)
	for enrichRows.Next() {
		var item store.Enrichment
		if err := enrichRows.Scan(&item.SnapshotID, &item.NodeIndex, &item.Payload); err != nil {
			return err
		}
		bySnapshot[item.SnapshotID] = append(bySnapshot[item.SnapshotID], item)
	}
	enrichRows.Close()

	for _, row := range batches {
		var snap domain.Snapshot
		if err := json.Unmarshal(row.payload, &snap); err != nil {
			continue
		}
		result := domain.ReconcileSnapshot(snap, linked, prune)
		if !result.Changed {
			continue
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM `+tableEnrichments+` WHERE snapshot_id = $1`, row.id); err != nil {
			return err
		}
		payload, err := json.Marshal(result.Snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE `+tableSnapshots+` SET node_count = $1, payload = $2 WHERE id = $3`,
			len(result.Snapshot.Nodes), string(payload), row.id); err != nil {
			return err
		}
		for _, item := range bySnapshot[row.id] {
			newIndex, ok := result.Indexes[item.NodeIndex]
			if !ok {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO `+tableEnrichments+` VALUES ($1, $2, $3)`,
				row.id, newIndex, string(item.Payload)); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// Close releases the pool.
func (a *Adapter) Close() error {
	a.pool.Close()
	return nil
}

// IsUniqueViolation reports whether an error is a constraint conflict.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// initProjectionSchema creates the projection tables and change triggers.
func (p *Projector) initSchema(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+lockKey+`)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ` + tableState + ` (
      singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
      source_revision bigint NOT NULL DEFAULT 0,
      built_revision bigint NOT NULL DEFAULT -1,
      retired_revision bigint NOT NULL DEFAULT 0,
      metadata jsonb
    )`,
		`CREATE TABLE IF NOT EXISTS ` + tableChanges + ` (
      snapshot_id integer PRIMARY KEY,
      revision bigint NOT NULL
    )`,
		`INSERT INTO ` + tableState + `(singleton) VALUES (true) ON CONFLICT DO NOTHING`,
		`CREATE INDEX IF NOT EXISTS node_explorer_results_changes_revision ON ` + tableChanges + `(revision)`,
		`CREATE TABLE IF NOT EXISTS ` + tableCurrent + ` (` + columnDefinitions() + `,
      PRIMARY KEY (position), UNIQUE (identity) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE INDEX IF NOT EXISTS node_explorer_current_speed
      ON ` + tableCurrent + `(rank_speed DESC NULLS LAST, position)`,
		`CREATE INDEX IF NOT EXISTS node_explorer_current_name
      ON ` + tableCurrent + `(rank_name, position)`,
		`CREATE INDEX IF NOT EXISTS node_explorer_current_country_speed
      ON ` + tableCurrent + `(country, rank_speed DESC NULLS LAST, position)`,
		`CREATE INDEX IF NOT EXISTS node_explorer_current_type_speed
      ON ` + tableCurrent + `(type, rank_speed DESC NULLS LAST, position)`,
		`CREATE OR REPLACE FUNCTION node_explorer_results_changed() RETURNS trigger
      LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
      DECLARE revision bigint; snapshot integer;
      BEGIN
        UPDATE ` + tableState + ` SET source_revision = source_revision + 1
          RETURNING source_revision INTO revision;
        IF TG_TABLE_NAME = '` + tableRetired + `' THEN
          UPDATE ` + tableState + ` SET retired_revision = revision;
        ELSE
          IF TG_TABLE_NAME = '` + tableSnapshots + `' THEN
            snapshot := CASE WHEN TG_OP = 'DELETE' THEN OLD.id ELSE NEW.id END;
          ELSE
            snapshot := CASE WHEN TG_OP = 'DELETE' THEN OLD.snapshot_id ELSE NEW.snapshot_id END;
          END IF;
          INSERT INTO ` + tableChanges + `(snapshot_id, revision) VALUES (snapshot, revision)
            ON CONFLICT (snapshot_id) DO UPDATE SET revision = EXCLUDED.revision;
        END IF;
        RETURN NULL;
      END $$`,
	}
	for _, stmt := range statements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create projection schema: %w", err)
		}
	}
	for _, table := range []string{tableSnapshots, tableEnrichments, tableRetired} {
		stmt := `DO $$ BEGIN
      IF NOT EXISTS (SELECT 1 FROM pg_trigger
        WHERE tgrelid = '` + table + `'::regclass AND tgname = 'results_changed') THEN
        CREATE TRIGGER results_changed AFTER INSERT OR UPDATE OR DELETE ON ` + table + `
          FOR EACH ROW EXECUTE FUNCTION node_explorer_results_changed();
      END IF;
    END $$`
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	// Rebuild persisted facets once when upgrading from the older metadata format.
	if _, err := tx.Exec(ctx, `UPDATE `+tableState+` SET built_revision = -1
    WHERE metadata IS NOT NULL AND NOT (metadata->'facets' ? 'ipSource'
      AND metadata->'facets' ? 'ipAttributes')`); err != nil {
		return err
	}
	// Rebuild when either the collation or node JSON representation changes.
	// Source revisions alone cannot detect a binary serialization upgrade.
	if _, err := tx.Exec(ctx, `UPDATE `+tableState+` SET built_revision = -1
    WHERE coalesce(metadata->>'ranking', '') <> $1
      OR coalesce(metadata->>'payloadVersion', '') <> $2`, rankingRevision, nodePayloadRevision); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func columnDefinitions() string {
	parts := make([]string, 0, len(projectionColumns))
	for _, name := range projectionColumnOrder {
		parts = append(parts, name+" "+projectionColumns[name])
	}
	return strings.Join(parts, ",")
}

// NewProjectorForAdapter builds a projection over an adapter's pool, which
// keeps the pool private to this package.
func NewProjectorForAdapter(ctx context.Context, a *Adapter, st *store.Store, cfg ProjectorConfig) (*Projector, error) {
	return NewProjector(ctx, a.pool, st, cfg)
}
