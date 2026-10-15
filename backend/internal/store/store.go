// Package store owns snapshot persistence and the in-process projection of the
// current node set.
//
// The previous JavaScript implementation had to decode every snapshot inside a
// single worker thread and ship the results back over IPC on every read, which
// pinned two cores. Go keeps the decoded batches in-process behind an
// RWMutex, so a refresh only re-reads the batches PostgreSQL reported as
// changed and every reader sees the same memory.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// RetireRestoreReason marks retirements a scheduled patrol recheck may undo.
// Manual retirements and IPPure retirements with other wording stay until
// handled explicitly, so the wording must match at both retire call sites.
const RetireRestoreReason = "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed"

// Departure thresholds: a node absent from every snapshot for longer than its
// source cadence tolerates has left its panel and must not occupy the results
// view or the probe queue forever. Qilan imports every 15 minutes, subs-check
// roughly every two hours.
const (
	DepartedQilan = 45 * time.Minute
	DepartedSubs  = 4 * time.Hour
)

// SnapshotRow is one persisted detection batch.
type SnapshotRow struct {
	ID         int
	ImportedAt string
	Payload    []byte
}

// HistoryItem is the batch list shown in the UI.
type HistoryItem struct {
	ID         int    `json:"id"`
	CheckedAt  string `json:"checkedAt"`
	ImportedAt string `json:"importedAt"`
	NodeCount  int    `json:"nodeCount"`
}

// jsTimestampLayout matches Date.prototype.toISOString(): exactly three
// fractional digits and a "Z" suffix. Both back ends keep imported_at as TEXT
// copied verbatim, so writing any other precision would surface as a visible
// format drift in /api/history.
const jsTimestampLayout = "2006-01-02T15:04:05.000Z"

// NowTimestamp renders the current time the way JavaScript does.
func NowTimestamp() string { return time.Now().UTC().Format(jsTimestampLayout) }

// NormalizeTimestamp rewrites a stored timestamp into the JavaScript layout.
// Rows written by the previous implementation are already in that layout; rows
// written by an earlier revision of this one carry microsecond precision, and
// re-rendering them keeps the API byte-identical either way. Unrecognised
// values pass through untouched.
func NormalizeTimestamp(value string) string {
	if value == "" {
		return value
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(jsTimestampLayout)
		}
	}
	return value
}

// Enrichment is one stored IPPure result.
type Enrichment struct {
	SnapshotID int
	NodeIndex  int
	Payload    []byte
}

// RetiredRow is one soft-deleted connection.
type RetiredRow struct {
	Identity  string
	RetiredAt string
	Reason    string
}

// Adapter is the persistence port. Both PostgreSQL and SQLite implement it;
// the projection lives in the PostgreSQL adapter only.
type Adapter interface {
	Insert(ctx context.Context, fingerprint string, payload []byte, checkedAt string, nodeCount int) error
	Get(ctx context.Context, id *int) (*SnapshotRow, error)
	LatestSnapshotID(ctx context.Context) (int, error)
	All(ctx context.Context) ([]SnapshotRow, error)
	History(ctx context.Context, limit, offset int) ([]HistoryItem, int, error)
	Enrichments(ctx context.Context, id *int) ([]Enrichment, error)
	SaveIPPure(ctx context.Context, snapshotID, index int, payload []byte, identity string) error
	RetireNodes(ctx context.Context, identities []string, retiredAt, reason string) error
	RetiredNodes(ctx context.Context) ([]RetiredRow, error)
	RestoreNode(ctx context.Context, identity, retiredAt, reason string) (int64, error)
	Reconcile(ctx context.Context, linked map[string]domain.Source, prune bool) error
	Close() error
}

// batchedSnapshot is the decoded form kept in memory.
type batchedSnapshot struct {
	ID         int
	ImportedAt string
	CheckedAt  string
	SpeedTest  bool
	MediaCheck bool
	Nodes      []domain.Node
	Identities []string
	// Provenance indicates a Qilan round; cadence decides the departure window.
	Qilan bool
}

