// Package sqlite implements the store adapter on a local database. It keeps the
// same tables and semantics as the PostgreSQL adapter so deployments can run
// without a database server; the SQL projection is PostgreSQL-only, so the API
// falls back to in-memory pagination for this backend.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

// Adapter persists snapshots in a local SQLite database.
type Adapter struct {
	db *sql.DB
}

// New opens (or creates) the database and applies the schema.
func New(path string) (*Adapter, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// The rewrite lets the API serve concurrent requests while a collector
	// writes results; SQLite serialises writers anyway, so a small pool is
	// deliberate rather than a limitation.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
	}
	a := &Adapter{db: db}
	if err := a.initSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *Adapter) initSchema() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS snapshots (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      fingerprint TEXT NOT NULL UNIQUE,
      checked_at TEXT,
      imported_at TEXT NOT NULL,
      node_count INTEGER NOT NULL,
      payload TEXT NOT NULL
    )`,
		`CREATE TABLE IF NOT EXISTS ippure_results (
      snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
      node_index INTEGER NOT NULL, payload TEXT NOT NULL,
      PRIMARY KEY (snapshot_id, node_index)
    )`,
		`CREATE TABLE IF NOT EXISTS retired_nodes (
      identity TEXT PRIMARY KEY,
      retired_at TEXT NOT NULL,
      reason TEXT NOT NULL
    )`,
	}
	for _, stmt := range statements {
		if _, err := a.db.Exec(stmt); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	return a.initChanges()
}

// Insert stores a detection batch, ignoring duplicates by fingerprint.
func (a *Adapter) Insert(ctx context.Context, fingerprint string, payload []byte, checkedAt string, nodeCount int) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing []byte
	lookupErr := tx.QueryRowContext(ctx, `SELECT payload FROM snapshots WHERE fingerprint=?`, fingerprint).Scan(&existing)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return lookupErr
	}
	if lookupErr == nil {
		merged, changed, e := domain.MergeSnapshotLinks(existing, payload)
		if e != nil {
			return e
		}
		if changed {
			if _, e = tx.ExecContext(ctx, `UPDATE snapshots SET payload=? WHERE fingerprint=?`, string(merged), fingerprint); e != nil {
				return e
			}
		}
		return tx.Commit()
	}
	var checked any
	if checkedAt != "" {
		checked = checkedAt
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO snapshots
		(fingerprint, checked_at, imported_at, node_count, payload) VALUES (?, ?, ?, ?, ?)`,
		fingerprint, checked, store.NowTimestamp(), nodeCount, string(payload))
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return tx.Commit()
}

