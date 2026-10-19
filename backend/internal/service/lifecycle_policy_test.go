package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestStrongExitReportsStageWithoutExposingTransportSecrets(t *testing.T) {
	broken := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxy http://user:password@private.invalid failed")
	})}
	_, err := StrongExit(context.Background(), broken, echoClient())
	if err == nil || !strings.Contains(err.Error(), "proxy api.ipify.org") || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "private.invalid") {
		t.Fatalf("unsafe diagnostics: %v", err)
	}
	_, err = StrongExit(context.Background(), echoClient("8.8.4.4", "8.8.4.4"), broken)
	if err == nil || !strings.Contains(err.Error(), "host api.ipify.org") {
		t.Fatalf("lost host failure stage: %v", err)
	}
}

func TestRetirementNeedsSpacedRoundsAndResetsOnSuccess(t *testing.T) {
	start := time.Now().UTC()
	failure := func(at time.Time) domain.ConnectivityState {
		return domain.ConnectivityState{CheckedAt: at.Format(time.RFC3339Nano), ConfirmedDead: true, Error: "两次连通性检测失败"}
	}
	v := accumulateFailures(domain.ConnectivityState{}, failure(start))
	if v.ConfirmedDead || v.FailedRounds != 1 {
		t.Fatalf("first round retired: %+v", v)
	}
	v = accumulateFailures(v, failure(start.Add(time.Minute)))
	if v.ConfirmedDead || v.FailedRounds != 1 {
		t.Fatalf("duplicate counted: %+v", v)
	}
	v = accumulateFailures(v, failure(start.Add(2*time.Minute)))
	if v.ConfirmedDead || v.FailedRounds != 2 {
		t.Fatalf("second round retired: %+v", v)
	}
	third := accumulateFailures(v, failure(start.Add(4*time.Minute)))
	if !retirementConfirmed(third) {
		t.Fatalf("sustained failure not retired: %+v", third)
	}
	for _, reset := range []domain.ConnectivityState{
		{CheckedAt: start.Add(3 * time.Minute).Format(time.RFC3339Nano), Reachable: true},
		{CheckedAt: start.Add(3 * time.Minute).Format(time.RFC3339Nano), Error: "对照不足"},
	} {
		reset = accumulateFailures(v, reset)
		if reset.FailedRounds != 0 || reset.ConfirmedDead {
			t.Fatal("streak survived recovery or inconclusive control")
		}
		next := accumulateFailures(reset, failure(start.Add(4*time.Minute)))
		if next.FailedRounds != 1 || next.ConfirmedDead {
			t.Fatal("old streak reused")
		}
	}
	if got := accumulateFailures(v, failure(start.Add(3*time.Hour))); got.FailedRounds != 1 || got.ConfirmedDead {
		t.Fatal("stale streak reused")
	}
}

func TestExpectedExitPreservesFailedEchoAndOnlyFlagsObservedChange(t *testing.T) {
	for _, tc := range []struct {
		v    domain.ExitState
		want string
	}{
		{domain.ExitState{Error: "真实出口核验失败"}, "真实出口核验失败"},
		{domain.ExitState{IP: "8.8.4.4", Error: "真实出口核验失败"}, "真实出口核验失败"},
		{domain.ExitState{IP: "8.8.8.8", Verified: true}, "复核期间出口变化"},
		{domain.ExitState{IP: "8.8.4.4", Verified: true}, ""},
	} {
		checkExpectedExit(&tc.v, "8.8.4.4")
		if tc.v.Error != tc.want {
			t.Fatalf("got %q want %q", tc.v.Error, tc.want)
		}
	}
}

func TestLifecycleRejectsLegacyOrSingleRoundRetirement(t *testing.T) {
	for _, rounds := range []int{0, 1, 3} {
		f := setup(t)
		now := time.Now().UTC()
		v := domain.ConnectivityState{CheckedAt: now.Format(time.RFC3339Nano), ConfirmedDead: true, FailedRounds: rounds,
			FirstFailureAt: now.Add(-4 * time.Minute).Format(time.RFC3339Nano), LastFailureAt: now.Format(time.RFC3339Nano)}
		b, _ := json.Marshal(v)
		f.exec(t, `INSERT INTO node_explorer_connectivity VALUES($1,now(),$2)`, f.id, b)
		p := Payload{Identity: f.id, Action: "retire", CheckedAt: v.CheckedAt}
		if err := f.repo.Schedule(context.Background(), KindLifecycle, "retire", p, 1, now); err != nil {
			t.Fatal(err)
		}
		if err := f.runner.lifecycle(context.Background(), f.claim(t, KindLifecycle)); err != nil {
			t.Fatal(err)
		}
		target, err := f.repo.Target(context.Background(), f.id)
		if err != nil || (target.Retired != nil) != (rounds == 3) {
			t.Fatalf("rounds=%d target=%+v err=%v", rounds, target, err)
		}
	}
}

