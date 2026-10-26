package service

import (
	"context"
	"testing"
)

// Risk queries must not depend on the scored node's own tunnel: any recently
// verified node can carry them, and the pool has to exclude the node itself,
// anything whose last reachability probe failed.
func TestCarriersOnlyReturnsVerifiedLiveNodes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	if got, err := f.runner.carriers(ctx, "", 3); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("returned %d carriers before any reachability evidence", len(got))
	}

	f.evidence(t, "carrier")
	got, err := f.runner.carriers(ctx, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("carriers=%d want 1", len(got))
	}
	if got[0].Server != "192.0.2.1" {
		t.Fatalf("carrier server=%q", got[0].Server)
	}

	if got, err = f.runner.carriers(ctx, f.id, 3); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("excluded identity still returned %d carriers", len(got))
	}

	// Availability is authoritative; legacy retirement rows cannot override it.
	f.exec(t, `UPDATE node_explorer_node_registry SET available=false WHERE identity=$1`, f.id)
	if got, err = f.runner.carriers(ctx, "", 3); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Fatalf("unavailable node returned as carrier: %d", len(got))
	}
}

// Reachability older than a day must drop out of the pool: a carrier last seen
// working yesterday is not worth leasing again.
func TestCarriersDropsStaleReachability(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "stale")
	f.exec(t, `UPDATE node_explorer_connectivity SET checked_at = now() - interval '25 hours'`)
	got, err := f.runner.carriers(ctx, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale carrier returned: %d", len(got))
	}
}