// Store is the read model shared by the API and the collectors.
type Store struct {
	adapter Adapter

	ioMu     sync.Mutex // serializes database reads with local cache mutations
	mu       sync.RWMutex
	batches  map[int]*batchedSnapshot
	order    []int // batch ids, newest first
	enrich   map[int]map[int]json.RawMessage
	retired  map[string]RetiredRow
	revision int64

	syncFlight    *flight
	refreshFlight *flight
	snapshotWarn  string

	// refresh is the projection rebuild hook; nil for SQLite deployments.
	refresh func(context.Context) error
}

// New builds a store over an adapter.
func New(adapter Adapter, refresh func(context.Context) error) *Store {
	return &Store{
		adapter:       adapter,
		revision:      -1,
		batches:       make(map[int]*batchedSnapshot),
		enrich:        make(map[int]map[int]json.RawMessage),
		retired:       make(map[string]RetiredRow),
		syncFlight:    newFlight(),
		refreshFlight: newFlight(),
		refresh:       refresh,
	}
}

// flight collapses concurrent callers onto one in-flight operation, so a burst
// of requests and the background timer share a single database round.
type flightCall struct {
	done chan struct{}
	err  error
}
type flight struct {
	mu   sync.Mutex
	call *flightCall
}

func newFlight() *flight { return &flight{} }
func (f *flight) run(ctx context.Context, fn func(context.Context) error) error {
	f.mu.Lock()
	if c := f.call; c != nil {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &flightCall{done: make(chan struct{})}
	f.call = c
	f.mu.Unlock()
	c.err = fn(ctx)
	f.mu.Lock()
	close(c.done)
	f.call = nil
	f.mu.Unlock()
	return c.err
}

// Source warning codes. The JavaScript store distinguished a results file that
// had not been published yet from one it could not read, and the code travelled
// to the browser in the results payload. Keep the two distinct so a deployment
// comparing the old and new service sees identical bytes.
const (
	SourceWarningNone       = ""
	SourceWarningNoResults  = "NO_RESULTS"
	SourceWarningReadFailed = "READ_FAILED"
)

// SourceWarningFor classifies an ingestion failure. A missing file is expected
// while subs-check has not finished its first round, so it is not reported as a
// read failure; anything else is.
func SourceWarningFor(err error) string {
	switch {
	case err == nil:
		return SourceWarningNone
	case errors.Is(err, fs.ErrNotExist):
		return SourceWarningNoResults
	default:
		return SourceWarningReadFailed
	}
}

// SetSourceWarning records why the upstream files could not be read.
func (s *Store) SetSourceWarning(code string) {
	s.mu.Lock()
	s.snapshotWarn = code
	s.mu.Unlock()
}

func (s *Store) SourceWarning() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotWarn
}

// LoadBatch decodes one persisted batch into the in-memory cache.
func (s *Store) loadBatch(row SnapshotRow) (*batchedSnapshot, error) {
	var snap domain.Snapshot
	if err := json.Unmarshal(row.Payload, &snap); err != nil {
		return nil, fmt.Errorf("decode snapshot %d: %w", row.ID, err)
	}
	identities := make([]string, len(snap.Nodes))
	for i, n := range snap.Nodes {
		identities[i] = domain.AggregateIdentity(n)
	}
	return &batchedSnapshot{
		ID:         row.ID,
		ImportedAt: row.ImportedAt,
		CheckedAt:  snap.CheckedAt,
		SpeedTest:  snap.SpeedTest,
		MediaCheck: snap.MediaCheck,
		Nodes:      snap.Nodes,
		Identities: identities,
		Qilan:      len(snap.Nodes) > 0 && snap.Nodes[0].SubTag == "Qilan",
	}, nil
}

