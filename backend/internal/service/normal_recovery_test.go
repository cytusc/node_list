package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func claimNormal(t *testing.T, f *fixture) Job {
	t.Helper()
	var ref string
	if err := f.repo.Pool.QueryRow(context.Background(), `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind='normal-recovery' AND state='queued' ORDER BY id LIMIT 1`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	j, err := f.repo.Claim(context.Background(), KindNormalRecovery, ref, false)
	if err != nil || j == nil {
		t.Fatalf("normal claim: %v", err)
	}
	return *j
}

func queueNormal(t *testing.T, f *fixture) Job {
	t.Helper()
	f.retirement(t)
	f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
	f.runner.Config.Role = "recovery"
	if err := f.runner.schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE node_explorer_work SET due_at=now() WHERE kind='normal-recovery'`)
	return claimNormal(t, f)
}

func completeNormal(t *testing.T, f *fixture, j Job, classification string) {
	t.Helper()
	ctx := context.Background()
	if err := f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("initial dependencies: %v", err)
	}
	session := "recovery:" + j.Ref()
	f.evidence(t, session)
	f.evidence(t, session+":verify")
	if classification == "missing" {
		f.exec(t, `DELETE FROM node_explorer_ip_attributes`)
	} else {
		f.exec(t, `UPDATE node_explorer_ip_attributes SET payload='{"ip":"8.8.4.4","asn":"AS13335","classification":{"ip":"8.8.4.4","state":"datacenter"}}'`)
	}
	if err := f.runner.recovery(ctx, j); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->>'recoveryMode' FROM node_explorer_work WHERE kind='lifecycle'`).Scan(&mode); err != nil || mode != "normal" {
		t.Fatalf("mode: %s %v", mode, err)
	}
	var high int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind IN ('connectivity','exit') AND priority>0`).Scan(&high); err != nil || high != 0 {
		t.Fatalf("used high probe lane: %d %v", high, err)
	}
}

func TestNormalRecoveryRestoresWithoutResidentialClassification(t *testing.T) {
	for _, classification := range []string{"hosting", "missing"} {
		t.Run(classification, func(t *testing.T) {
			f := setup(t)
			j := queueNormal(t, f)
			if j.Payload.StartedAt.IsZero() {
				t.Fatal("start not persisted")
			}
			completeNormal(t, f, j, classification)
			transition := f.claim(t, KindLifecycle)
			if err := f.runner.lifecycle(context.Background(), transition); err != nil {
				t.Fatal(err)
			}
			target, err := f.repo.Target(context.Background(), f.id)
			if err != nil || target.Retired != nil {
				t.Fatalf("normal not restored: %+v %v", target, err)
			}
			var history int
			if err = f.repo.Pool.QueryRow(context.Background(), `SELECT jsonb_array_length(payload->'recoveryHistory') FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&history); err != nil || history != 1 {
				t.Fatal("missing history", err)
			}
		})
	}
}

