package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryASNPolicyUsesExactExitAndKeepsAccessNetworks(t *testing.T) {
	for _, tc := range []struct {
		asn  string
		skip bool
	}{
		{"AS13335", true}, {"45102", true}, {"as12876 Scaleway SAS", true},
		{"AS16509", true}, {"AS9009", true}, {"AS8560", true},
		{"AS6939", false}, {"AS3462", false}, {"AS4760", false}, {"AS4837", false},
		{"", false}, {"AS133350", false}, {"Cloudflare", false},
	} {
		t.Run(tc.asn, func(t *testing.T) {
			a := &domain.IPPureResult{IP: "8.8.4.4", ASN: tc.asn}
			if got := hostingRecoveryASN(a, a.IP) != ""; got != tc.skip {
				t.Fatalf("skip=%v want %v", got, tc.skip)
			}
			if hostingRecoveryASN(a, "8.8.8.8") != "" {
				t.Fatal("used another exit's ASN")
			}
		})
	}
	a := &domain.IPPureResult{IP: "8.8.4.4", Classification: &ipcheck.Result{IP: "8.8.4.4", Evidence: []ipcheck.Evidence{{IP: "8.8.4.4", ASN: "AS13335 Cloudflare", QueryStatus: "success"}}}}
	if hostingRecoveryASN(a, a.IP) != "AS13335" {
		t.Fatal("lost ASN from classification provider")
	}
	a.Classification.Evidence = append(a.Classification.Evidence, ipcheck.Evidence{IP: a.IP, ASN: "AS3462", QueryStatus: "success"})
	if hostingRecoveryASN(a, a.IP) != "" {
		t.Fatal("conflicting ASN evidence was accepted")
	}
}

func TestRecoveryResultsSurviveCleanupRestartAndScheduler(t *testing.T) {
	for _, outcome := range []string{"not-residential", "probe-failed", "timed-out", "eligible", "exit-changed"} {
		t.Run(outcome, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			p := f.retirement(t)
			f.runner.Config.Role = "residential"
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			j := f.claim(t, KindRecovery)
			if err := f.repo.Finish(ctx, j, func(tx pgx.Tx) error { return recordRecoveryResult(ctx, tx, j, outcome, "8.8.4.4") }); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE node_explorer_work SET due_at=now()-interval '3 days'`)
			if err := f.repo.pruneCompletedWork(ctx); err != nil {
				t.Fatal(err)
			}
			f.runner.Repo = &Repository{Pool: f.repo.Pool}
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			var raw []byte
			var generation int64
			if err := f.repo.Pool.QueryRow(ctx, `SELECT state,payload,generation FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state, &raw, &generation); err != nil {
				t.Fatal(err)
			}
			var saved Payload
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if state != "done" || generation != j.Generation || saved.RecoveryResult != outcome || saved.IP != "8.8.4.4" || saved.CheckedAt == "" {
				t.Fatalf("lost/requeued result: %s %+v", state, saved)
			}
			// A distinct retirement is a new assessment; the old marker cannot block it.
			f.exec(t, `UPDATE node_explorer_retired_nodes SET retired_at=$1`, p.RetiredAt+"-new")
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind='recovery' AND state='queued'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("new retirement blocked: %d %v", count, err)
			}
		})
	}
}

func TestRecoveryFailedAssessmentEndsInsteadOfRecurring(t *testing.T) {
	for _, scenario := range []string{"probe-failed", "not-residential", "timed-out", "exit-changed", "eligible", "skipped-datacenter-asn"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			f.retirement(t)
			f.runner.Config.Role = "residential"
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			j := f.claim(t, KindRecovery)
			session := "recovery:" + j.Ref()
			if scenario == "timed-out" {
				j.Payload.StartedAt = time.Now().Add(-4 * time.Minute)
			} else {
				f.evidence(t, session)
				switch scenario {
				case "probe-failed":
					f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{reachable}','false') WHERE kind='connectivity'`)
				case "not-residential":
					f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=jsonb_set(payload #- '{classification,evidence}' #- '{classification,types}','{classification,state}','"datacenter"')`)
				case "skipped-datacenter-asn":
					f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=payload || '{"asn":"AS12876","asnOrganization":"Scaleway SAS"}'::jsonb`)
				case "eligible", "exit-changed":
					f.evidence(t, session+":verify")
					if scenario == "exit-changed" {
						f.exec(t, `UPDATE node_explorer_probe_results SET payload=jsonb_set(payload,'{ip}','"8.8.8.8"') WHERE session=$1`, session+":verify")
					}
				}
			}
			if err := f.runner.recovery(ctx, j); err != nil {
				t.Fatal(err)
			}
			if err := f.runner.schedule(ctx); err != nil {
				t.Fatal(err)
			}
			var state, result string
			if err := f.repo.Pool.QueryRow(ctx, `SELECT state,payload->>'recoveryResult' FROM node_explorer_work WHERE id=$1`, j.ID).Scan(&state, &result); err != nil {
				t.Fatal(err)
			}
			if state != "done" || result != scenario {
				t.Fatalf("state=%s result=%s", state, result)
			}
			if scenario == "skipped-datacenter-asn" {
				var count int
				if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_work WHERE kind<>'recovery'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("hosting created probes: %d %v", count, err)
				}
			}
		})
	}
}

func TestHostingRecoveryBacklogSkippedBeforeAdmission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.retirement(t)
	f.evidence(t, "seed")
	f.exec(t, `UPDATE node_explorer_ip_attributes SET payload=payload || '{"asn":"AS13335","asnOrganization":"Cloudflare"}'::jsonb`)
	f.runner.Config.Role = "residential"
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var state, result, ip string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT state,payload->>'recoveryResult',payload->>'ip' FROM node_explorer_work WHERE kind='recovery'`).Scan(&state, &result, &ip); err != nil {
		t.Fatal(err)
	}
	if state != "done" || result != "skipped-datacenter-asn" || ip != "8.8.4.4" {
		t.Fatalf("backlog not skipped: %s %s %s", state, result, ip)
	}
}