// Get returns one batch, or the newest when id is nil.
func (a *Adapter) Get(ctx context.Context, id *int) (*store.SnapshotRow, error) {
	query := `SELECT id, imported_at, payload FROM snapshots`
	args := []any{}
	if id != nil {
		query += " WHERE id = ?"
		args = append(args, *id)
	} else {
		query += " ORDER BY id DESC LIMIT 1"
	}
	row := a.db.QueryRowContext(ctx, query, args...)
	out := store.SnapshotRow{}
	if err := row.Scan(&out.ID, &out.ImportedAt, &out.Payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

// LatestSnapshotID returns the newest batch id.
func (a *Adapter) LatestSnapshotID(ctx context.Context) (int, error) {
	var id int
	err := a.db.QueryRowContext(ctx, `SELECT id FROM snapshots ORDER BY id DESC LIMIT 1`).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

// All returns every batch, newest first.
func (a *Adapter) All(ctx context.Context) ([]store.SnapshotRow, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, imported_at, payload FROM snapshots ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.SnapshotRow, 0, 128)
	for rows.Next() {
		var row store.SnapshotRow
		if err := rows.Scan(&row.ID, &row.ImportedAt, &row.Payload); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Enrichments returns stored IPPure results.
func (a *Adapter) Enrichments(ctx context.Context, id *int) ([]store.Enrichment, error) {
	query := `SELECT snapshot_id, node_index, payload FROM ippure_results`
	args := []any{}
	if id != nil {
		query += " WHERE snapshot_id = ?"
		args = append(args, *id)
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.Enrichment, 0, 512)
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
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO ippure_results SELECT ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM retired_nodes WHERE identity = ?)
		ON CONFLICT(snapshot_id, node_index) DO UPDATE SET payload = excluded.payload`,
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
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO retired_nodes(identity, retired_at, reason) VALUES (?, ?, ?)
		ON CONFLICT(identity) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, identity := range identities {
		if _, err := stmt.ExecContext(ctx, identity, retiredAt, reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RetiredNodes lists every soft-deleted connection.
func (a *Adapter) RetiredNodes(ctx context.Context) ([]store.RetiredRow, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT identity, retired_at, reason FROM retired_nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.RetiredRow, 0, 512)
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
	result, err := a.db.ExecContext(ctx,
		`DELETE FROM retired_nodes WHERE identity = ? AND retired_at = ? AND reason = ?`,
		identity, retiredAt, reason)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Reconcile rewrites archived batches with newly discovered provenance.
func (a *Adapter) Reconcile(ctx context.Context, linked map[string]domain.Source, prune bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, payload FROM snapshots ORDER BY id`)
	if err != nil {
		return err
	}
	type rowPayload struct {
		id      int
		payload []byte
	}
	batches := make([]rowPayload, 0, 128)
	for rows.Next() {
		var rp rowPayload
		if err := rows.Scan(&rp.id, &rp.payload); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, rp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	enrichRows, err := tx.QueryContext(ctx,
		`SELECT snapshot_id, node_index, payload FROM ippure_results`)
	if err != nil {
		return err
	}
	bySnapshot := make(map[int][]store.Enrichment, 128)
	for enrichRows.Next() {
		var item store.Enrichment
		if err := enrichRows.Scan(&item.SnapshotID, &item.NodeIndex, &item.Payload); err != nil {
			enrichRows.Close()
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
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM ippure_results WHERE snapshot_id = ?`, row.id); err != nil {
			return err
		}
		payload, err := json.Marshal(result.Snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE snapshots SET node_count = ?, payload = ? WHERE id = ?`,
			len(result.Snapshot.Nodes), string(payload), row.id); err != nil {
			return err
		}
		for _, item := range bySnapshot[row.id] {
			newIndex, ok := result.Indexes[item.NodeIndex]
			if !ok {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ippure_results VALUES (?, ?, ?)`,
				row.id, newIndex, string(item.Payload)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Close releases the database handle.
func (a *Adapter) Close() error {
	return a.db.Close()
}

// ErrNoProjection is returned when a caller asks this backend for SQL paging.
var ErrNoProjection = errors.New("sqlite backend has no results projection")

// Paginate applies a query in memory for deployments without PostgreSQL.
func Paginate(snapshot domain.Snapshot, q *Query) domain.Snapshot {
	if q == nil {
		return snapshot
	}
	matches := make([]domain.Node, 0, len(snapshot.Nodes))
	for _, n := range snapshot.Nodes {
		if !matchNode(n, q, snapshot) {
			continue
		}
		matches = append(matches, n)
	}
	key, direction := domain.ResultsSort(domain.ResultsMetadata(snapshot), q.Sort, q.Direction)
	sign := 1
	if direction == "desc" {
		sign = -1
	}
	keys := make([][]byte, len(matches))
	for i, n := range matches {
		v := domain.SortValue(n, key)
		if v == nil {
			continue
		}
		if _, isNum := v.(float64); isNum {
			keys[i] = []byte(fmt.Sprintf("%v", v))
		} else {
			keys[i] = domain.SortKey(fmt.Sprint(v))
		}
	}
	sortNodes(matches, keys, key, sign)
	page := q.Page
	if max := (len(matches) + q.Limit - 1) / q.Limit; max < 1 {
		page = 1
	} else if page > max {
		page = max
	}
	start := (page - 1) * q.Limit
	end := start + q.Limit
	if start > len(matches) {
		start = len(matches)
	}
	if end > len(matches) {
		end = len(matches)
	}
	out := snapshot
	out.Nodes = matches[start:end]
	return out
}

func sortNodes(nodes []domain.Node, keys [][]byte, key string, direction int) {
	index := make([]int, len(nodes))
	for i := range index {
		index[i] = i
	}
	numeric := key == "speed" || key == "ippure" || key == "trafficRatio"
	sort.SliceStable(index, func(a, b int) bool {
		ia, ib := index[a], index[b]
		// Nodes without a value always sort last, matching SQL NULLS LAST.
		if keys[ia] == nil && keys[ib] == nil {
			return ia < ib
		}
		if keys[ia] == nil {
			return false
		}
		if keys[ib] == nil {
			return true
		}
		var cmp int
		if numeric {
			fa, fb := toFloat(nodes[ia], key), toFloat(nodes[ib], key)
			switch {
			case fa < fb:
				cmp = -1
			case fa > fb:
				cmp = 1
			}
		} else {
			cmp = strings.Compare(string(keys[ia]), string(keys[ib]))
		}
		if cmp == 0 {
			return ia < ib
		}
		return direction*cmp < 0
	})
	sorted := make([]domain.Node, len(nodes))
	for i, pos := range index {
		sorted[i] = nodes[pos]
	}
	copy(nodes, sorted)
}

func toFloat(n domain.Node, key string) float64 {
	v := domain.SortValue(n, key)
	f, _ := v.(float64)
	return f
}

// Query mirrors the PostgreSQL query shape for the in-memory fallback.
type Query struct {
	Page         int
	Limit        int
	Search       string
	Countries    []string
	Types        []string
	Networks     []string
	Features     []string
	IPSources    []string
	IPAttributes []string
	Media        []string
	MinSpeed     float64
	Sort         string
	Direction    string
}

func matchNode(n domain.Node, q *Query, snapshot domain.Snapshot) bool {
	if q.Search != "" {
		found := false
		for _, v := range domain.SearchValues(n) {
			if strings.Contains(v, q.Search) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(q.Countries) > 0 && !contains(q.Countries, orUnknown(n.Country)) {
		return false
	}
	if len(q.Types) > 0 && !contains(q.Types, orUnknown(n.Type)) {
		return false
	}
	if len(q.Networks) > 0 && !contains(q.Networks, orUnknown(n.Network)) {
		return false
	}
	for _, feature := range q.Features {
		switch feature {
		case "tls":
			if !n.TLS {
				return false
			}
		case "reality":
			if !n.Reality {
				return false
			}
		case "udp":
			if !n.UDP {
				return false
			}
		}
	}
	if len(q.IPSources) > 0 {
		if n.IPPure == nil || n.IPPure.Status != "success" || !contains(q.IPSources, n.IPPure.Source) {
			return false
		}
	}
	if len(q.IPAttributes) > 0 {
		if n.IPPure == nil || (n.IPPure.Status != "success" && n.IPPure.Classification == nil) {
			return false
		}
		found := false
		for _, value := range n.IPPure.Attributes {
			if contains(q.IPAttributes, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if snapshot.SpeedTest && (n.Speed) < q.MinSpeed*1024 {
		return false
	}
	if snapshot.MediaCheck {
		for _, platform := range q.Media {
			if !domain.HasMedia(n, platform) {
				return false
			}
		}
	}
	return true
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