func TestNormalRecoveryLifecycleRechecksFences(t *testing.T) {
	for _, change := range []string{"manual", "new-retirement", "source-deleted", "unavailable", "unreachable", "exit-changed", "stale-proof"} {
		t.Run(change, func(t *testing.T) {
			f := setup(t)
			j := queueNormal(t, f)
			completeNormal(t, f, j, "hosting")
			switch change {
			case "manual":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual'`)
			case "new-retirement":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET retired_at='new-retirement'`)
			case "source-deleted":
				f.exec(t, `DELETE FROM node_explorer_snapshots`)
			case "unavailable":
				f.exec(t, `UPDATE node_explorer_node_registry SET available=false`)
			case "unreachable":
				f.exec(t, `UPDATE node_explorer_connectivity SET payload=jsonb_set(payload,'{reachable}','false')`)
			case "exit-changed":
				f.exec(t, `UPDATE node_explorer_exits SET version=version+1`)
			case "stale-proof":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{checkedAt}','"2000-01-01T00:00:00Z"')`)
			}
			transition := f.claim(t, KindLifecycle)
			if err := f.runner.lifecycle(context.Background(), transition); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_retired_nodes`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("fence %s failed: %d %v", change, count, err)
			}
		})
	}
}

func TestNormalRecoveryRoutingAndPersistentCadence(t *testing.T) {
	for _, scenario := range []string{"nonhistorical", "hosting", "nonresidential", "residential", "transient", "manual", "source-deleted", "unavailable", "fast-active"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			p := f.retirement(t)
			want := false
			switch scenario {
			case "nonhistorical":
				want = true
				f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
			case "hosting", "nonresidential", "transient", "fast-active":
				result := "probe-failed"
				if scenario == "hosting" {
					result = "skipped-datacenter-asn"
					want = true
				}
				if scenario == "nonresidential" {
					result = "not-residential"
					want = true
				}
				p.RecoveryResult = result
				if err := f.repo.Schedule(context.Background(), KindRecovery, f.id+":"+p.RetiredAt, p, 1, time.Now()); err != nil {
					t.Fatal(err)
				}
				if scenario != "fast-active" {
					f.exec(t, `UPDATE node_explorer_work SET state='done'`)
				} else {
					f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
				}
			case "manual":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual'`)
			case "source-deleted":
				f.exec(t, `DELETE FROM node_explorer_snapshots`)
			case "unavailable":
				f.exec(t, `UPDATE node_explorer_node_registry SET available=false`)
			}
			f.runner.Config.Role = "recovery"
			if err := f.runner.schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT count(*) FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != want {
				t.Fatalf("routing: count %d want %v", count, want)
			}
			if !want {
				return
			}
			var due, after time.Time
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT due_at FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&due); err != nil {
				t.Fatal(err)
			}
			if due.Before(time.Now().Add(-time.Second)) || due.After(time.Now().Add(5*time.Minute)) {
				t.Fatalf("first attempt too early/late: %v", due)
			}
			if err := f.runner.schedule(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := f.repo.Pool.QueryRow(context.Background(), `SELECT due_at FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&after); err != nil || !due.Equal(after) {
				t.Fatal("restart moved deadline", err)
			}
		})
	}
}

func TestNormalRecoveryRetriesWithFreshProofsAndNoFastPromotion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j := queueNormal(t, f)
	if err := f.repo.Finish(ctx, j, func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, j, "probe-failed", "") }); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var generation int64
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&due, &generation); err != nil {
		t.Fatal(err)
	}
	if due.Before(time.Now().Add(19*time.Minute)) || generation != j.Generation+1 {
		t.Fatalf("wrong retry deadline/generation: %v %d", due, generation)
	}
	next, err := f.repo.Claim(ctx, KindNormalRecovery, fmt.Sprintf("%d:%d", j.ID, generation), false)
	if err != nil || next != nil {
		t.Fatal("claimed before interval", err)
	}
	f.runner.Config.Role = "residential"
	if err = f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var high int
	if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='recovery'`).Scan(&high); err != nil || high != 0 {
		t.Fatalf("nonresidential in fast service: %d %v", high, err)
	}
	f.evidence(t, "recovery:"+j.Ref())
	f.exec(t, `UPDATE node_explorer_work SET due_at=now() WHERE id=$1`, j.ID)
	nextJob := claimNormal(t, f)
	if err = f.runner.recovery(ctx, nextJob); !isWaiting(err) {
		t.Fatalf("reused old proof: %v", err)
	}
}

