package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestRegistryPreservesOriginalThroughConfigOnlyRoundsAndCompaction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := domain.ProxyConfig{"name": "n", "type": "http", "server": "example.com", "port": 8080}
	n := domain.SummarizeProxy(p)
	n.RawConfig = p
	identity := domain.AggregateIdentity(n)
	insert := func(index int) {
		t.Helper()
		snap := domain.Snapshot{CheckedAt: time.Now().Add(time.Duration(index) * time.Minute).UTC().Format(time.RFC3339Nano), Nodes: []domain.Node{n}}
		raw, _ := json.Marshal(snap)
		if err := f.st.Adapter().Insert(ctx, snap.CheckedAt, raw, snap.CheckedAt, 1); err != nil {
			t.Fatal(err)
		}
		if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
			t.Fatal(err)
		}
	}
	insert(1)
	var generated string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT node->>'generatedLink' FROM node_explorer_node_registry WHERE identity=$1`, identity).Scan(&generated); err != nil || generated == "" {
		t.Fatalf("old config-only record not backfilled: %v", err)
	}
	original := "http://example.com:8080#upstream%20original"
	n.RawLink = original
	insert(2)
	n.RawLink = ""
	insert(3)
	var got string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT node->>'rawLink' FROM node_explorer_node_registry WHERE identity=$1`, identity).Scan(&got); err != nil || got != original {
		t.Fatalf("config-only update erased original: %v", err)
	}
	var count int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_links WHERE link=$1 AND origin='original'`, original).Scan(&count); err != nil || count != 1 {
		t.Fatalf("original not durably archived: %d %v", count, err)
	}
	// The retained source can be rewritten without any credentials from the URI;
	// rebuilding must recover the original from the independent link archive.
	f.exec(t, `UPDATE node_explorer_node_registry SET node=node-'rawLink' WHERE identity=$1`, identity)
	insert(4)
	if err := f.repo.Pool.QueryRow(ctx, `SELECT node->>'rawLink' FROM node_explorer_node_registry WHERE identity=$1`, identity).Scan(&got); err != nil || got != original {
		t.Fatalf("original not recovered from link archive: %v", err)
	}
	p["password"] = "different"
	p["username"] = "user"
	n.RawConfig = p
	insert(5)
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE identity=$1 AND coalesce(node->>'rawLink','')<>''`, domain.AggregateIdentity(n)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("borrowed original from different credentials: %d %v", count, err)
	}
}

func TestHistoricalVMessOriginalRecoveredWithoutNewDetectionRound(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p := domain.ProxyConfig{"name": "old vmess", "type": "vmess", "server": "example.com", "port": 443, "uuid": "11111111-1111-4111-8111-111111111111"}
	n := domain.SummarizeProxy(p)
	n.RawConfig = p
	snap := domain.Snapshot{CheckedAt: "2026-10-26T00:00:00Z", Nodes: []domain.Node{n}}
	raw, _ := json.Marshal(snap)
	if err := f.st.Adapter().Insert(ctx, "historical-vmess", raw, snap.CheckedAt, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	original, err := domain.ConfigShareLink(p)
	if err != nil {
		t.Fatal(err)
	}
	links, err := domain.ParseShareLinks(original)
	if err != nil {
		t.Fatal(err)
	}
	count, err := f.repo.recoverSourceLinks(ctx, &domain.Sources{Links: links})
	if err != nil || count != 1 {
		t.Fatalf("historical original not restored: %d %v", count, err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT node->>'rawLink' FROM node_explorer_node_registry WHERE identity=$1`, domain.AggregateIdentity(n)).Scan(&got); err != nil || got != original {
		t.Fatalf("recovered original unavailable: %v", err)
	}
	count, err = f.repo.recoverSourceLinks(ctx, &domain.Sources{Links: links})
	if err != nil || count != 0 {
		t.Fatalf("recovery is not idempotent: %d %v", count, err)
	}
}

func TestUnsupportedAndUnmatchedSourceURIsAreStillArchivedVerbatim(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	original := "future-protocol://example.com:443?future=%2f#Name%20Encoded"
	count, err := f.repo.recoverSourceLinks(ctx, &domain.Sources{Links: map[string]string{"future": original}})
	if err != nil || count != 0 {
		t.Fatalf("unmatched entry attached to a node: %d %v", count, err)
	}
	var got string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT link FROM node_explorer_node_links WHERE connection_key LIKE 'unmatched:%' AND origin='original'`).Scan(&got); err != nil || got != original {
		t.Fatalf("unsupported source URI discarded: %v", err)
	}
}
