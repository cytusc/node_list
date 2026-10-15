package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

// projectionColumns is the persisted projection schema. Column names are part
// of the deployed database and the ranking columns are derived from
// domain.ResultSortKeys.
var (
	projectionColumnOrder = []string{
		"position", "identity", "payload", "country", "type", "network",
		"tls", "reality", "udp", "speed", "search_values", "media_available",
	}
	projectionColumns = buildProjectionColumns()
)

func buildProjectionColumns() map[string]string {
	columns := map[string]string{
		"position":        "integer",
		"identity":        "text",
		"payload":         "text",
		"country":         "text",
		"type":            "text",
		"network":         "text",
		"tls":             "boolean",
		"reality":         "boolean",
		"udp":             "boolean",
		"speed":           "double precision",
		"search_values":   "text[]",
		"media_available": "text[]",
	}
	for _, key := range domain.ResultSortKeys {
		columns[domain.SortColumn(key)] = "integer"
		projectionColumnOrder = append(projectionColumnOrder, domain.SortColumn(key))
	}
	return columns
}

// projectionRow is one persisted current-node row.
type projectionRow struct {
	Position       int      `json:"position"`
	Identity       string   `json:"identity"`
	Payload        string   `json:"payload"`
	Country        string   `json:"country"`
	Type           string   `json:"type"`
	Network        string   `json:"network"`
	TLS            bool     `json:"tls"`
	Reality        bool     `json:"reality"`
	UDP            bool     `json:"udp"`
	Speed          float64  `json:"speed"`
	SearchValues   []string `json:"search_values"`
	MediaAvailable []string `json:"media_available"`
	rankByColumn   map[string]*int
	// serialized is the rendered row, kept so the diff pass compares strings
	// instead of re-marshalling every node on every rebuild.
	serialized string
}

// MarshalJSON flattens the rank columns into the row. The projection writes
// whole rows through jsonb_to_recordset, so the shape must match the table.
func (r projectionRow) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(projectionColumnOrder))
	out["position"] = r.Position
	out["identity"] = r.Identity
	out["payload"] = r.Payload
	out["country"] = r.Country
	out["type"] = r.Type
	out["network"] = r.Network
	out["tls"] = r.TLS
	out["reality"] = r.Reality
	out["udp"] = r.UDP
	out["speed"] = r.Speed
	out["search_values"] = r.SearchValues
	out["media_available"] = r.MediaAvailable
	for _, key := range domain.ResultSortKeys {
		out[domain.SortColumn(key)] = r.rankByColumn[key]
	}
	return json.Marshal(out)
}

// Projector owns the current-node projection: an incremental materialisation
// that lets the results page filter, sort and paginate in SQL.
type Projector struct {
	pool  *pgxpool.Pool
	store *store.Store
	model *store.Store
	runMu sync.Mutex

	mu         sync.Mutex
	revision   int64
	projection map[int]string
	analyzedAt time.Time
	lastPrune  time.Time

	retentionDays int
	analyzeEvery  time.Duration
}

// ProjectorConfig tunes the rebuild loop.
type ProjectorConfig struct {
	RetentionDays int
	AnalyzeEvery  time.Duration
}

