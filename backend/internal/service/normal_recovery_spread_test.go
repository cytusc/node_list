package service

import (
	"context"
	"testing"
	"time"
)

// The tiered backoff tops out at a day, so jobs that exhausted their attempts
// on the same pass used to land on the same second. The offset must stay inside
// a quarter of the interval, be stable for one key, and actually vary across
// keys — otherwise the exit lane still sees a spike.
func TestSpreadDueIsStableAndBounded(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	interval := 24 * time.Hour
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		key := string(rune('a'+i%26)) + time.Duration(i).String()
		due := spreadDue(base, key, interval)
		if again := spreadDue(base, key, interval); !again.Equal(due) {
			t.Fatalf("%s is not deterministic: %s vs %s", key, due, again)
		}
		offset := due.Sub(base)
		if offset < 0 || offset >= interval/4 {
			t.Fatalf("%s offset %s outside [0,%s)", key, offset, interval/4)
		}
		seen[offset] = true
	}
	if len(seen) < 20 {
		t.Fatalf("only %d distinct offsets across 200 keys", len(seen))
	}
	for _, short := range []time.Duration{time.Second, 5 * time.Minute, 59 * time.Minute} {
		if got := spreadDue(base, "short", short); !got.Equal(base) {
			t.Fatalf("sub-hour interval %s must not be spread: %s", short, got)
		}
	}
	// One hour is the first tier worth spreading: the offset must stay bounded.
	if got := spreadDue(base, "hour", time.Hour); got.Sub(base) < 0 || got.Sub(base) >= 15*time.Minute {
		t.Fatalf("hour interval offset %s outside [0,15m)", got.Sub(base))
	}
}

// Nodes retired in one wave used to get the identical due_at because the re-arm
// used retiredAt plus a fixed interval. 39k of them landed on two hourly spikes
// and buried the exit lane, so the re-arm must spread a wave out while staying
// deterministic per key.
func TestNormalRecoverySpreadsRetirementWave(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// The re-arm computes retiredAt + interval * offset. With the fixture's
	// 20-minute default the whole expression is already in the past, greatest()
	// pins everything to now() and there is nothing left to spread — so this
	// case has to run with the real daily ceiling.
	f.runner.Config.NormalRecoveryInterval = 24 * time.Hour
	retiredAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	keys := []string{"wave-a", "wave-b", "wave-c", "wave-d", "wave-e", "wave-f",
		"wave-g", "wave-h", "wave-i", "wave-j", "wave-k", "wave-l"}
	for _, key := range keys {
		f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,due_at)
			VALUES('normal-recovery',$1,$2::jsonb,now()+interval '30 hours')`,
			key, `{"retiredAt":"`+retiredAt+`","recoveryAttempts":0}`)
	}
	if err := f.runner.scheduleNormalRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	var distinct int
	var earliest time.Time
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(DISTINCT due_at), min(due_at) FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&distinct, &earliest); err != nil {
		t.Fatal(err)
	}
	if distinct < len(keys)/2 {
		t.Fatalf("retirement wave kept only %d distinct due_at for %d keys", distinct, len(keys))
	}
	if !earliest.After(time.Now()) {
		t.Fatalf("re-arm collapsed the wave into the past: earliest %s", earliest)
	}

	// Re-running the scheduler must not reshuffle: the offset is a key hash.
	before := map[string]time.Time{}
	rows, err := f.repo.Pool.Query(ctx, `SELECT key,due_at FROM node_explorer_work WHERE kind='normal-recovery'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var due time.Time
		if err = rows.Scan(&key, &due); err != nil {
			t.Fatal(err)
		}
		before[key] = due
	}
	rows.Close()
	if err = f.runner.scheduleNormalRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = f.repo.Pool.Query(ctx, `SELECT key,due_at FROM node_explorer_work WHERE kind='normal-recovery'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var due time.Time
		if err = rows.Scan(&key, &due); err != nil {
			t.Fatal(err)
		}
		if !due.Equal(before[key]) {
			t.Fatalf("%s moved on a second pass: %s -> %s", key, before[key], due)
		}
	}
}
