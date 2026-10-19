package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

// Each assessment owns fresh proofs. The normal service shares transport checks
// but does not require residential classification.
func (r *Runner) recovery(ctx context.Context, j Job) error {
	t, err := r.Repo.Target(ctx, j.Payload.Identity)
	if err != nil {
		return err
	}
	if !matchesRetirement(t, j.Payload) {
		return r.Repo.Finish(ctx, j, nil)
	}
	finish := func(result, ip string) error {
		return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
			return recordRecoveryResult(ctx, tx, j, result, ip)
		})
	}
	// Reuse the last observed real exit before creating any network probes.
	previous, err := r.Repo.Exit(ctx, t.Identity)
	if err != nil {
		return err
	}
	normal := j.Kind == KindNormalRecovery
	if !normal && previous != nil && previous.IP != "" {
		attr, err := r.Repo.Attributes(ctx, previous.IP)
		if err != nil {
			return err
		}
		if asn := hostingRecoveryASN(attr, previous.IP); asn != "" {
			return finish("skipped-datacenter-asn", previous.IP)
		}
	}
	expired, err := r.assessmentExpired(ctx, "recovery:"+j.Ref(), j.Payload.StartedAt)
	if err != nil {
		return err
	}
	if expired {
		return finish("timed-out", "")
	}

	session := "recovery:" + j.Ref()
	p := j.Payload
	p.RecoveryMode = ""
	if normal {
		p.RecoveryMode = "normal"
	}
	p.Session = session
	// Nonresidential failures are common: do not spend exit-pool capacity until
	// this assessment's fresh connectivity proof has passed. Residential recovery
	// keeps its existing parallel probes for low latency.
	kinds := []string{KindLive, KindExit}
	if normal {
		kinds = []string{KindLive}
	}
	if r.Config.Manager != nil {
		kinds = []string{KindFast}
	}
	err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		for _, kind := range kinds {
			if e := once(ctx, tx, kind, session, p, j.Priority); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.Config.Manager != nil {
		// A permanently rejected dependency cannot produce a proof. End this
		// assessment with a diagnostic outcome, without changing node lifecycle.
		var rejected bool
		e := r.Repo.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_work
 WHERE kind IN ('fast-delay','connectivity','exit') AND key=ANY($1::text[])
 AND error LIKE 'node_config_rejected:%')`, []string{session, session + ":verify"}).Scan(&rejected)
		if e != nil {
			return e
		}
		if rejected {
			return finish("config-rejected", "")
		}
		var quick transport.DelayResult
		ready, e := r.Repo.Probe(ctx, session, KindFast, &quick)
		if e != nil {
			return e
		}
		if ready && !quick.Success {
			return finish("probe-failed", "")
		}
	}
	var live domain.ConnectivityState
	var exit domain.ExitState
	a, err := r.Repo.Probe(ctx, session, KindLive, &live)
	if err != nil {
		return err
	}
	if normal || r.Config.Manager != nil {
		if !a {
			return dependencyWait(j.Attempts)
		}
		if !live.Reachable || live.Error != "" || !fresh(live.CheckedAt, 3*time.Minute) {
			return finish("probe-failed", "")
		}
		if err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
			return once(ctx, tx, KindExit, session, p, j.Priority)
		}); err != nil {
			return err
		}
	}
	b, err := r.Repo.Probe(ctx, session, KindExit, &exit)
	if err != nil {
		return err
	}
	if !a || !b {
		return dependencyWait(j.Attempts)
	}
	if !live.Reachable || live.Error != "" || !fresh(live.CheckedAt, 3*time.Minute) || !validExit(exit) {
		return finish("probe-failed", exit.IP)
	}
	if r.Config.Manager != nil {
		if r.Config.Manager.Generation(ctx, live.KernelGeneration) != nil || r.Config.Manager.Generation(ctx, exit.KernelGeneration) != nil {
			return finish("stale-kernel-proof", "")
		}
	}
	attr, err := r.Repo.Attributes(ctx, exit.IP)
	if err != nil {
		return err
	}
	if asn := hostingRecoveryASN(attr, exit.IP); !normal && asn != "" {
		return finish("skipped-datacenter-asn", exit.IP)
	}
	if !normal && !domain.FreshResidential(attr, exit.IP, time.Now()) {
		if attr != nil && attr.Classification != nil && fresh(attr.Classification.CheckedAt, 24*time.Hour) {
			if attr.Classification.State == "unknown" {
				return finish("classification-unconfirmed", exit.IP)
			}
			return finish("not-residential", exit.IP)
		}
		err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
			if e := scheduleIP(ctx, tx, exit.IP, t.Identity, j.Priority); e != nil {
				return e
			}
			_, e := tx.Exec(ctx, `UPDATE node_explorer_work SET due_at=now(),priority=greatest(priority,$3) WHERE kind=$1 AND key=$2 AND state='queued'`, KindAttributes, exit.IP, j.Priority)
			return e
		})
		if err != nil {
			return err
		}
		return dependencyWait(j.Attempts)
	}
	p.Session = session + ":verify"
	p.ExpectedIP = exit.IP
	if err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error { return once(ctx, tx, KindExit, p.Session, p, j.Priority) }); err != nil {
		return err
	}
	var final domain.ExitState
	ready, err := r.Repo.Probe(ctx, p.Session, KindExit, &final)
	if err != nil {
		return err
	}
	if !ready {
		return dependencyWait(j.Attempts)
	}
	if !validExit(final) && final.Error != "复核期间出口变化" {
		return finish("exit-probe-failed", final.IP)
	}
	if !validExit(final) || final.IP != exit.IP || final.Version != exit.Version {
		return finish("exit-changed", final.IP)
	}
	p.Action = "restore"
	p.IP = final.IP
	p.ExitVersion = final.Version
	p.Session = session
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if normal {
			if e := scheduleIP(ctx, tx, final.IP, t.Identity, 0); e != nil {
				return e
			}
		}
		if e := once(ctx, tx, KindLifecycle, "restore:"+j.Ref(), p, 1); e != nil {
			return e
		}
		return recordRecoveryResult(ctx, tx, j, "eligible", final.IP)
	})
}

// Retain recent outcomes across generations, including the legacy once-only result.
func recordRecoveryResult(ctx context.Context, tx pgx.Tx, j Job, result, ip string) error {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	history := append([]RecoveryAssessment(nil), j.Payload.RecoveryHistory...)
	attempts := j.Payload.RecoveryAttempts
	if len(history) == 0 && j.Payload.RecoveryResult != "" && j.Payload.CheckedAt != "" {
		history = append(history, RecoveryAssessment{Result: j.Payload.RecoveryResult, IP: j.Payload.IP, CheckedAt: j.Payload.CheckedAt})
		attempts = max(attempts, 1)
	}
	history = append(history, RecoveryAssessment{Result: result, IP: ip, CheckedAt: stamp, Generation: j.Generation})
	if len(history) > 16 {
		history = history[len(history)-16:]
	}
	raw, err := json.Marshal(history)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE node_explorer_work SET payload=(payload-'startedAt') ||
	 jsonb_build_object('recoveryResult',$2::text,'checkedAt',$3::text,'ip',$4::text,
	 'recoveryAttempts',$5::integer,'recoveryHistory',$6::jsonb) WHERE id=$1`,
		j.ID, result, stamp, ip, attempts+1, raw)
	return err
}

