package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// A generation is one complete assessment. Dependency retries reuse its proofs;
// an unsuccessful assessment starts a new generation at the normal interval.
func (r *Runner) recovery(ctx context.Context, j Job) error {
	t, err := r.Repo.Target(ctx, j.Payload.Identity)
	if err != nil {
		return err
	}
	if !matchesRetirement(t, j.Payload) {
		return r.Repo.Finish(ctx, j, nil)
	}
	delay := r.Config.NormalInterval
	if t.HistoricalResidential {
		delay = r.Config.ResidentialInterval
	}
	finish := func() error { return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error { return recur(ctx, tx, j, delay) }) }
	if time.Since(j.Payload.StartedAt) > 3*time.Minute {
		return finish()
	}
	session := "recovery:" + j.Ref()
	p := j.Payload
	p.Session = session
	err = r.Repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
		for _, kind := range []string{KindLive, KindExit} {
			if e := once(ctx, tx, kind, session, p, j.Priority); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var live domain.ConnectivityState
	var exit domain.ExitState
	a, err := r.Repo.Probe(ctx, session, KindLive, &live)
	if err != nil {
		return err
	}
	b, err := r.Repo.Probe(ctx, session, KindExit, &exit)
	if err != nil {
		return err
	}
	if !a || !b {
		return waiting{time.Second}
	}
	if !live.Reachable || live.Error != "" || !fresh(live.CheckedAt, 3*time.Minute) || !validExit(exit) {
		return finish()
	}
	attr, err := r.Repo.Attributes(ctx, exit.IP)
	if err != nil {
		return err
	}
	if !domain.FreshResidential(attr, exit.IP, time.Now()) {
		if attr != nil && attr.Classification != nil && fresh(attr.Classification.CheckedAt, 24*time.Hour) {
			return finish()
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
		return waiting{time.Second}
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
		return waiting{time.Second}
	}
	if !validExit(final) || final.IP != exit.IP || final.Version != exit.Version {
		return finish()
	}
	p.Action = "restore"
	p.IP = final.IP
	p.ExitVersion = final.Version
	p.Session = session
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if e := once(ctx, tx, KindLifecycle, "restore:"+j.Ref(), p, 1); e != nil {
			return e
		}
		return recur(ctx, tx, j, delay)
	})
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
		reason := store.RetireRestoreReason
		switch j.Payload.Action {
		case "retire":
			if !live.ConfirmedDead || live.Reachable || live.CheckedAt != j.Payload.CheckedAt || !fresh(live.CheckedAt, 3*time.Minute) {
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
			if !validExit(proof) || proof.IP != exit.IP || proof.Version != version {
				return nil
			}
			e = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip=$1 FOR SHARE`, exit.IP).Scan(&raw)
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
			if !domain.FreshResidential(&attr, exit.IP, time.Now()) {
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