// NewProjector prepares the projection schema.
func NewProjector(ctx context.Context, pool *pgxpool.Pool, st *store.Store, cfg ProjectorConfig) (*Projector, error) {
	p := &Projector{
		pool:          pool,
		store:         st,
		model:         store.New(nil, nil),
		revision:      -1,
		projection:    make(map[int]string, 2048),
		retentionDays: cfg.RetentionDays,
		analyzeEvery:  cfg.AnalyzeEvery,
	}
	if p.analyzeEvery <= 0 {
		p.analyzeEvery = 15 * time.Minute
	}
	if p.retentionDays == 0 {
		if v := os.Getenv("SNAPSHOT_RETENTION_DAYS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				p.retentionDays = n
			}
		} else {
			p.retentionDays = 7
		}
	}
	if err := p.initSchema(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// RefreshStats reports what one rebuild did.
type RefreshStats struct {
	Updated      bool  `json:"updated"`
	Nodes        int   `json:"nodes"`
	BatchesRead  int   `json:"batchesRead"`
	NodesWritten int   `json:"nodesWritten"`
	Incremental  bool  `json:"incremental"`
	Pruned       int   `json:"pruned"`
	Ms           int64 `json:"ms"`
}

// Refresh publishes one consistent source generation. Concurrent changes stay
// dirty for the next pass, which is safe because the trigger bumps the revision
// for every writer, including other services.
func (p *Projector) Refresh(ctx context.Context) error {
	p.runMu.Lock()
	defer p.runMu.Unlock()
	started := time.Now()
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+lockKey+`)`).Scan(&locked); err != nil {
		// The query may have reached the server even if its response was cancelled.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Conn().Close(cleanup)
		conn.Release()
		return err
	}
	if !locked {
		conn.Release()
		return nil
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(cleanup, `SELECT pg_advisory_unlock(`+lockKey+`)`).Scan(&unlocked); err != nil || !unlocked {
				_ = conn.Conn().Close(cleanup)
			}
			conn.Release()
		})
	}
	defer release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var source, built int64
	if err := tx.QueryRow(ctx, `SELECT source_revision, built_revision FROM `+tableState).Scan(&source, &built); err != nil {
		return err
	}
	p.mu.Lock()
	cached := p.revision
	p.mu.Unlock()
	if source == built {
		return nil
	}
	changes, err := readChanges(ctx, tx, cached)
	if err != nil {
		return err
	}
	if err := tx.Rollback(ctx); err != nil {
		return err
	}
	if err := p.model.ApplyChanges(changes); err != nil {
		return err
	}
	incremental := !changes.Full
	batchRows := changes.Batches

	aggregate := p.model.Aggregate(false)
	var nodes []domain.Node
	var metadata domain.Metadata
	if aggregate != nil {
		aggregate.Nodes = p.model.FilterDepartedNodes(aggregate.Nodes)
		nodes = aggregate.Nodes
		aggregate.Nodes = nodes
		metadata = domain.ResultsMetadata(*aggregate)
	}
	rows := projectionRows(nodes)

	// Only rows whose serialised form changed need a write.
	p.mu.Lock()
	canPatch := p.revision == built
	changed := rows
	if canPatch {
		changed = nil
		for i := range rows {
			serialized, err := json.Marshal(rows[i])
			if err != nil {
				p.mu.Unlock()
				return err
			}
			rows[i].serialized = string(serialized)
			if previous, ok := p.projection[rows[i].Position]; ok && previous == rows[i].serialized {
				continue
			}
			changed = append(changed, rows[i])
		}
	} else {
		for i := range rows {
			serialized, err := json.Marshal(rows[i])
			if err != nil {
				p.mu.Unlock()
				return err
			}
			rows[i].serialized = string(serialized)
		}
	}
	previous := p.projection
	p.mu.Unlock()

	if err := p.write(ctx, conn, rows, changed, canPatch, previous, source, metadata); err != nil {
		return err
	}

	next := make(map[int]string, len(rows))
	for i := range rows {
		next[rows[i].Position] = rows[i].serialized
	}
	p.mu.Lock()
	p.projection = next
	p.revision = source
	p.mu.Unlock()

	stats := RefreshStats{
		Updated:      true,
		Nodes:        len(rows),
		BatchesRead:  len(batchRows),
		NodesWritten: len(changed),
		Incremental:  incremental,
		Ms:           time.Since(started).Milliseconds(),
	}
	release()
	if pruned, err := p.prune(ctx); err == nil {
		stats.Pruned = pruned
	}
	if stats.NodesWritten > 0 {
		p.analyze(ctx)
	}
	lastStats.Store(stats)
	return nil
}

var lastStats atomicValue[RefreshStats]

// atomicValue is a tiny typed atomic box, used for the last rebuild summary.
type atomicValue[T any] struct {
	mu   sync.Mutex
	item T
}

func (a *atomicValue[T]) Store(v T) {
	a.mu.Lock()
	a.item = v
	a.mu.Unlock()
}

func (a *atomicValue[T]) Load() T {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.item
}

// Stats returns the last rebuild summary for health output.
func (p *Projector) Stats() RefreshStats { return lastStats.Load() }

