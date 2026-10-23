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
	if got := spreadDue(base, "short", time.Second); !got.Equal(base) {
		t.Fatalf("sub-second interval must not be spread: %s", got)
	}
}

// Nodes retired in one wave used to get the identical due_at because the re-arm
// used retiredAt plus a fixed interval. 39k of them landed on two hourly spikes
// and buried the exit lane, so the re-arm must spread a wave out while staying
// deterministic per key.
func TestNormalRecoverySpreadsRetirementWave(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	retiredAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	for _, key := range []string{"wave-a", "wave-b", "wave-c", "wave-d", "wave-e", "wave-f"} {
		f.exec(t, `INSERT INTO node_explorer_work(kind,key,payload,due_at)
			VALUES('normal-recovery',$1,$2::jsonb,now()+interval '30 hours')`,
			key, `{"retiredAt":"`+retiredAt+`","recoveryAttempts":0}`)
	}
	if err := f.runner.scheduleNormalRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	var distinct int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(DISTINCT due_at) FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct < 2 {
		t.Fatalf("retirement wave kept a single due_at: %d", distinct)
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