func TestNormalAdmissionIsIndependentAndBoundedAcrossReplicas(t *testing.T) {
	for _, limit := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			f.repo.NormalRecoveryConcurrency = limit
			replica := &Repository{Pool: f.repo.Pool, NormalRecoveryConcurrency: limit}
			p := f.retirement(t)
			var refs []string
			for i := 0; i < 8; i++ {
				if err := f.repo.Schedule(ctx, KindNormalRecovery, fmt.Sprint(i), p, 0, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := f.repo.Pool.Query(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var ref string
				if err = rows.Scan(&ref); err != nil {
					t.Fatal(err)
				}
				refs = append(refs, ref)
			}
			rows.Close()
			var wg sync.WaitGroup
			claims := make(chan *Job, len(refs))
			for i, ref := range refs {
				wg.Add(1)
				go func(i int, ref string) {
					defer wg.Done()
					repo := f.repo
					if i%2 == 0 {
						repo = replica
					}
					j, e := repo.Claim(ctx, KindNormalRecovery, ref, false)
					if e != nil {
						t.Error(e)
					}
					claims <- j
				}(i, ref)
			}
			wg.Wait()
			close(claims)
			var admitted []*Job
			for j := range claims {
				if j != nil {
					admitted = append(admitted, j)
				}
			}
			if len(admitted) != limit {
				t.Fatalf("admitted %d want %d", len(admitted), limit)
			}
			// Waiting for dependencies still owns a full-assessment slot.
			if err = f.repo.Retry(ctx, *admitted[0], 0, "dependency"); err != nil {
				t.Fatal(err)
			}
			var extra string
			if err = f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE NOT(payload ? 'startedAt') ORDER BY id LIMIT 1`).Scan(&extra); err != nil {
				t.Fatal(err)
			}
			if j, e := replica.Claim(ctx, KindNormalRecovery, extra, false); e != nil || j != nil {
				t.Fatalf("exceeded waiting admission: %v %v", j, e)
			}
			resumed, e := replica.Claim(ctx, KindNormalRecovery, admitted[0].Ref(), false)
			if e != nil || resumed == nil {
				t.Fatalf("cannot resume full lane: %v", e)
			}
			if e = replica.Finish(ctx, *resumed, nil); e != nil {
				t.Fatal(e)
			}
			if j, e := replica.Claim(ctx, KindNormalRecovery, extra, false); e != nil || j == nil {
				t.Fatalf("slot not released: %v", e)
			}
			if err = f.repo.Schedule(ctx, KindRecovery, "fast", p, 1, time.Now()); err != nil {
				t.Fatal(err)
			}
			if f.claim(t, KindRecovery).Payload.StartedAt.IsZero() {
				t.Fatal("normal blocked fast")
			}
		})
	}
}

func TestNormalRecoveryGatesExitOnFreshSuccessfulConnectivity(t *testing.T) {
	for _, scenario := range []string{"pending", "unreachable", "stale", "error", "success"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			j := queueNormal(t, f)
			session := "recovery:" + j.Ref()
			if err := f.runner.recovery(ctx, j); !isWaiting(err) {
				t.Fatalf("initial: %v", err)
			}
			var exits int
			if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='exit'`).Scan(&exits); err != nil || exits != 0 {
				t.Fatalf("eager exit: %d %v", exits, err)
			}
			if scenario == "pending" {
				return
			}
			f.evidence(t, session)
			f.exec(t, `DELETE FROM node_explorer_probe_results WHERE kind='exit'`)
			switch scenario {
			case "unreachable":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{reachable}','false')`)
			case "stale":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{checkedAt}','"2000-01-01T00:00:00Z"')`)
			case "error":
				f.exec(t, `UPDATE node_explorer_probe_results SET payload=payload || '{"error":"inconclusive"}'`)
			}
			err := f.runner.recovery(ctx, j)
			if scenario != "success" {
				if err != nil {
					t.Fatal(err)
				}
				var result string
				if err = f.repo.Pool.QueryRow(ctx, `SELECT payload->>'recoveryResult' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&result); err != nil || result != "probe-failed" {
					t.Fatalf("accepted failed connectivity: %s %v", result, err)
				}
				if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='exit'`).Scan(&exits); err != nil || exits != 0 {
					t.Fatalf("failed node requested exit: %d %v", exits, err)
				}
				return
			}
			if !isWaiting(err) {
				t.Fatalf("first exit not awaited: %v", err)
			}
			f.evidence(t, session)
			if err = f.runner.recovery(ctx, j); !isWaiting(err) {
				t.Fatalf("final exit not awaited: %v", err)
			}
			f.evidence(t, session+":verify")
			if err = f.runner.recovery(ctx, j); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShorterRecoveryIntervalAdvancesOnlyUnstartedJobs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.retirement(t)
	f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
	f.runner.Config.Role = "recovery"
	f.runner.Config.NormalRecoveryInterval = 6 * time.Hour
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	f.runner.Config.NormalRecoveryInterval = 20 * time.Minute
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var generation int64
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&due, &generation); err != nil || due.After(time.Now().Add(20*time.Minute)) || generation != 2 {
		t.Fatalf("old cadence retained: %v %d %v", due, generation, err)
	}
	f.exec(t, `UPDATE node_explorer_work SET due_at=now()+interval '6 hours',payload=payload||jsonb_build_object('startedAt',now()) WHERE kind='normal-recovery'`)
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,generation FROM node_explorer_work WHERE kind='normal-recovery'`).Scan(&due, &generation); err != nil || due.Before(time.Now().Add(5*time.Hour)) || generation != 2 {
		t.Fatalf("running session replaced: %v %d %v", due, generation, err)
	}
}

func TestRetryIntervalForTiersFailures(t *testing.T) {
	base := 20 * time.Minute
	cases := []struct {
		name     string
		result   string
		attempts int
		want     time.Duration
	}{
		{"first retry", "probe-failed", 1, base},
		{"second retry", "probe-failed", 2, 2 * time.Hour},
		{"third retry", "probe-failed", 3, 12 * time.Hour},
		{"fourth retry", "probe-failed", 4, 24 * time.Hour},
		{"fifth retry", "probe-failed", 5, 24 * time.Hour},
		{"stays stuck at ceiling", "probe-failed", 20, 24 * time.Hour},
		{"config rejection unchanged", "config-rejected", 1, max(base, 6*time.Hour)},
		{"config rejection high attempt", "config-rejected", 30, max(base, 6*time.Hour)},
		{"dead verdict is terminal", "config-rejected-dead", 1, time.Duration(1 << 62)},
		{"other outcomes ride the ladder", "timed-out", 2, 2 * time.Hour},
		{"unknown outcome rides the ladder", "", 2, 2 * time.Hour},
		{"attempts below one never shorten", "probe-failed", 0, base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryIntervalFor(tc.result, tc.attempts, base); got != tc.want {
				t.Fatalf("retryIntervalFor(%q,%d) = %v want %v", tc.result, tc.attempts, got, tc.want)
			}
		})
	}
}

func TestNormalRecoveryBacksOffRepeatedFailures(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j := queueNormal(t, f)
	// Two recorded failures must push the next attempt to a two-hour deadline.
	// Attempts are zeroed by re-queueing, so simulate the persisted ladder by
	// replaying recordRecoveryResult twice across two generations.
	if err := f.repo.Finish(ctx, j, func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, j, "probe-failed", "") }); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := f.repo.Pool.QueryRow(ctx, `SELECT generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	ref := fmt.Sprintf("%d:%d", j.ID, generation)
	f.exec(t, `UPDATE node_explorer_work SET due_at=now() WHERE id=$1`, j.ID)
	next, err := f.repo.Claim(ctx, KindNormalRecovery, ref, false)
	if err != nil || next == nil {
		t.Fatalf("claim retry generation: %v %v", next, err)
	}
	if err := f.repo.Finish(ctx, *next, func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, *next, "probe-failed", "") }); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	var attempts int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at,(payload->>'recoveryAttempts')::int FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&due, &attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts not accumulated: %d", attempts)
	}
	// The wait is two hours plus a keyed offset of at most a quarter of it, so a
	// wave of second failures spreads instead of landing on one second.
	if due.Before(time.Now().Add(1*time.Hour+59*time.Minute)) || due.After(time.Now().Add(2*time.Hour+31*time.Minute)) {
		t.Fatalf("second failure did not back off to two hours: %v", due)
	}
	// The prefetch advance must leave tiered deadlines alone.
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !due.Equal(after) {
		t.Fatalf("prefetch moved a tiered deadline: %v -> %v", due, after)
	}
}