func validExit(e domain.ExitState) bool {
	return e.Verified && e.Error == "" && e.IP != "" && e.AddressFamily == 4 && fresh(e.CheckedAt, 3*time.Minute)
}

func (r *Repository) FinishDependencies(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, e := r.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Only this worker changes automatic lifecycle state. The registry row lock
// serializes it with committed observations; the retirement epoch is a CAS.
func (r *Runner) lifecycle(ctx context.Context, j Job) error {
	ctx, release, paths, err := r.proofFence(ctx, j.Payload)
	if err != nil {
		return err
	}
	defer release()
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		ok, e := guard(ctx, tx, j.Payload)
		if e != nil || !ok {
			return e
		}
		var raw []byte
		var live domain.ConnectivityState
		e = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_connectivity WHERE identity=$1`, j.Payload.Identity).Scan(&raw)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &live); e != nil {
			return e
		}
		if r.Config.Manager != nil {
			if e = r.Config.Manager.Generation(ctx, live.KernelGeneration); e != nil {
				return e
			}
		}
		if e = validatePaths(ctx, tx, paths); e != nil {
			return e
		}
		reason := store.RetireRestoreReason
		switch j.Payload.Action {
		case "retire":
			if !retirementConfirmed(live) || live.CheckedAt != j.Payload.CheckedAt || !fresh(live.CheckedAt, 3*time.Minute) {
				return nil
			}
		case "restore":
			if !live.Reachable || live.Error != "" || !fresh(live.CheckedAt, 3*time.Minute) {
				return nil
			}
			var exit domain.ExitState
			var version int64
			e = tx.QueryRow(ctx, `SELECT payload,version FROM node_explorer_exits WHERE identity=$1`, j.Payload.Identity).Scan(&raw, &version)
			if e == pgx.ErrNoRows {
				return nil
			}
			if e != nil {
				return e
			}
			if e = json.Unmarshal(raw, &exit); e != nil {
				return e
			}
			if !validExit(exit) || exit.IP != j.Payload.IP || version != j.Payload.ExitVersion {
				return nil
			}
			var proof domain.ExitState
			e = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_probe_results WHERE session=$1 AND kind=$2 AND identity=$3`, j.Payload.Session+":verify", KindExit, j.Payload.Identity).Scan(&raw)
			if e == pgx.ErrNoRows {
				return nil
			}
			if e != nil {
				return e
			}
			if e = json.Unmarshal(raw, &proof); e != nil {
				return e
			}
			if r.Config.Manager != nil {
				if e = r.Config.Manager.Generation(ctx, proof.KernelGeneration); e != nil {
					return e
				}
				if e = r.Config.Manager.Generation(ctx, exit.KernelGeneration); e != nil {
					return e
				}
			}
			if !validExit(proof) || proof.IP != exit.IP || proof.Version != version {
				return nil
			}
			e = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip=$1 FOR SHARE`, exit.IP).Scan(&raw)
			if e == pgx.ErrNoRows && j.Payload.RecoveryMode == "normal" {
				raw = []byte(`{}`)
				e = nil
			}
			if e == pgx.ErrNoRows {
				return nil
			}
			if e != nil {
				return e
			}
			var attr domain.IPPureResult
			if e = json.Unmarshal(raw, &attr); e != nil {
				return e
			}
			if j.Payload.RecoveryMode != "normal" && (hostingRecoveryASN(&attr, exit.IP) != "" || !domain.FreshResidential(&attr, exit.IP, time.Now())) {
				return nil
			}
			reason = j.Payload.Reason
		default:
			return nil
		}
		if !r.Config.DryRun {
			if j.Payload.Action == "restore" {
				_, e = tx.Exec(ctx, `DELETE FROM node_explorer_retired_nodes WHERE identity=$1 AND retired_at=$2 AND reason=$3`, j.Payload.Identity, j.Payload.RetiredAt, reason)
			} else {
				_, e = tx.Exec(ctx, `INSERT INTO node_explorer_retired_nodes(identity,retired_at,reason) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, j.Payload.Identity, time.Now().UTC().Format(time.RFC3339Nano), reason)
			}
			if e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, `INSERT INTO node_explorer_lifecycle_events(identity,action,dry_run,reason) VALUES($1,$2,$3,$4)`, j.Payload.Identity, j.Payload.Action, r.Config.DryRun, reason)
		return e
	})
}
