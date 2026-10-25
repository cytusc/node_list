package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

func TestRejectUnreachableEndpointsDropsOnlyDeadPorts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	live := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	// The control endpoint stands in for the probe path's own health.
	t.Setenv("SOURCE_PROBE_CONTROL", "127.0.0.1:"+live)

	nodes := []domain.Node{
		{Type: "http", Server: "127.0.0.1", Port: live},
		{Type: "http", Server: "127.0.0.1", Port: "1"},
		{Type: "hysteria2", Server: "127.0.0.1", Port: "1"},
	}
	denied, err := f.repo.RejectUnreachableEndpoints(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if denied[1] != "endpoint-unreachable" {
		t.Fatalf("closed port not rejected: %v", denied)
	}
	// A listening port passes; a UDP transport is never dialled over TCP.
	if _, ok := denied[0]; ok {
		t.Fatalf("reachable endpoint rejected: %v", denied)
	}
	if _, ok := denied[2]; ok {
		t.Fatalf("udp transport rejected by a tcp dial: %v", denied)
	}
}

func TestRejectUnreachableEndpointsYieldsWhenTheProbePathIsBroken(t *testing.T) {
	f := setup(t)
	// These sources are routinely 80-90% dead, so a mostly-dead batch proves
	// nothing about our egress. Only the control endpoint can decide, and here
	// it fails: silencing the source would be worse than admitting the batch.
	t.Setenv("SOURCE_PROBE_CONTROL", "127.0.0.1:1,127.0.0.1:2")
	nodes := []domain.Node{
		{Type: "http", Server: "127.0.0.1", Port: "1"},
		{Type: "http", Server: "127.0.0.1", Port: "2"},
		{Type: "http", Server: "127.0.0.1", Port: "3"},
	}
	denied, err := f.repo.RejectUnreachableEndpoints(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 0 {
		t.Fatalf("a broken probe path must admit the batch: %v", denied)
	}
}

func TestRejectUnreachableEndpointsCanBeDisabled(t *testing.T) {
	f := setup(t)
	t.Setenv("SOURCE_PROBE_SECONDS", "0")
	denied, err := f.repo.RejectUnreachableEndpoints(context.Background(), []domain.Node{
		{Type: "http", Server: "127.0.0.1", Port: "1"},
	})
	if err != nil || len(denied) != 0 {
		t.Fatalf("disabled probe still filtered: %v %v", denied, err)
	}
}

func TestRejectRecentlyDeletedHonoursCooldown(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	node := domain.Node{Type: "http", Server: "example.com", Port: "8080", RawLink: "http://example.com:8080"}
	identity := domain.AggregateIdentity(node)
	if _, err := f.repo.Pool.Exec(ctx, `INSERT INTO node_explorer_deleted_nodes(identity,deleted_at)
 VALUES($1,now()) ON CONFLICT(identity) DO UPDATE SET deleted_at=now()`, identity); err != nil {
		t.Fatal(err)
	}
	denied, err := f.repo.RejectRecentlyDeleted(ctx, []domain.Node{node})
	if err != nil {
		t.Fatal(err)
	}
	// Without this the deletion is undone on the next import, with a fresh
	// first_seen_at that also restarts the seven-day age gate.
	if denied[0] != "recently-deleted" {
		t.Fatalf("freshly deleted identity re-admitted: %v", denied)
	}
	if _, err := f.repo.Pool.Exec(ctx, `UPDATE node_explorer_deleted_nodes SET deleted_at=now()-interval '10 days' WHERE identity=$1`, identity); err != nil {
		t.Fatal(err)
	}
	denied, err = f.repo.RejectRecentlyDeleted(ctx, []domain.Node{node})
	if err != nil {
		t.Fatal(err)
	}
	// A node that may have been repaired must come back eventually.
	if len(denied) != 0 {
		t.Fatalf("cooldown never expires: %v", denied)
	}
}

func TestRegistryReappearanceInheritsTombstoneAge(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	node := domain.Node{Type: "http", Server: "reappear.example", Port: "8080", RawLink: "http://reappear.example:8080"}
	identity := domain.AggregateIdentity(node)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(domain.Snapshot{CheckedAt: stamp, Nodes: []domain.Node{node}})
	if err := f.st.Adapter().Insert(ctx, "reappear", raw, stamp, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	// It was judged dead a week ago, then the source re-delivered it. A fresh
	// first_seen_at would restart the age gate and keep a dead node forever.
	f.exec(t, `DELETE FROM node_explorer_node_registry WHERE identity=$1`, identity)
	f.exec(t, `INSERT INTO node_explorer_deleted_nodes(identity,deleted_at) VALUES($1,now()-interval '8 days')`, identity)

	later := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	raw, _ = json.Marshal(domain.Snapshot{CheckedAt: later, Nodes: []domain.Node{node}})
	if err := f.st.Adapter().Insert(ctx, "reappear", raw, later, 2); err != nil {
		t.Fatal(err)
	}
	// The fold is revision-driven; a direct adapter insert has to announce one.
	f.exec(t, `INSERT INTO node_explorer_results_changes(snapshot_id,revision)
 SELECT max(id),(SELECT coalesce(max(revision),0)+1 FROM node_explorer_results_changes) FROM node_explorer_snapshots
 ON CONFLICT(snapshot_id) DO UPDATE SET revision=EXCLUDED.revision`)
	if err := f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	var age time.Duration
	if err := f.repo.Pool.QueryRow(ctx, `SELECT now()-first_seen_at FROM node_explorer_node_registry WHERE identity=$1`, identity).Scan(&age); err != nil {
		t.Fatalf("node was not re-admitted: %v", err)
	}
	if age < 7*24*time.Hour {
		t.Fatalf("tombstone age not inherited, gate restarted at %v", age)
	}
}