// ingest stores a snapshot and adds it to the cache.
func (s *Store) Adapter() Adapter { return s.adapter }

// SetRefresh installs the projection rebuild hook. It must be called before the
// first Refresh so the adapter and the projection share one generation.
func (s *Store) SetRefresh(fn func(context.Context) error) {
	s.refresh = fn
}

// Reload re-reads every batch, enrichment and retirement from the adapter. Used
// after a full rebuild or a provenance reconciliation.
func (s *Store) Reload(ctx context.Context) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if reader, ok := s.adapter.(ChangeReader); ok {
		changes, err := reader.ReadChanges(ctx, -1)
		if err != nil {
			return err
		}
		return s.ApplyChanges(changes)
	}
	return s.reloadLegacy(ctx)
}

func (s *Store) reloadLegacy(ctx context.Context) error {
	rows, err := s.adapter.All(ctx)
	if err != nil {
		return err
	}
	batches := make(map[int]*batchedSnapshot, len(rows))
	for _, row := range rows {
		batch, err := s.loadBatch(row)
		if err != nil {
			continue
		}
		batches[batch.ID] = batch
	}
	enrichments, err := s.adapter.Enrichments(ctx, nil)
	if err != nil {
		return err
	}
	bySnapshot := make(map[int][]Enrichment, 64)
	for _, item := range enrichments {
		bySnapshot[item.SnapshotID] = append(bySnapshot[item.SnapshotID], item)
	}
	retired, err := s.adapter.RetiredNodes(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.batches = batches
	s.enrich = make(map[int]map[int]json.RawMessage, len(bySnapshot))
	for id, items := range bySnapshot {
		byIndex := make(map[int]json.RawMessage, len(items))
		for _, item := range items {
			byIndex[item.NodeIndex] = json.RawMessage(item.Payload)
		}
		s.enrich[id] = byIndex
	}
	s.retired = make(map[string]RetiredRow, len(retired))
	for _, row := range retired {
		s.retired[row.Identity] = row
	}
	s.rebuildOrderLocked()
	s.mu.Unlock()
	return nil
}

// RemoveBatch drops a batch from the cache after pruning.
func (s *Store) RemoveBatch(ids []int) {
	s.mu.Lock()
	for _, id := range ids {
		delete(s.batches, id)
		delete(s.enrich, id)
	}
	s.rebuildOrderLocked()
	s.mu.Unlock()
}

func (s *Store) rebuildOrderLocked() {
	ids := make([]int, 0, len(s.batches))
	for id := range s.batches {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	s.order = ids
}

// RetiredNodeIdentities is the hot-path read used before every probe.
func (s *Store) RetiredNodeIdentities() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retiredNodeIdentitiesLocked()
}

func (s *Store) retiredNodeIdentitiesLocked() map[string]struct{} {
	out := make(map[string]struct{}, len(s.retired))
	for k := range s.retired {
		out[k] = struct{}{}
	}
	return out
}

// RetiredRows returns the full retirement list for the patrol recovery pass.
func (s *Store) RetiredRows() []RetiredRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RetiredRow, 0, len(s.retired))
	for _, row := range s.retired {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

// hydrate attaches enrichment to a batch, returning independent node copies.
func (s *Store) hydrate(batch *batchedSnapshot, enrich map[int]json.RawMessage, retired map[string]RetiredRow) domain.Snapshot {
	out := domain.Snapshot{
		ID:         batch.ID,
		CheckedAt:  batch.CheckedAt,
		SpeedTest:  batch.SpeedTest,
		MediaCheck: batch.MediaCheck,
		Nodes:      make([]domain.Node, len(batch.Nodes)),
	}
	copy(out.Nodes, batch.Nodes)
	for index, payload := range enrich {
		if index >= len(out.Nodes) {
			continue
		}
		var result domain.IPPureResult
		if err := json.Unmarshal(payload, &result); err != nil {
			continue
		}
		out.Nodes[index].IPPure = &result
		domain.ApplyExitCountry(&out.Nodes[index])
	}
	// Before collection preserved original indexes, a filtered batch could
	// attach later nodes' results to earlier rows. Retain the records, but
	// rediscover ambiguous exits written after the earliest retirement.
	earliest := int64(0)
	hasRetirement := false
	for i := range out.Nodes {
		if row, ok := retired[batch.Identities[i]]; ok {
			at, _ := time.Parse(time.RFC3339, row.RetiredAt)
			if !hasRetirement || at.UnixMilli() < earliest {
				earliest = at.UnixMilli()
				hasRetirement = true
			}
		}
	}
	if hasRetirement {
		for i := range out.Nodes {
			r := out.Nodes[i].IPPure
			if r == nil {
				continue
			}
			if domain.HasVerifiedExitIP(r) {
				continue
			}
			at, _ := time.Parse(time.RFC3339, r.CheckedAt)
			if at.UnixMilli() >= earliest {
				out.Nodes[i].IPPure = nil
			}
		}
	}
	return out
}

// Aggregate folds every batch into the current node set: the newest batch that
// mentions a connection wins, and the most recent enrichment follows the
// identity across batches.
//
// The JavaScript version re-decoded payloads on every call because the worker
// thread could not share memory. Here the batches are already decoded, so the
// fold is a single pass over cached slices.
func (s *Store) Aggregate(includeRetired bool) *domain.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.aggregateLocked(includeRetired)
}