func queryBatches(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, ids []int) ([]store.SnapshotRow, error) {
	query := `SELECT id, imported_at, payload FROM ` + tableSnapshots
	args := []any{}
	if ids != nil {
		query += " WHERE id = ANY($1::integer[])"
		args = append(args, ids)
	}
	query += " ORDER BY id DESC"
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.SnapshotRow, 0, len(ids)+8)
	for rows.Next() {
		var row store.SnapshotRow
		if err := rows.Scan(&row.ID, &row.ImportedAt, &row.Payload); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func queryEnrichments(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, ids []int) ([]store.Enrichment, error) {
	query := `SELECT snapshot_id, node_index, payload FROM ` + tableEnrichments
	args := []any{}
	if ids != nil {
		query += " WHERE snapshot_id = ANY($1::integer[])"
		args = append(args, ids)
	}
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.Enrichment, 0, 256)
	for rows.Next() {
		var item store.Enrichment
		if err := rows.Scan(&item.SnapshotID, &item.NodeIndex, &item.Payload); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func queryRetired(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]store.RetiredRow, error) {
	rows, err := db.Query(ctx, `SELECT identity, retired_at, reason FROM `+tableRetired)
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

// write publishes the projection and advances the built revision atomically.
func (p *Projector) write(ctx context.Context, conn *pgxpool.Conn, all, changed []projectionRow, patch bool, previous map[int]string, revision int64, metadata domain.Metadata) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	rolled := false
	defer func() {
		if !rolled {
			tx.Rollback(ctx)
		}
	}()
	if patch {
		present := make(map[int]struct{}, len(all))
		for i := range all {
			present[all[i].Position] = struct{}{}
		}
		removed := make([]int, 0, 8)
		for position := range previous {
			if _, ok := present[position]; !ok {
				removed = append(removed, position)
			}
		}
		if len(removed) > 0 {
			if _, err := tx.Exec(ctx,
				`DELETE FROM `+tableCurrent+` WHERE position = ANY($1::integer[])`, removed); err != nil {
				return err
			}
		}
	} else {
		if _, err := tx.Exec(ctx, `DELETE FROM `+tableCurrent); err != nil {
			return err
		}
	}
	assignments := make([]string, 0, len(projectionColumnOrder))
	for _, name := range projectionColumnOrder {
		if name == "position" {
			continue
		}
		assignments = append(assignments, name+" = EXCLUDED."+name)
	}
	const chunk = 250
	for offset := 0; offset < len(changed); offset += chunk {
		end := offset + chunk
		if end > len(changed) {
			end = len(changed)
		}
		payload, err := json.Marshal(changed[offset:end])
		if err != nil {
			return err
		}
		stmt := `INSERT INTO ` + tableCurrent + `
		SELECT * FROM jsonb_to_recordset($1::jsonb) AS n(` + columnDefinitions() + `)
		ON CONFLICT (position) DO UPDATE SET ` + strings.Join(assignments, ",")
		if _, err := tx.Exec(ctx, stmt, string(payload)); err != nil {
			return fmt.Errorf("write projection: %w", err)
		}
	}
	meta, err := marshalStoredMetadata(metadata)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE `+tableState+` SET built_revision = $1, metadata = $2::jsonb`,
		revision, string(meta)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	rolled = true
	return nil
}

// analyze refreshes planner statistics. The JavaScript service ran ANALYZE
// after every rebuild, which happened every few seconds; throttling it removes
// a steady background write load without affecting plan quality.
func (p *Projector) analyze(ctx context.Context) {
	p.mu.Lock()
	due := time.Since(p.analyzedAt) >= p.analyzeEvery
	if due {
		p.analyzedAt = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	analyzeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = p.pool.Exec(analyzeCtx, `ANALYZE `+tableCurrent)
}

// prune drops batches older than the retention window. Deleting rows fires the
// change trigger, so the next rebuild naturally accounts for the loss.
func (p *Projector) prune(ctx context.Context) (int, error) {
	if p.retentionDays <= 0 {
		return 0, nil
	}
	p.mu.Lock()
	if time.Since(p.lastPrune) < time.Hour {
		p.mu.Unlock()
		return 0, nil
	}
	p.lastPrune = time.Now()
	p.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -p.retentionDays).UTC().Format(time.RFC3339Nano)
	rows, err := p.pool.Query(ctx,
		`SELECT id FROM `+tableSnapshots+` WHERE imported_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	ids := make([]int, 0, 64)
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM `+tableEnrichments+` WHERE snapshot_id = ANY($1::integer[])`, ids); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM `+tableSnapshots+` WHERE id = ANY($1::integer[])`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	p.store.RemoveBatch(ids)
	p.mu.Lock()
	p.revision = -1
	p.projection = make(map[int]string, 2048)
	p.mu.Unlock()
	return len(ids), nil
}

// Query is a parsed results request.
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

// Result is one page of the projection.
type Result struct {
	domain.Metadata
	Nodes []domain.Node `json:"nodes"`
	Total int           `json:"total"`
	Page  int           `json:"page"`
	Limit int           `json:"limit"`
}

// rankingRevision identifies the collation the persisted rank_* columns were
// built with. Bump it whenever the ordering changes: initSchema compares the
// stored marker and resets built_revision so the projection is rebuilt once.
//
// The first Go cutover corrected the numeric collation (see domain/numeric.go),
// so every rank_name written by the JavaScript service is stale and must be
// recomputed even when the source revision is otherwise up to date.
const rankingRevision = "icu-numeric-1"

// nodePayloadRevision invalidates persisted node JSON when serialization
// changes, even when no snapshot/enrichment has been updated since deployment.
const nodePayloadRevision = "json-presence-1"

// storedMetadata is the metadata column layout. The API must not see the
// ranking marker, so it lives in a wrapper rather than on domain.Metadata,
// which is embedded directly in the JSON response.
type storedMetadata struct {
	domain.Metadata
	Ranking        string `json:"ranking,omitempty"`
	PayloadVersion string `json:"payloadVersion,omitempty"`
}

func marshalStoredMetadata(m domain.Metadata) ([]byte, error) {
	return json.Marshal(storedMetadata{Metadata: m, Ranking: rankingRevision, PayloadVersion: nodePayloadRevision})
}

func unmarshalStoredMetadata(raw []byte) (domain.Metadata, error) {
	var stored storedMetadata
	if err := json.Unmarshal(raw, &stored); err != nil {
		return domain.Metadata{}, err
	}
	return stored.Metadata, nil
}

// featureColumns is the whitelist of boolean filters. Request values are bound
// parameters only; these names are interpolated, so they are never user input.
var featureColumns = map[string]bool{"tls": true, "reality": true, "udp": true}

// QueryPage filters, sorts and paginates the projection in SQL.
func (p *Projector) QueryPage(ctx context.Context, q *Query) (*Result, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return nil, err
	}
	var rawMetadata []byte
	if err := tx.QueryRow(ctx, `SELECT metadata FROM `+tableState).Scan(&rawMetadata); err != nil {
		return nil, err
	}
	if len(rawMetadata) == 0 || string(rawMetadata) == "null" {
		tx.Rollback(ctx)
		return nil, nil
	}
	metadata, err := unmarshalStoredMetadata(rawMetadata)
	if err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	if q == nil {
		rows, err := tx.Query(ctx,
			`SELECT payload FROM `+tableCurrent+` ORDER BY position`)
		if err != nil {
			return nil, err
		}
		nodes, err := scanNodes(rows)
		if err != nil {
			return nil, err
		}
		tx.Rollback(ctx)
		return &Result{Metadata: metadata, Nodes: nodes}, nil
	}

	var values []any
	param := func(v any) string {
		values = append(values, v)
		return "$" + strconv.Itoa(len(values))
	}
	var conditions []string
	addList := func(column string, list []string) {
		if len(list) > 0 {
			conditions = append(conditions, fmt.Sprintf("%s = ANY(%s::text[])", column, param(list)))
		}
	}
	addList("country", q.Countries)
	addList("type", q.Types)
	addList("network", q.Networks)
	for _, feature := range q.Features {
		if featureColumns[feature] {
			conditions = append(conditions, feature)
		}
	}
	if len(q.IPSources) > 0 || len(q.IPAttributes) > 0 {
		conditions = append(conditions, "payload::jsonb->'ippure'->>'status' = 'success'")
		if len(q.IPSources) > 0 {
			conditions = append(conditions,
				fmt.Sprintf(`payload::jsonb->'ippure'->>'source' = ANY(%s::text[])`, param(q.IPSources)))
		}
		if len(q.IPAttributes) > 0 {
			conditions = append(conditions,
				fmt.Sprintf(`(payload::jsonb->'ippure'->'attributes') ?| %s::text[]`, param(q.IPAttributes)))
		}
	}
	if q.Search != "" {
		conditions = append(conditions,
			fmt.Sprintf(`EXISTS (SELECT 1 FROM unnest(search_values) AS v WHERE strpos(v, %s) > 0)`, param(q.Search)))
	}
	if metadata.SpeedTest && q.MinSpeed > 0 {
		conditions = append(conditions, fmt.Sprintf("speed >= %s", param(q.MinSpeed*1024)))
	}
	if metadata.MediaCheck && len(q.Media) > 0 {
		conditions = append(conditions, fmt.Sprintf("%s::text[] <@ media_available", param(q.Media)))
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+tableCurrent+` `+where, values...).Scan(&total); err != nil {
		return nil, err
	}
	page := q.Page
	if max := (total + q.Limit - 1) / q.Limit; max < 1 {
		page = 1
	} else if page > max {
		page = max
	}
	key, direction := domain.ResultsSort(metadata, q.Sort, q.Direction)
	order := "DESC"
	if direction == "asc" {
		order = "ASC"
	}
	limit := param(q.Limit)
	offset := param((page - 1) * q.Limit)
	query := fmt.Sprintf(`SELECT payload FROM `+tableCurrent+` %s
		ORDER BY %s %s NULLS LAST, position LIMIT %s OFFSET %s`,
		where, domain.SortColumn(key), order, limit, offset)
	rows, err := tx.Query(ctx, query, values...)
	if err != nil {
		return nil, err
	}
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, err
	}
	tx.Rollback(ctx)
	return &Result{
		Metadata: metadata,
		Nodes:    nodes,
		Total:    total,
		Page:     page,
		Limit:    q.Limit,
	}, nil
}