func TestPruneCompletedNormalRecoveryWork(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	marker := func(key, payload string, due string) int64 {
		t.Helper()
		var id int64
		if err := f.repo.Pool.QueryRow(ctx, `INSERT INTO node_explorer_work(kind,key,payload,state,due_at)
		VALUES('normal-recovery',$1,$2,'done',$3::timestamptz) RETURNING id`, key, payload, due).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// A fresh completed marker must survive: the staged cadence still reads it.
	fresh := marker("prune:fresh", `{"identity":"i-fresh","checkedAt":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","recoveryResult":"probe-failed","recoveryAttempts":1}`, "now()")
	// An old completed marker is residue: its retirement either still qualifies
	// (and schedule starts a fresh assessment) or nothing references it.
	old := marker("prune:old", `{"identity":"i-old","checkedAt":"`+time.Now().UTC().Add(-45*24*time.Hour).Format(time.RFC3339Nano)+`","recoveryResult":"probe-failed","recoveryAttempts":3}`, "now()")
	// A legacy row with no checkedAt falls back to its due_at timestamp.
	var legacy int64
	if err := f.repo.Pool.QueryRow(ctx, `INSERT INTO node_explorer_work(kind,key,payload,state,due_at)
	VALUES('normal-recovery','prune:legacy','{"identity":"i-legacy","recoveryResult":"probe-failed"}','done',now()-interval '45 days') RETURNING id`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	// An unfinished queued row must never be deleted regardless of age.
	var pending int64
	if err := f.repo.Pool.QueryRow(ctx, `INSERT INTO node_explorer_work(kind,key,payload,state,due_at)
	VALUES('normal-recovery','prune:pending','{"identity":"i-pending"}','queued',now()-interval '45 days') RETURNING id`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	// A different kind keeps the legacy two-day bucket, untouched by this path.
	var other int64
	if err := f.repo.Pool.QueryRow(ctx, `INSERT INTO node_explorer_work(kind,key,payload,state,due_at)
	VALUES('recovery','prune:other','{"identity":"i-other"}','done',now()-interval '45 days') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}

	if err := f.repo.pruneCompletedWork(ctx); err != nil {
		t.Fatal(err)
	}
	var present int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE id=ANY($1::bigint[])`, []int64{fresh, pending, other}).Scan(&present); err != nil || present != 3 {
		t.Fatalf("prune kept wrong rows: %d %v", present, err)
	}
	var gone int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE id=ANY($1::bigint[])`, []int64{old, legacy}).Scan(&gone); err != nil || gone != 0 {
		t.Fatalf("stale markers survived: %d %v", gone, err)
	}
}

func TestConfigRejectedDeadLetterAfterThreeRounds(t *testing.T) {
	cases := []struct {
		attempts int
		want     string
	}{
		{0, "config-rejected"},
		{1, "config-rejected"},
		{2, "config-rejected"},
		{3, "config-rejected-dead"},
		{9, "config-rejected-dead"},
	}
	for _, tc := range cases {
		if got := configRejectedResult(tc.attempts); got != tc.want {
			t.Fatalf("configRejectedResult(%d) = %q want %q", tc.attempts, got, tc.want)
		}
	}
}

func TestNormalRecoveryDeadLetterStopsRescheduling(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	j := queueNormal(t, f)
	// Simulate four completed config-rejected assessment rounds the way the
	// manager path records them: attempts counts completed rejected rounds, so
	// the verdict flips to the terminal dead state on the fourth assessment.
	for round := 0; round < 4; round++ {
		got := configRejectedResult(j.Payload.RecoveryAttempts)
		if err := f.repo.Finish(ctx, j, func(tx pgx.Tx) error {
			return recordRecoveryResult(ctx, tx, j, got, "")
		}); err != nil {
			t.Fatal(err)
		}
		var result string
		if err := f.repo.Pool.QueryRow(ctx, `SELECT payload->>'recoveryResult' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		want := "config-rejected"
		if round == 3 {
			want = "config-rejected-dead"
		}
		var attempts int
		if err := f.repo.Pool.QueryRow(ctx, `SELECT coalesce((payload->>'recoveryAttempts')::int,0) FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != round+1 {
			t.Fatalf("round %d attempts=%d want %d", round, attempts, round+1)
		}
		if result != want {
			t.Fatalf("round %d recorded %q want %q (attempts %d)", round, result, want, attempts)
		}
		// The manager-backed retry ladder persists the verdict via requeue;
		// emulate it without restoring the session payload.
		f.exec(t, `UPDATE node_explorer_work SET state='queued',generation=generation+1,due_at=now(),
   payload=payload-'startedAt',lease_token=NULL,lease_until=NULL,attempts=0,error='' WHERE id=$1`, j.ID)
		if round < 3 {
			var ref string
			if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE state='queued' ORDER BY id LIMIT 1`).Scan(&ref); err != nil {
				t.Fatal(err)
			}
			next, err := f.repo.Claim(ctx, KindNormalRecovery, ref, false)
			if err != nil || next == nil {
				t.Fatal("requeue lost the job", err)
			}
			j = *next
		}
	}
	// The scheduler must stop reviving a dead-lettered assessment and must not
	// error out; the stale queued marker from the emulated retry stays dormant.
	f.runner.Config.Role = "recovery"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var running int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work w
 JOIN node_explorer_retired_nodes tr ON tr.identity=w.payload->>'identity' AND tr.retired_at=w.payload->>'retiredAt'
 WHERE w.kind='normal-recovery' AND w.state='queued' AND coalesce(w.payload->>'recoveryResult','')<>'config-rejected-dead'`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Fatalf("dead letter revived by scheduler: %d active rows", running)
	}
}

func TestNormalRecoverySkipsWhenFastProbeRecentlyFailed(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		age      time.Duration
		gate     bool // gate should fire and finish without manager traffic
		skipTail bool // second state path (browser-fingerprint sponsor)
	}{
		{"genuine failure", `{"success":false,"delay":0,"checkedAt":"2025-01-01T00:00:00Z"}`, 10 * time.Minute, true, false},
		{"abnormal environment", `{"checkedAt":"2025-01-01T00:00:00Z","abnormal":true}`, 10 * time.Minute, false, false},
		{"stale failure outside window", `{"success":false,"delay":0,"checkedAt":"2025-01-01T00:00:00Z"}`, 2 * time.Hour, false, false},
		{"recent success", `{"success":true,"delay":120,"checkedAt":"2025-01-01T00:00:00Z"}`, 10 * time.Minute, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			j := queueNormal(t, f)
			identity := f.id
			f.exec(t, `INSERT INTO node_explorer_fast_results(identity, checked_at, payload) VALUES($1, now()-$2::interval, $3::jsonb)
			 ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at, payload=EXCLUDED.payload`,
				identity, tc.age, tc.payload)
			ctx := context.Background()
			err := f.runner.recovery(ctx, j)
			gated := tc.gate
			if gated {
				if err != nil {
					t.Fatalf("should finish directly on fresh fast failure: %v", err)
				}
				var result string
				if err = f.repo.Pool.QueryRow(ctx, `SELECT payload->>'recoveryResult' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&result); err != nil {
					t.Fatal(err)
				}
				if result != "probe-failed" {
					t.Fatalf("expected probe-failed skip, got %s", result)
				}
				// No manager work must be spawned: no fast session row for this round.
				var n int
				if err = f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='fast-delay' AND payload->>'session' LIKE 'recovery:%'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("skip must not start manager sessions: %d", n)
				}
				return
			}
			if !isWaiting(err) {
				t.Fatalf("non-failing fast results must not short-circuit: %v", err)
			}
			session := "recovery:" + j.Ref()
			f.evidence(t, session)
			f.evidence(t, session+":verify")
			if err = f.runner.recovery(ctx, j); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalRecoverySkipGateGuards(t *testing.T) {
	// In the normal lane the only verdict ahead of the fresh-failure gate is
	// the expired-session fence: it must still win with timed-out.
	t.Run("expired beats skip", func(t *testing.T) {
		f := setup(t)
		j := queueNormal(t, f)
		f.exec(t, `INSERT INTO node_explorer_fast_results(identity, checked_at, payload) VALUES($1, now(), $2::jsonb)
		 ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at, payload=EXCLUDED.payload`,
			f.id, `{"checkedAt":"2025-01-01T00:00:00Z","abnormal":true}`)
		j.Payload.StartedAt = time.Now().Add(-2 * time.Hour) // in-memory payload drives the expired fence
		if err := f.runner.recovery(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		var result string
		if err := f.repo.Pool.QueryRow(context.Background(), `SELECT payload->>'recoveryResult' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		if result != "timed-out" {
			t.Fatalf("expired fence must beat the skip gate, got %s", result)
		}
	})
	t.Run("gate respects P1-5 backoff cadence", func(t *testing.T) {
		f := setup(t)
		j := queueNormal(t, f)
		f.exec(t, `INSERT INTO node_explorer_fast_results(identity, checked_at, payload) VALUES($1, now(), $2::jsonb)
		 ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at, payload=EXCLUDED.payload`,
			f.id, `{"success":false,"delay":0}`)
		if err := f.runner.recovery(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		if err := f.runner.schedule(context.Background()); err != nil {
			t.Fatal(err)
		}
		var due time.Time
		var generation int64
		if err := f.repo.Pool.QueryRow(context.Background(), `SELECT due_at,generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&due, &generation); err != nil {
			t.Fatal(err)
		}
		if due.Before(time.Now().Add(19*time.Minute)) || generation != j.Generation+1 {
			t.Fatalf("skip finish must queue through the probe-failed backoff: %v %d", due, generation)
		}
	})
}