func (s *Store) aggregateLocked(includeRetired bool) *domain.Snapshot {

	batches := make([]*batchedSnapshot, 0, len(s.order))
	for _, id := range s.order {
		batches = append(batches, s.batches[id])
	}
	if len(batches) == 0 {
		return nil
	}
	sort.SliceStable(batches, func(i, j int) bool {
		ti, tj := batchTime(batches[i]), batchTime(batches[j])
		if ti != tj {
			return ti > tj
		}
		return batches[i].ID > batches[j].ID
	})

	enrichments := s.enrich
	retired := s.retired

	nodes := make(map[string]domain.Node, 2048)
	latest := make(map[string]*domain.IPPureResult, 2048)
	firstSeenOrder := make([]string, 0, 2048)

	for _, batch := range batches {
		for i := range batch.Nodes {
			identity := batch.Identities[i]
			if identity == "" {
				continue
			}
			if !includeRetired {
				if _, ok := retired[identity]; ok {
					continue
				}
			}
			if _, ok := nodes[identity]; ok {
				continue
			}
			firstSeenOrder = append(firstSeenOrder, identity)
			node := batch.Nodes[i]
			node.CheckedAt = batch.CheckedAt
			node.Index = i
			// Take the address of per-node copies: the batch values are shared
			// by every node it contains.
			batchID, speedTest, mediaCheck := batch.ID, batch.SpeedTest, batch.MediaCheck
			node.SnapshotID = &batchID
			node.SpeedTest = &speedTest
			node.MediaCheck = &mediaCheck
			nodes[identity] = node
		}
	}

	// Enrichment follows the identity for every source: a changed
	// configuration yields a new identity anyway.
	for _, batch := range batches {
		byIndex := enrichments[batch.ID]
		if len(byIndex) == 0 {
			continue
		}
		for i := range batch.Nodes {
			identity := batch.Identities[i]
			if identity == "" {
				continue
			}
			if !includeRetired {
				if _, ok := retired[identity]; ok {
					continue
				}
			}
			payload, ok := byIndex[i]
			if !ok {
				continue
			}
			var result domain.IPPureResult
			if err := json.Unmarshal(payload, &result); err != nil {
				continue
			}
			prev, ok := latest[identity]
			if !ok || newer(&result, prev) {
				copyResult := result
				latest[identity] = &copyResult
			}
		}
	}

	// The result keeps first-seen order, which for newest-first batches means
	// the order the JavaScript store produced when it iterated its insertion
	// ordered Map.
	result := make([]domain.Node, 0, len(firstSeenOrder))
	for _, identity := range firstSeenOrder {
		node := nodes[identity]
		if r, ok := latest[identity]; ok {
			node.IPPure = r
			// A source-claimed region regularly disagrees with the verified
			// exit IP. The JavaScript store classified every hydrated node,
			// single batch or aggregate, so the aggregate must too: without
			// this the view shows the claim and loses the mismatch badge.
			domain.ApplyExitCountry(&node)
		}
		result = append(result, node)
	}

	head := batches[0]
	snap := &domain.Snapshot{
		// The aggregate spans every batch, so it deliberately carries no id:
		// the JavaScript store only ever exposes batchCount, and the UI keys
		// "all batches" off that field rather than a snapshot id.
		CheckedAt:     head.CheckedAt,
		BatchCount:    len(batches),
		SpeedTest:     head.SpeedTest,
		MediaCheck:    head.MediaCheck,
		Nodes:         result,
		SourceWarning: s.snapshotWarn,
	}
	if len(result) > 0 {
		snap.SpeedTest = false
		snap.MediaCheck = false
		for _, n := range result {
			if n.SpeedTested() {
				snap.SpeedTest = true
			}
			if n.MediaChecked() {
				snap.MediaCheck = true
			}
		}
	}
	return snap
}