func scanNodes(rows pgx.Rows) ([]domain.Node, error) {
	defer rows.Close()
	out := make([]domain.Node, 0, 256)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var node domain.Node
		if err := json.Unmarshal(payload, &node); err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, rows.Err()
}

// projectionRows ranks and serialises the visible node set.
func projectionRows(nodes []domain.Node) []projectionRow {
	rows := make([]projectionRow, len(nodes))
	ranks := make([]map[int]*int, len(domain.ResultSortKeys))
	for i, n := range nodes {
		rankByColumn := make(map[string]*int, len(domain.ResultSortKeys))
		rows[i] = projectionRow{
			Position:       i,
			Identity:       domain.AggregateIdentity(n),
			Payload:        mustJSON(n),
			Country:        orUnknown(n.Country),
			Type:           orUnknown(n.Type),
			Network:        orUnknown(n.Network),
			TLS:            n.TLS,
			Reality:        n.Reality,
			UDP:            n.UDP,
			Speed:          n.Speed,
			SearchValues:   domain.SearchValues(n),
			MediaAvailable: domain.MediaPlatforms(n),
			rankByColumn:   rankByColumn,
		}
	}
	for keyIndex, key := range domain.ResultSortKeys {
		ranks[keyIndex] = rankColumn(nodes, key)
	}
	for i := range rows {
		for keyIndex, key := range domain.ResultSortKeys {
			rows[i].rankByColumn[key] = ranks[keyIndex][i]
		}
	}
	return rows
}

