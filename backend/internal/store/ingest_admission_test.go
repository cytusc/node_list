package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

type admissionTestAdapter struct {
	Adapter
	payloads [][]byte
}

func (a *admissionTestAdapter) Insert(_ context.Context, _ string, payload []byte, _ string, _ int) error {
	a.payloads = append(a.payloads, append([]byte(nil), payload...))
	return nil
}

func TestMixedAdmissionRechecksUnchangedFileAfterRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "results.json")
	snap := domain.Snapshot{Nodes: []domain.Node{{RawLink: "http://example.com:80"}, {RawLink: "http://example.org:80"}}}
	raw, _ := json.Marshal(snap)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	adapter := &admissionTestAdapter{}
	deny, calls := true, 0
	i := NewPersistentIngestor(New(adapter, nil), func(context.Context, []domain.Node) (map[int]string, error) {
		calls++
		if deny {
			return map[int]string{0: "confirmed-retired-connection"}, nil
		}
		return nil, nil
	})
	if result, err := i.ImportFile(ctx, path, "", ""); err != nil || !result.Inserted || i.admissionRetry.IsZero() {
		t.Fatalf("mixed batch did not schedule retry: %+v %v", result, err)
	}
	if result, err := i.ImportFile(ctx, path, "", ""); err != nil || !result.Skipped || calls != 1 {
		t.Fatalf("unchanged file bypassed retry backoff: %+v %v", result, err)
	}
	// Successful Qilan/public imports must not erase a file's revisit.
	deny = false
	if err := i.Import(ctx, domain.Snapshot{Nodes: snap.Nodes[1:]}, nil); err != nil {
		t.Fatal(err)
	}
	if i.admissionRetry.IsZero() {
		t.Fatal("other source erased the file retry")
	}
	i.admissionRetry = time.Now().Add(-time.Second)
	if result, err := i.ImportFile(ctx, path, "", ""); err != nil || !result.Inserted || calls != 3 {
		t.Fatalf("mixed batch did not revisit restored connection: %+v %v", result, err)
	}
	var first, last domain.Snapshot
	if err := json.Unmarshal(adapter.payloads[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(adapter.payloads[len(adapter.payloads)-1], &last); err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != 1 || len(last.Nodes) != 2 || !i.admissionRetry.IsZero() {
		t.Fatal("recovery did not readmit the previously rejected connection")
	}
}