func newer(a, b *domain.IPPureResult) bool {
	ta, _ := time.Parse(time.RFC3339, a.CheckedAt)
	tb, _ := time.Parse(time.RFC3339, b.CheckedAt)
	return ta.UnixMilli() > tb.UnixMilli()
}

func batchTime(b *batchedSnapshot) int64 {
	if t, err := time.Parse(time.RFC3339, b.CheckedAt); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, b.ImportedAt); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// FilterDepartedNodes hides identities absent from every snapshot for longer
// than their source cadence tolerates. A returning node reappears on its next
// import, so this is a display and queue filter, never a retirement.
func (s *Store) FilterDepartedNodes(nodes []domain.Node) []domain.Node {
	s.mu.RLock()
	batches := make([]*batchedSnapshot, 0, len(s.order))
	for _, id := range s.order {
		batches = append(batches, s.batches[id])
	}
	s.mu.RUnlock()
	if len(batches) == 0 {
		return nodes
	}
	lastSeen := make(map[string]int64, 4096)
	qilan := make(map[string]bool, 4096)
	var reference int64 = -1
	for _, batch := range batches {
		at, err := time.Parse(time.RFC3339, batch.ImportedAt)
		if err != nil {
			continue
		}
		ms := at.UnixMilli()
		if ms > reference {
			reference = ms
		}
		for _, identity := range batch.Identities {
			if prev, ok := lastSeen[identity]; ok && prev >= ms {
				continue
			}
			lastSeen[identity] = ms
			qilan[identity] = batch.Qilan
		}
	}
	if reference < 0 {
		return nodes
	}
	out := nodes[:0:0]
	for _, node := range nodes {
		identity := domain.AggregateIdentity(node)
		seen, ok := lastSeen[identity]
		if !ok {
			out = append(out, node)
			continue
		}
		limit := DepartedSubs
		if qilan[identity] {
			limit = DepartedQilan
		}
		if reference-seen <= int64(limit/time.Millisecond) {
			out = append(out, node)
		}
	}
	return out
}

// Candidate is one node still worth probing.
type Candidate struct {
	SnapshotID int
	Index      int
	Node       domain.Node
	Identity   string
}

// IPPureCandidates lists connections that still need an IP result. The
// JavaScript version recomputed every identity per call; the cache carries
// them, so this is a scan over already-hashed identities.
func (s *Store) IPPureCandidates() []Candidate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ippureCandidatesLocked()
}

