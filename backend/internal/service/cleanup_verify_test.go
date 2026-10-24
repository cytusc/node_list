package service

import (
	"context"
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
	// Stale evidence must never remove anything.
	stale := domain.ConnectivityState{CheckedAt: time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	if ok, e := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, digest, stale); e != nil || ok {
		t.Fatalf("stale proof deleted: ok=%v err=%v", ok, e)
	}
	// A digest that does not name the stored configuration is refused.
	fresh := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ConfirmedDead: true}
	if ok, e := f.repo.DeleteVerifiedDead(ctx, f.id, incarnation, "deadbeef", fresh); e != nil || ok {
		t.Fatalf("wrong digest deleted: ok=%v err=%v", ok, e)
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
	f.exec(t, `INSERT INTO node_explorer_probe_results(session,kind,identity,checked_at,payload) VALUES($1,$2,$3,now(),'{}')`, VerificationSession(f.id), KindLive, f.id)
	wave, e = f.repo.VerifiedCandidates(ctx, 24*time.Hour, 6*time.Hour, 10)
	if e != nil || len(wave) != 0 {
		t.Fatalf("recently verified resurfaced: %+v %v", wave, e)
	}
}