// rankColumn assigns dense ranks under the zh-CN numeric collation so
// PostgreSQL sorting reproduces the order the UI showed before the rewrite.
// Collation keys are computed once per value instead of on every comparison.
func rankColumn(nodes []domain.Node, key string) map[int]*int {
	type entry struct {
		position int
		value    any
		sortKey  []byte
	}
	entries := make([]entry, 0, len(nodes))
	numeric := key == "speed" || key == "ippure" || key == "trafficRatio"
	for i, n := range nodes {
		value := domain.SortValue(n, key)
		if value == nil {
			continue
		}
		e := entry{position: i, value: value}
		if !numeric {
			e.sortKey = domain.SortKey(fmt.Sprint(value))
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if numeric {
			return toFloat(entries[a].value) < toFloat(entries[b].value)
		}
		return bytes.Compare(entries[a].sortKey, entries[b].sortKey) < 0
	})
	out := make(map[int]*int, len(entries))
	rank := 0
	for i, e := range entries {
		if i > 0 {
			var different bool
			if numeric {
				different = toFloat(entries[i-1].value) != toFloat(e.value)
			} else {
				different = !bytes.Equal(entries[i-1].sortKey, e.sortKey)
			}
			if different {
				rank++
			}
		}
		value := rank
		out[e.position] = &value
	}
	return out
}

func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ErrNoProjection is returned when the projection has never been built.
var ErrNoProjection = errors.New("results projection is not built yet")