func (s *Store) ippureCandidatesLocked() []Candidate {
	candidates := make([]Candidate, 0, 1024)
	for _, id := range s.order {
		batch := s.batches[id]
		byIndex := s.enrich[id]
		for i := range batch.Nodes {
			identity := batch.Identities[i]
			if identity == "" {
				continue
			}
			if _, ok := s.retired[identity]; ok {
				continue
			}
			node := batch.Nodes[i]
			if node.RawLink == "" && len(node.RawConfig) == 0 {
				continue
			}
			var result domain.IPPureResult
			done := false
			if payload, ok := byIndex[i]; ok {
				if err := json.Unmarshal(payload, &result); err == nil {
					done = result.Status == "success" &&
						domain.HasVerifiedExitIP(&result) &&
						result.Organization != "" && result.Country != ""
				}
			}
			if done {
				continue
			}
			candidates = append(candidates, Candidate{
				SnapshotID: batch.ID,
				Index:      i,
				Node:       node,
				Identity:   identity,
			})
		}
	}
	return candidates
}

// CollectionSnapshot is the current batch, filtered and with original indexes
// preserved so a probe result can be written back to its own row.
type CollectionSnapshot struct {
	Snapshot   domain.Snapshot
	Indexes    []int
	Identities []string
}

// GetCollectionSnapshot returns the newest batch with retired connections
// removed, plus the original node indexes for result writes.
func (s *Store) GetCollectionSnapshot() *CollectionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectionSnapshotLocked()
}

func (s *Store) collectionSnapshotLocked() *CollectionSnapshot {
	if len(s.order) == 0 {
		return nil
	}
	batch := s.batches[s.order[0]]
	enrich := s.enrich[batch.ID]
	hydrated := s.hydrate(batch, enrich, s.retired)
	kept := make([]domain.Node, 0, len(hydrated.Nodes))
	indexes := make([]int, 0, len(hydrated.Nodes))
	identities := make([]string, 0, len(hydrated.Nodes))
	for i := range hydrated.Nodes {
		identity := batch.Identities[i]
		if _, ok := s.retired[identity]; ok {
			continue
		}
		kept = append(kept, hydrated.Nodes[i])
		indexes = append(indexes, i)
		identities = append(identities, identity)
	}
	hydrated.Nodes = kept
	return &CollectionSnapshot{Snapshot: hydrated, Indexes: indexes, Identities: identities}
}

// CollectionContext is everything one collection round needs. Building it under
// a single read lock replaces the JavaScript pattern of three separate IPC
// round-trips, each of which could observe a different refresh generation.
type CollectionContext struct {
	Snapshot    *CollectionSnapshot
	Candidates  []Candidate
	Aggregate   *domain.Snapshot
	Retired     map[string]struct{}
	VisibleSet  map[string]struct{}
	AggregateIP map[string]*domain.IPPureResult
}

func (s *Store) GetCollectionContext() CollectionContext {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := s.collectionSnapshotLocked()
	candidates := s.ippureCandidatesLocked()
	aggregate := s.aggregateLocked(false)
	retired := s.retiredNodeIdentitiesLocked()

	visible := make(map[string]struct{}, 1024)
	results := make(map[string]*domain.IPPureResult, 1024)
	if aggregate != nil {
		for i := range aggregate.Nodes {
			identity := domain.AggregateIdentity(aggregate.Nodes[i])
			visible[fmt.Sprintf("%d:%s", aggregate.Nodes[i].Batch(), identity)] = struct{}{}
			if aggregate.Nodes[i].IPPure != nil {
				results[identity] = aggregate.Nodes[i].IPPure
			}
		}
	}
	return CollectionContext{
		Snapshot:    snap,
		Candidates:  candidates,
		Aggregate:   aggregate,
		Retired:     retired,
		VisibleSet:  visible,
		AggregateIP: results,
	}
}

