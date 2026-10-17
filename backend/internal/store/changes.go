package store

import (
	"context"
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/domain"
)

// Changes is one consistent database generation. SnapshotIDs includes deleted
// batches and batches whose enrichment set is now empty.
type Changes struct {
	Revision       int64
	Full           bool
	SnapshotIDs    []int
	Batches        []SnapshotRow
	Enrichments    []Enrichment
	Retired        []RetiredRow
	RetiredChanged bool
	Runtime        map[string]domain.NodeState
}

// ChangeReader allows independent processes to follow compact per-batch versions.
type ChangeReader interface {
	ReadChanges(context.Context, int64) (Changes, error)
}

// Sync checks a cheap revision and reads only affected batches. Local writes and
// synchronization share ioMu, so an older database read cannot undo a local save.
// A read-through store retains nothing, so there is no generation to sync.
func (s *Store) Sync(ctx context.Context) error {
	if s.readThrough {
		return nil
	}
	return s.syncFlight.run(ctx, func(ctx context.Context) error {
		s.ioMu.Lock()
		defer s.ioMu.Unlock()
		reader, ok := s.adapter.(ChangeReader)
		if !ok {
			return s.reloadLegacy(ctx)
		}
		s.mu.RLock()
		revision := s.revision
		s.mu.RUnlock()
		changes, err := reader.ReadChanges(ctx, revision)
		if err != nil {
			return err
		}
		return s.ApplyChanges(changes)
	})
}

// ApplyChanges swaps a whole generation under one lock. Decode errors leave the
// previous generation intact and are reported for retry. A read-through store
// retains nothing, so there is no generation to swap.
func (s *Store) ApplyChanges(c Changes) error {
	if s.readThrough {
		return nil
	}
	decoded := make(map[int]*batchedSnapshot, len(c.Batches))
	for _, row := range c.Batches {
		batch, err := s.loadBatch(row)
		if err != nil {
			return err
		}
		decoded[row.ID] = batch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Full {
		s.batches = make(map[int]*batchedSnapshot, len(decoded))
		s.enrich = make(map[int]map[int]json.RawMessage)
	}
	for _, id := range c.SnapshotIDs {
		delete(s.batches, id)
		delete(s.enrich, id)
	}
	for id, batch := range decoded {
		s.batches[id] = batch
	}
	for _, item := range c.Enrichments {
		if s.enrich[item.SnapshotID] == nil {
			s.enrich[item.SnapshotID] = make(map[int]json.RawMessage)
		}
		s.enrich[item.SnapshotID][item.NodeIndex] = json.RawMessage(item.Payload)
	}
	if c.Full || c.RetiredChanged {
		s.retired = make(map[string]RetiredRow, len(c.Retired))
		for _, row := range c.Retired {
			s.retired[row.Identity] = row
		}
	}
	s.revision = c.Revision
	if c.Runtime != nil {
		s.runtime = c.Runtime
	}
	if c.Full || len(c.SnapshotIDs) > 0 || len(decoded) > 0 {
		s.rebuildOrderLocked()
	}
	return nil
}
