package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// IngestResult reports what one import pass did.
type IngestResult struct {
	Inserted bool
	Skipped  bool
}

// Ingestor imports subs-check (or Qilan) rounds into the store.
type Ingestor struct {
	store *Store

	mu         sync.Mutex
	signature  string
	backfilled bool
}

// NewIngestor builds an importer for one store.
func NewIngestor(s *Store) *Ingestor {
	return &Ingestor{store: s}
}

// ImportFile reads a detection result file and imports it when its signature
// changed. The signature covers every source file, so a rewritten subscription
// without a new detection round is still picked up.
func (i *Ingestor) ImportFile(ctx context.Context, resultsFile, clashFile, linksFile string) (IngestResult, error) {
	signature, err := fileSignature(resultsFile, clashFile, linksFile)
	if err != nil {
		return IngestResult{}, err
	}
	i.mu.Lock()
	unchanged := i.signature == signature
	i.mu.Unlock()
	if unchanged {
		return IngestResult{Skipped: true}, nil
	}
	raw, err := os.ReadFile(resultsFile)
	if err != nil {
		return IngestResult{}, err
	}
	var snap domain.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return IngestResult{}, fmt.Errorf("parse %s: %w", resultsFile, err)
	}
	var sources *domain.Sources
	// A source that has not been published yet is not an error: subscription
	// rounds arrive with credentials already embedded, and the share-only
	// deployment simply has no Clash file at all.
	if clashFile != "" && fileExists(clashFile) {
		sources, err = domain.ReadNodeSources(clashFile, linksFile)
		if err != nil {
			return IngestResult{}, err
		}
	} else if linksFile != "" && fileExists(linksFile) {
		sources, err = domain.ReadShareSources(linksFile)
		if err != nil {
			return IngestResult{}, err
		}
	}
	if err := i.Import(ctx, snap, sources); err != nil {
		return IngestResult{}, err
	}
	i.mu.Lock()
	i.signature = signature
	i.mu.Unlock()
	return IngestResult{Inserted: true}, nil
}

// Import validates, enriches with provenance and stores one snapshot.
func (i *Ingestor) Import(ctx context.Context, snap domain.Snapshot, sources *domain.Sources) error {
	if snap.Nodes == nil {
		return errors.New("invalid snapshot: nodes missing")
	}
	normalized := domain.Snapshot{
		CheckedAt:  snap.CheckedAt,
		SpeedTest:  snap.SpeedTest,
		MediaCheck: snap.MediaCheck,
		Nodes:      make([]domain.Node, 0, len(snap.Nodes)),
	}
	for _, node := range snap.Nodes {
		if sources != nil {
			source := domain.SourceForNode(node, sources, true)
			if source == nil && node.RawLink == "" && len(node.RawConfig) == 0 {
				return errors.New("results and node sources do not match; retry after upstream finishes publishing")
			}
			if source != nil {
				if len(source.RawConfig) > 0 {
					node.RawConfig = source.RawConfig
				}
				if source.RawLink != "" {
					node.RawLink = source.RawLink
				}
			}
		}
		normalized.Nodes = append(normalized.Nodes, node)
	}

	i.mu.Lock()
	needsBackfill := sources != nil && !i.backfilled
	i.mu.Unlock()
	if needsBackfill {
		linked, err := i.buildBackfill(normalized, sources)
		if err != nil {
			return err
		}
		if err := i.store.ReconcileSources(linked, false); err != nil {
			return err
		}
		i.mu.Lock()
		i.backfilled = true
		i.mu.Unlock()
		// Reconciliation rewrote stored payloads; reload everything once.
		if err := i.reload(ctx); err != nil {
			return err
		}
	}

	fingerprint, err := domain.SnapshotFingerprint(normalized)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	if err := i.store.adapter.Insert(ctx, fingerprint, payload, normalized.CheckedAt, len(normalized.Nodes)); err != nil {
		return err
	}
	return i.reload(ctx)
}

// buildBackfill attaches credentials to every archived batch using the current
// subscription, then adds the incoming round.
func (i *Ingestor) buildBackfill(snap domain.Snapshot, sources *domain.Sources) (map[string]domain.Source, error) {
	rows, err := i.store.adapter.All(context.Background())
	if err != nil {
		return nil, err
	}
	linked := make(map[string]domain.Source, 8192)
	for _, row := range rows {
		var old domain.Snapshot
		if err := json.Unmarshal(row.Payload, &old); err != nil {
			continue
		}
		for _, node := range old.Nodes {
			if source := domain.SourceForNode(node, sources, false); source != nil {
				linked[domain.SourceKey(node)] = *source
			}
		}
	}
	for _, node := range snap.Nodes {
		source := domain.Source{RawLink: node.RawLink}
		if len(node.RawConfig) > 0 {
			source.RawConfig = node.RawConfig
		}
		linked[domain.SourceKey(node)] = source
	}
	return linked, nil
}

// reload refreshes the in-memory batches from the database.
func (i *Ingestor) reload(ctx context.Context) error {
	rows, err := i.store.adapter.All(ctx)
	if err != nil {
		return err
	}
	batches := make(map[int]*batchedSnapshot, len(rows))
	for _, row := range rows {
		batch, err := i.store.loadBatch(row)
		if err != nil {
			continue
		}
		batches[batch.ID] = batch
	}
	enrichments, err := i.store.adapter.Enrichments(ctx, nil)
	if err != nil {
		return err
	}
	bySnapshot := make(map[int]map[int]json.RawMessage, 64)
	for _, item := range enrichments {
		if bySnapshot[item.SnapshotID] == nil {
			bySnapshot[item.SnapshotID] = make(map[int]json.RawMessage, 64)
		}
		bySnapshot[item.SnapshotID][item.NodeIndex] = json.RawMessage(item.Payload)
	}
	retired, err := i.store.adapter.RetiredNodes(ctx)
	if err != nil {
		return err
	}
	i.store.mu.Lock()
	i.store.batches = batches
	i.store.enrich = bySnapshot
	i.store.retired = make(map[string]RetiredRow, len(retired))
	for _, row := range retired {
		i.store.retired[row.Identity] = row
	}
	i.store.rebuildOrderLocked()
	i.store.mu.Unlock()
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// fileSignature fingerprints the upstream files by inode, size and timestamps.
// Statting three files is far cheaper than re-parsing a megabyte of YAML, and
// it is what keeps the five-second import timer from doing real work.
func fileSignature(files ...string) (string, error) {
	type entry struct {
		file string
		ino  uint64
		size int64
		mtim int64
		ctim int64
		miss bool
	}
	items := make([]entry, 0, len(files))
	for _, file := range files {
		if file == "" {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			if os.IsNotExist(err) {
				items = append(items, entry{file: file, miss: true})
				continue
			}
			return "", err
		}
		items = append(items, entry{
			file: file,
			size: info.Size(),
			mtim: info.ModTime().UnixNano(),
		})
	}
	out, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
