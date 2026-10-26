package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

// IngestResult reports what one import pass did.
type IngestResult struct {
	Inserted bool
	Skipped  bool
}

// ErrNoAdmissibleNodes leaves the last stored batch untouched when every
// incoming node is rejected. ImportFile also keeps its signature retryable.
var ErrNoAdmissibleNodes = errors.New("no admissible nodes in source batch")

// AdmissionFilter returns fixed rejection codes indexed by candidate position.
// Candidates have already been enriched, statically validated and deduplicated.
// Errors abort the import; a failed evidence lookup must not bypass admission.
type AdmissionFilter func(context.Context, []domain.Node) (map[int]string, error)

// Ingestor imports subs-check (or Qilan) rounds into the store.
type Ingestor struct {
	store *Store

	mu                sync.Mutex
	signature         string
	backfilled        bool
	persistentOnly    bool
	filters           []AdmissionFilter
	admissionRetry    time.Time
	admissionRejected bool
}

// NewIngestor builds an importer for one store.
func NewIngestor(s *Store, filters ...AdmissionFilter) *Ingestor {
	return &Ingestor{store: s, filters: filters}
}

// NewPersistentIngestor writes source rounds without retaining an archive cache.
// The source worker synchronizes changed batches into the registry separately.
func NewPersistentIngestor(s *Store, filters ...AdmissionFilter) *Ingestor {
	return &Ingestor{store: s, persistentOnly: true, filters: filters}
}

// ImportFile reads a detection result file and imports it when its signature
// changed. The signature covers every source file, so a rewritten subscription
// without a new detection round is still picked up.
func (i *Ingestor) ImportFile(ctx context.Context, resultsFile, clashFile, linksFile string) (IngestResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	signature, err := fileSignature(resultsFile, clashFile, linksFile)
	if err != nil {
		return IngestResult{}, err
	}
	unchanged := i.signature == signature
	if unchanged && (i.admissionRetry.IsZero() || time.Now().Before(i.admissionRetry)) {
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
	if err := i.importLocked(ctx, snap, sources); err != nil {
		return IngestResult{}, err
	}
	i.signature = signature
	i.admissionRetry = time.Time{}
	if i.admissionRejected {
		i.admissionRetry = time.Now().Add(time.Minute)
	}
	return IngestResult{Inserted: true}, nil
}

// Import validates, enriches with provenance and stores one snapshot.
func (i *Ingestor) Import(ctx context.Context, snap domain.Snapshot, sources *domain.Sources) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.importLocked(ctx, snap, sources)
}

func (i *Ingestor) importLocked(ctx context.Context, snap domain.Snapshot, sources *domain.Sources) error {
	if snap.Nodes == nil {
		return errors.New("invalid snapshot: nodes missing")
	}
	normalized := domain.Snapshot{
		CheckedAt:  snap.CheckedAt,
		SpeedTest:  snap.SpeedTest,
		MediaCheck: snap.MediaCheck,
		Nodes:      make([]domain.Node, 0, len(snap.Nodes)),
	}
	seen := make(map[string]int, len(snap.Nodes))
	i.admissionRejected = false
	duplicates := 0
	rejected := make(map[string]int)
	for _, node := range snap.Nodes {
		// Node-level dedup (REFACTOR_PLAN §5): variants of the same node (e.g.
		// vmess noise-field spellings) collapse to one AggregateIdentity; the
		// first occurrence wins its raw link/config.
		if sources != nil {
			source := domain.SourceForNode(node, sources, true)
			if source == nil && node.RawLink == "" && len(node.RawConfig) == 0 {
				return errors.New("results and node sources do not match; retry after upstream finishes publishing")
			}
			if source != nil {
				if len(node.RawConfig) == 0 && len(source.RawConfig) > 0 {
					node.RawConfig = source.RawConfig
				}
				if node.RawLink == "" && source.RawLink != "" {
					node.RawLink = source.RawLink
				}
			}
		}
		// Persist a usable fallback only when the source has no original URI.
		// Never overwrite rawLink with a normalized/generated spelling.
		if err := domain.EnsureNodeLink(&node); err != nil {
			log.Printf("ingest: share-link export unavailable: %v", err)
		}
		if err := domain.NodeAdmissionError(node); err != nil {
			var detail *domain.ShareLinkError
			if errors.As(err, &detail) {
				rejected[detail.Code]++
			} else {
				rejected["invalid-config"]++
			}
			continue
		}
		id := domain.AggregateIdentity(node)
		if index, exists := seen[id]; exists {
			// Duplicate observations may arrive config-first, URI-second.
			// Preserve the richer provenance instead of dropping that URI.
			if normalized.Nodes[index].RawLink == "" {
				normalized.Nodes[index].RawLink = node.RawLink
			}
			if normalized.Nodes[index].GeneratedLink == "" {
				normalized.Nodes[index].GeneratedLink = node.GeneratedLink
			}
			duplicates++
			continue
		}
		seen[id] = len(normalized.Nodes)
		normalized.Nodes = append(normalized.Nodes, node)
	}
	for _, filter := range i.filters {
		if len(normalized.Nodes) == 0 {
			break
		}
		denied, err := filter(ctx, normalized.Nodes)
		if err != nil {
			return fmt.Errorf("source admission failed: %w", err)
		}
		kept := make([]domain.Node, 0, len(normalized.Nodes))
		for index, node := range normalized.Nodes {
			if reason := denied[index]; reason != "" {
				rejected[reason]++
				// A mixed batch must revisit rejected connections even when
				// its upstream file stays unchanged after recovery succeeds.
				i.admissionRejected = true
				continue
			}
			kept = append(kept, node)
		}
		normalized.Nodes = kept
	}
	reasons := make([]string, 0, len(rejected))
	rejectedCount := 0
	for reason := range rejected {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		rejectedCount += rejected[reason]
		log.Printf("ingest: dropped %d node entries: %s", rejected[reason], reason)
	}
	if duplicates > 0 {
		log.Printf("ingest: dropped %d duplicate node entries by aggregate identity", duplicates)
	}
	if len(snap.Nodes) > 0 && len(normalized.Nodes) == 0 {
		return ErrNoAdmissibleNodes
	}

	needsBackfill := sources != nil && !i.backfilled && !i.persistentOnly
	if needsBackfill {
		linked, err := i.buildBackfill(normalized, sources)
		if err != nil {
			return err
		}
		if err := i.store.ReconcileSources(linked, false); err != nil {
			return err
		}
		// Reconciliation rewrote stored payloads; reload everything once.
		if err := i.reload(ctx); err != nil {
			return err
		}
		i.backfilled = true
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
	log.Printf("ingest: source batch accepted=%d rejected=%d duplicates=%d input=%d", len(normalized.Nodes), rejectedCount, duplicates, len(snap.Nodes))
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
	if i.persistentOnly {
		return nil
	}
	return i.store.Sync(ctx)
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
		File    string
		Size    int64
		ModTime int64
		System  any
		Missing bool
	}
	items := make([]entry, 0, len(files))
	for _, file := range files {
		if file == "" {
			continue
		}
		info, err := os.Stat(file)
		if err != nil {
			if os.IsNotExist(err) {
				items = append(items, entry{File: file, Missing: true})
				continue
			}
			return "", err
		}
		items = append(items, entry{
			File:    file,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
			System:  signatureStat(info.Sys()),
		})
	}
	out, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