// Get returns one batch, or the newest when id is nil.
func (s *Store) Get(id *int) (*domain.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	batch := s.latestLocked()
	if id != nil {
		b, ok := s.batches[*id]
		if !ok {
			return nil, nil
		}
		batch = b
	}
	if batch == nil {
		return nil, nil
	}
	snap := s.hydrate(batch, s.enrich[batch.ID], s.retired)
	if id == nil {
		kept := make([]domain.Node, 0, len(snap.Nodes))
		for i := range snap.Nodes {
			if _, ok := s.retired[batch.Identities[i]]; ok {
				continue
			}
			kept = append(kept, snap.Nodes[i])
		}
		snap.Nodes = kept
	}
	if s.snapshotWarn != "" {
		snap.SourceWarning = s.snapshotWarn
	}
	return &snap, nil
}

func (s *Store) latestLocked() *batchedSnapshot {
	if len(s.order) == 0 {
		return nil
	}
	return s.batches[s.order[0]]
}

// LatestSnapshotID is the newest batch id.
func (s *Store) LatestSnapshotID() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.order) == 0 {
		return 0
	}
	return s.order[0]
}

// History lists batches for the UI.
func (s *Store) History(ctx context.Context, limit, offset int) ([]HistoryItem, int, error) {
	return s.adapter.History(ctx, limit, offset)
}

// RetireNodes soft-deletes connections by identity.
func (s *Store) RetireNodes(nodes []domain.Node, reason string) (int, error) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if len(nodes) == 0 {
		return 0, nil
	}
	seen := make(map[string]struct{}, len(nodes))
	identities := make([]string, 0, len(nodes))
	for _, n := range nodes {
		identity := domain.AggregateIdentity(n)
		if identity == "" {
			return 0, errors.New("cannot retire a node without a stable identity")
		}
		if _, ok := seen[identity]; ok {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.adapter.RetireNodes(context.Background(), identities, at, reason); err != nil {
		return 0, err
	}
	s.mu.Lock()
	for _, identity := range identities {
		if _, ok := s.retired[identity]; ok {
			continue
		}
		s.retired[identity] = RetiredRow{Identity: identity, RetiredAt: at, Reason: reason}
	}
	s.mu.Unlock()
	return len(identities), nil
}

// RestoreNode undoes one retirement by exact match.
func (s *Store) RestoreNode(identity, retiredAt, reason string) (int64, error) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	n, err := s.adapter.RestoreNode(context.Background(), identity, retiredAt, reason)
	if err == nil && n > 0 {
		s.mu.Lock()
		delete(s.retired, identity)
		s.mu.Unlock()
	}
	return n, err
}

// SaveIPPure writes one probe result. The identity is supplied so the adapter
// never has to re-read the snapshot payload.
func (s *Store) SaveIPPure(snapshotID, index int, result domain.IPPureResult, identity string) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := s.adapter.SaveIPPure(context.Background(), snapshotID, index, payload, identity); err != nil {
		return err
	}
	s.mu.Lock()
	if s.enrich[snapshotID] == nil {
		s.enrich[snapshotID] = make(map[int]json.RawMessage, 8)
	}
	s.enrich[snapshotID][index] = json.RawMessage(payload)
	s.mu.Unlock()
	return nil
}

// ReconcileSources attaches provenance to archived batches.
func (s *Store) ReconcileSources(linked map[string]domain.Source, prune bool) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	return s.adapter.Reconcile(context.Background(), linked, prune)
}

// Refresh rebuilds the persisted projection when another process changed data.
func (s *Store) Refresh(ctx context.Context) error {
	return s.refreshFlight.run(ctx, func(ctx context.Context) error {
		if err := s.Sync(ctx); err != nil {
			return err
		}
		if s.refresh != nil {
			return s.refresh(ctx)
		}
		return nil
	})
}

// Close releases the adapter.
func (s *Store) Close() error {
	return s.adapter.Close()
}