func TestTransientRecoveryBackoffUsesFreshGenerationAndPreservesHistory(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.retirement(t)
	f.runner.Config.Role = "residential"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	j := f.claim(t, KindRecovery)
	f.evidence(t, "recovery:"+j.Ref())
	f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{reachable}','false') WHERE kind='connectivity'`)
	if err := f.runner.recovery(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var generation int64
	f.repo.Pool.QueryRow(ctx, `SELECT state,generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state, &generation)
	if state != "done" || generation != 1 {
		t.Fatal("retry ignored backoff")
	}
	f.exec(t, `UPDATE node_explorer_work SET payload=jsonb_set(payload,'{checkedAt}',to_jsonb($2::text)) WHERE id=$1`, j.ID, time.Now().Add(-6*time.Minute).UTC().Format(time.RFC3339Nano))
	// Repeated schedulers/restarts must enqueue this retry only once.
	for range 2 {
		if err := f.runner.schedule(ctx); err != nil {
			t.Fatal(err)
		}
	}
	next := f.claim(t, KindRecovery)
	if next.ID != j.ID || next.Generation != 2 || next.Payload.RecoveryAttempts != 1 {
		t.Fatalf("bad retry %+v", next)
	}
	if err := f.runner.recovery(ctx, next); err == nil {
		t.Fatal("reused old session proofs")
	}
	f.evidence(t, "recovery:"+next.Ref())
	f.evidence(t, "recovery:"+next.Ref()+":verify")
	if err := f.runner.recovery(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.lifecycle(ctx, f.claim(t, KindLifecycle)); err != nil {
		t.Fatal(err)
	}
	target, err := f.repo.Target(ctx, f.id)
	if err != nil || target.Retired != nil {
		t.Fatal("fresh complete proof did not restore")
	}
	var raw []byte
	f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&raw)
	var saved Payload
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.RecoveryAttempts != 2 || len(saved.RecoveryHistory) != 2 || saved.RecoveryHistory[0].Result != "probe-failed" || saved.RecoveryHistory[1].Result != "eligible" {
		t.Fatalf("lost history %+v", saved)
	}
}

func TestRecoveryRetryEligibilityAndLegacyBackoff(t *testing.T) {
	for _, scenario := range []string{"legacy", "manual", "new-epoch", "unavailable", "source-deleted", "not-historical", "not-residential", "hosting", "eligible", "backoff"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			f.retirement(t)
			f.runner.Config.Role = "residential"
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			j := f.claim(t, KindRecovery)
			if err := f.repo.Finish(ctx, j, func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, j, "probe-failed", "") }); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE node_explorer_work SET payload=(payload-'recoveryAttempts'-'recoveryHistory') || jsonb_build_object('checkedAt',$1::text)`, time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339Nano))
			switch scenario {
			case "manual":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET reason='manual disable'`)
			case "new-epoch":
				f.exec(t, `UPDATE node_explorer_retired_nodes SET retired_at='new'`)
			case "unavailable":
				f.exec(t, `UPDATE node_explorer_node_registry SET available=false`)
			case "source-deleted":
				f.exec(t, `DELETE FROM node_explorer_snapshots`)
			case "not-historical":
				f.exec(t, `UPDATE node_explorer_node_registry SET historical_residential=false`)
			case "not-residential", "eligible":
				f.exec(t, `UPDATE node_explorer_work SET payload=jsonb_set(payload,'{recoveryResult}',to_jsonb($1::text))`, scenario)
			case "hosting":
				f.exec(t, `UPDATE node_explorer_work SET payload=jsonb_set(payload,'{recoveryResult}','"skipped-datacenter-asn"')`)
			case "backoff":
				f.exec(t, `UPDATE node_explorer_work SET payload=payload || '{"recoveryAttempts":2}'`)
			}
			if err := f.repo.retryResidentialRecovery(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := f.repo.Pool.QueryRow(ctx, `SELECT state FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if (state == "queued") != (scenario == "legacy") {
				t.Fatalf("unexpected state %s", state)
			}
		})
	}
}

func TestSuspectedFailureGetsEarlyRecheckWithoutRetirement(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.evidence(t, "seed")
	f.exec(t, `UPDATE node_explorer_connectivity SET payload=payload || '{"reachable":false,"failedRounds":1,"confirmedDead":false}'::jsonb`)
	f.runner.Config.Role = "connectivity"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due, checked time.Time
	if err := f.repo.Pool.QueryRow(ctx, `SELECT w.due_at,c.checked_at FROM node_explorer_work w JOIN node_explorer_connectivity c ON c.identity=w.key WHERE w.kind='connectivity'`).Scan(&due, &checked); err != nil {
		t.Fatal(err)
	}
	if (due.Sub(checked) - 2*time.Minute).Abs() > time.Millisecond {
		t.Fatalf("suspect recheck delayed: %s", due.Sub(checked))
	}
	target, err := f.repo.Target(ctx, f.id)
	if err != nil || target.Retired != nil {
		t.Fatal("suspected failure retired")
	}
}

func TestConnectivityTargetChangeResetsFailureStreak(t *testing.T) {
	now := time.Now().UTC()
	previous := domain.ConnectivityState{ConfirmedDead: true, FailedRounds: 3, FirstFailureAt: now.Add(-10 * time.Minute).Format(time.RFC3339Nano), LastFailureAt: now.Add(-5 * time.Minute).Format(time.RFC3339Nano)}
	current := domain.ConnectivityState{ConfirmedDead: true, CheckedAt: now.Format(time.RFC3339Nano), ProbeTarget: "https://www.gstatic.com/generate_204"}
	result := accumulateFailures(previous, current)
	if result.ConfirmedDead || result.FailedRounds != 1 {
		t.Fatalf("inherited old endpoint failures: %+v", result)
	}
}
