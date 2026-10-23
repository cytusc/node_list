package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

func TestSourceAdmissionUsesConfirmedConnectionAndPreservesRecovery(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	original := domain.Node{Type: "http", Server: "example.com", Port: "80", RawLink: "http://example.com:80"}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(domain.Snapshot{CheckedAt: stamp, Nodes: []domain.Node{original}})
	if err := f.st.Adapter().Insert(ctx, "admission", raw, stamp, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	id := domain.AggregateIdentity(original)
	proxy, err := domain.ShareLinkProxy(original.RawLink, "other source")
	if err != nil {
		t.Fatal(err)
	}
	equivalent := original
	equivalent.RawLink, equivalent.RawConfig = "", proxy
	changed := original
	changed.RawLink = "http://new-user:new-secret@example.com:80"
	nodes := []domain.Node{original, equivalent, changed}
	check := func(want int) {
		t.Helper()
		denied, err := f.repo.RejectConfirmedFaults(ctx, nodes)
		if err != nil || len(denied) != want || denied[2] != "" {
			t.Fatalf("incorrect admission result: %v %v", denied, err)
		}
	}
	// An exit lookup failure alone is insufficient, even if connectivity failed.
	f.exec(t, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,'',now(),'{"error":"出口探测失败"}')`, id)
	f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,now(),'{"reachable":false}')`, id)
	check(0)
	for _, reason := range []string{"manual disable", "IPPure: policy"} {
		f.exec(t, `INSERT INTO node_explorer_retired_nodes VALUES($1,$2,$3) ON CONFLICT(identity) DO UPDATE SET reason=EXCLUDED.reason`, id, stamp, reason)
		check(0)
	}
	f.exec(t, `UPDATE node_explorer_retired_nodes SET reason=$2 WHERE identity=$1`, id, store.RetireRestoreReason)
	check(2) // Both representations of the exact same connection are blocked.
	large := make([]domain.Node, 1001)
	for index := range large {
		large[index] = domain.Node{RawLink: "http://candidate-" + strconv.Itoa(index) + ".example:80"}
	}
	large = append(large, original)
	if denied, err := f.repo.RejectConfirmedFaults(ctx, large); err != nil || len(denied) != 1 || denied[len(large)-1] == "" {
		t.Fatalf("chunked admission missed final candidate: %v %v", denied, err)
	}
	importer := store.NewPersistentIngestor(f.st, f.repo.RejectConfirmedFaults)
	if err := importer.Import(ctx, domain.Snapshot{CheckedAt: stamp, Nodes: nodes[:2]}, nil); !errors.Is(err, store.ErrNoAdmissibleNodes) {
		t.Fatalf("confirmed faulty node reimported: %v", err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	target, err := f.repo.Target(ctx, id)
	if err != nil || target == nil || target.Retired == nil {
		t.Fatalf("filter destroyed recovery input: %+v %v", target, err)
	}
	f.exec(t, `DELETE FROM node_explorer_retired_nodes WHERE identity=$1`, id)
	check(0)
	if err := importer.Import(ctx, domain.Snapshot{CheckedAt: stamp, Nodes: nodes[:1]}, nil); err != nil {
		t.Fatalf("restored connection could not reenter: %v", err)
	}
}
