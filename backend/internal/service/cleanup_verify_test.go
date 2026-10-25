package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestDeleteVerifiedDeadHonoursGates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	var digest, incarnation string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT md5(node::text),incarnation FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&digest, &incarnation); e != nil {
		t.Fatal(e)
	}
	// Stale evidence must never remove anything, and the refusal must say why.
	stale := domain.ConnectivityState{CheckedAt: time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	if ok, e := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, digest, stale); ok || !strings.Contains(fmt.Sprint(e), "stale-or-live-proof") {
		t.Fatalf("stale proof not refused with a reason: ok=%v err=%v", ok, e)
	}
	// A digest that does not name the stored configuration is refused.
	fresh := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	if ok, e := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, "deadbeef", fresh); ok || !strings.Contains(fmt.Sprint(e), "guard") {
		t.Fatalf("wrong digest not refused with a reason: ok=%v err=%v", ok, e)
	}
	ok, e := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, digest, fresh)
	if e != nil || !ok {
		t.Fatalf("verified death not applied: ok=%v err=%v", ok, e)
	}
	var n int
	_ = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&n)
	if n != 0 {
		t.Fatal("node survived")
	}
}

func TestVerifiedCandidatesSkipsRecentlyVerified(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,now(),'{"reachable":false,"checkedAt":"2026-10-24T00:00:00Z"}')`, f.id)
	wave, e := f.repo.VerifiedCandidates(ctx, 24*time.Hour, 6*time.Hour, 10)
	if e != nil || len(wave) != 1 || wave[0].Identity != f.id || wave[0].Digest == "" || wave[0].Node.Type != "http" {
		t.Fatalf("candidate: %+v %v", wave, e)
	}
	// A fresh verdict for the same node takes it out of the next wave, so
	// successive waves cover the pool instead of retesting the same head.
	f.exec(t, `INSERT INTO node_explorer_probe_results(session,kind,identity,checked_at,payload) SELECT $1,$2,$3,now(),jsonb_build_object('verificationIncarnation',incarnation,'verificationDigest',md5(node::text)) FROM node_explorer_node_registry WHERE identity=$3`, VerificationSession(f.id), KindLive, f.id)
	wave, e = f.repo.VerifiedCandidates(ctx, 24*time.Hour, 6*time.Hour, 10)
	if e != nil || len(wave) != 0 {
		t.Fatalf("recently verified resurfaced: %+v %v", wave, e)
	}
}

func snapshotNodeCount(t *testing.T, f *fixture) int {
	t.Helper()
	var raw []byte
	if e := f.repo.Pool.QueryRow(context.Background(), `SELECT payload FROM node_explorer_snapshots ORDER BY id LIMIT 1`).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var snap domain.Snapshot
	if e := json.Unmarshal(raw, &snap); e != nil {
		t.Fatal(e)
	}
	return len(snap.Nodes)
}

func TestBulkDeletionDefersSnapshotCompaction(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	var digest, incarnation string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT md5(node::text),incarnation FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&digest, &incarnation); e != nil {
		t.Fatal(e)
	}
	proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	if ok, e := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, digest, proof); e != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, e)
	}
	// The bulk path must not rewrite the snapshot: that rewrite is what held the
	// exclusive lock for seconds per node.
	if n := snapshotNodeCount(t, f); n != 1 {
		t.Fatalf("snapshot rewritten during bulk delete: %d nodes", n)
	}
	if n, e := f.repo.CompactDeleted(ctx, 10); e != nil || n != 1 {
		t.Fatalf("compact: n=%d err=%v", n, e)
	}
	if n := snapshotNodeCount(t, f); n != 0 {
		t.Fatalf("snapshot not compacted: %d nodes", n)
	}
	// Idempotent: nothing left to compact.
	if n, e := f.repo.CompactDeleted(ctx, 10); e != nil || n != 0 {
		t.Fatalf("second compact: n=%d err=%v", n, e)
	}
}

func TestRecordVerificationKeepsAliveOutOfTheNextWave(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,now(),'{"reachable":false,"checkedAt":"2026-10-24T00:00:00Z"}')`, f.id)
	if wave, e := f.repo.VerifiedCandidates(ctx, 24*time.Hour, 6*time.Hour, 10); e != nil || len(wave) != 1 {
		t.Fatalf("candidate before: %+v %v", wave, e)
	}
	var incarnation, digest string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT incarnation,md5(node::text) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&incarnation, &digest); e != nil {
		t.Fatal(e)
	}
	// A reachable verdict must leave the age window and the recheck window, or the
	// next wave would retest the same node forever.
	proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Reachable: true, ProbeTarget: "batch-verify"}
	if e := f.repo.RecordVerification(ctx, f.id, incarnation, digest, proof); e != nil {
		t.Fatal(e)
	}
	if wave, e := f.repo.VerifiedCandidates(ctx, 24*time.Hour, 6*time.Hour, 10); e != nil || len(wave) != 0 {
		t.Fatalf("alive node resurfaced: %+v %v", wave, e)
	}
	var stamp time.Time
	if e := f.repo.Pool.QueryRow(ctx, `SELECT last_available_at FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&stamp); e != nil || time.Since(stamp) > time.Minute {
		t.Fatalf("availability not refreshed: %v %v", stamp, e)
	}
}

func TestPoolFloorRefusesDeletion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	var digest, incarnation string
	if e := f.repo.Pool.QueryRow(ctx, `SELECT md5(node::text),incarnation FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&digest, &incarnation); e != nil {
		t.Fatal(e)
	}
	// A floor above the current pool must park the deletion: emptying the
	// survival set is worse than keeping a node that will be retested later.
	t.Setenv("NODE_CLEANUP_MIN_POOL", "100000")
	proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	ok, err := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, digest, proof)
	if ok || !strings.Contains(fmt.Sprint(err), "pool-floor") {
		t.Fatalf("floor not enforced: ok=%v err=%v", ok, err)
	}
	var alive bool
	if e := f.repo.Pool.QueryRow(ctx, `SELECT available FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&alive); e != nil || !alive {
		t.Fatalf("node removed despite the floor: alive=%v err=%v", alive, e)
	}
}

func TestVerificationCacheFollowsConfigurationAndIncarnation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,now(),'{"reachable":false}')`, f.id)
	wave, err := f.repo.VerifiedCandidates(ctx, 7*24*time.Hour, 6*time.Hour, 10)
	if err != nil || len(wave) != 1 {
		t.Fatalf("initial wave: %+v %v", wave, err)
	}
	original := wave[0]
	proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: "config rejected"}
	if err = f.repo.RecordVerification(ctx, original.Identity, original.Incarnation, original.Digest, proof); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{
		`UPDATE node_explorer_node_registry SET node=node || '{"rawConfig":{"password":"replacement"}}'`,
		`UPDATE node_explorer_node_registry SET incarnation='replacement-incarnation'`,
	} {
		f.exec(t, mutation)
		wave, err = f.repo.VerifiedCandidates(ctx, 7*24*time.Hour, 6*time.Hour, 10)
		if err != nil || len(wave) != 1 {
			t.Fatalf("replacement suppressed: %+v %v", wave, err)
		}
		staleAlive := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Reachable: true}
		if err = f.repo.RecordVerification(ctx, original.Identity, original.Incarnation, original.Digest, staleAlive); err == nil {
			t.Fatal("replaced config accepted stale liveness")
		}
		current := wave[0]
		if err = f.repo.RecordVerification(ctx, current.Identity, current.Incarnation, current.Digest, proof); err != nil {
			t.Fatal(err)
		}
	}
	var lastAvailable *time.Time
	if err = f.repo.Pool.QueryRow(ctx, `SELECT last_available_at FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&lastAvailable); err != nil || lastAvailable != nil {
		t.Fatalf("stale proof refreshed availability: %v %v", lastAvailable, err)
	}
}

func TestVetoCommitsObservationForNextWave(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days'`)
	f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,now(),'{"reachable":false}')`, f.id)
	wave, err := f.repo.VerifiedCandidates(ctx, 7*24*time.Hour, 6*time.Hour, 10)
	if err != nil || len(wave) != 1 {
		t.Fatalf("initial wave: %+v %v", wave, err)
	}
	t.Setenv("NODE_CLEANUP_MIN_POOL", "500")
	c := wave[0]
	proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	if deleted, err := f.repo.DeleteVerifiedDead(ctx, c.Identity, c.Incarnation, c.Digest, proof); deleted || !strings.Contains(fmt.Sprint(err), "pool-floor") {
		t.Fatalf("floor: %v %v", deleted, err)
	}
	wave, err = f.repo.VerifiedCandidates(ctx, 7*24*time.Hour, 6*time.Hour, 10)
	if err != nil || len(wave) != 0 {
		t.Fatalf("veto caused repeated probe: %+v %v", wave, err)
	}
}

func TestConcurrentDeletionsRespectPoolFloor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	t.Setenv("NODE_CLEANUP_MIN_POOL", "1")
	f.exec(t, `UPDATE node_explorer_node_registry SET first_seen_at=now()-interval '8 days';
 INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index,first_seen_at)
 SELECT identity||suffix,node,snapshot_id,node_index,first_seen_at FROM node_explorer_node_registry CROSS JOIN (VALUES('-a'),('-b')) extras(suffix)`)
	rows, err := f.repo.Pool.Query(ctx, `SELECT identity,incarnation,md5(node::text) FROM node_explorer_node_registry`)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []VerifiedCandidate
	for rows.Next() {
		var c VerifiedCandidate
		if err = rows.Scan(&c.Identity, &c.Incarnation, &c.Digest); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, len(candidates))
	for _, c := range candidates {
		go func() {
			proof := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
			deleted, err := f.repo.DeleteVerifiedDead(ctx, c.Identity, c.Incarnation, c.Digest, proof)
			if !deleted && !strings.Contains(fmt.Sprint(err), "pool-floor") {
				results <- fmt.Errorf("unexpected veto: %v", err)
				return
			}
			results <- nil
		}()
	}
	for range candidates {
		if err = <-results; err != nil {
			t.Fatal(err)
		}
	}
	var remaining, deleted int
	if err = f.repo.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM node_explorer_node_registry WHERE available),(SELECT count(*) FROM node_explorer_deleted_nodes)`).Scan(&remaining, &deleted); err != nil || remaining != 1 || deleted != 2 {
		t.Fatalf("remaining=%d deleted=%d err=%v", remaining, deleted, err)
	}
}
