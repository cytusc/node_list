package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// SyncRegistry is owned by source-ingest. Reused identities keep their facts.
func (r *Repository) SyncRegistry(ctx context.Context, _ *store.Store) error {
	return r.syncRegistry(ctx)
}

func (r *Repository) Connectivity(ctx context.Context, id string) (*domain.ConnectivityState, error) {
	var b []byte
	err := r.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_connectivity WHERE identity=$1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := &domain.ConnectivityState{}
	err = json.Unmarshal(b, v)
	return v, err
}
func (r *Repository) Exit(ctx context.Context, id string) (*domain.ExitState, error) {
	var b []byte
	var version int64
	err := r.Pool.QueryRow(ctx, `SELECT payload,version FROM node_explorer_exits WHERE identity=$1`, id).Scan(&b, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := &domain.ExitState{}
	err = json.Unmarshal(b, v)
	v.Version = version
	return v, err
}
func (r *Repository) Attributes(ctx context.Context, ip string) (*domain.IPPureResult, error) {
	var b []byte
	err := r.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip=$1`, ip).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := &domain.IPPureResult{}
	err = json.Unmarshal(b, v)
	return v, err
}

func writeProbe(ctx context.Context, tx pgx.Tx, session, kind, id string, v any) error {
	if session == "" {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_probe_results(session,kind,identity,checked_at,payload)
	VALUES($1,$2,$3,now(),$4) ON CONFLICT(session,kind) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, session, kind, id, b)
	return err
}

func (r *Repository) Probe(ctx context.Context, session, kind string, out any) (bool, error) {
	var b []byte
	err := r.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_probe_results WHERE session=$1 AND kind=$2`, session, kind).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, out)
}

// mergeAttributes serializes independent classification/risk writes for one IP.
func mergeAttributes(ctx context.Context, tx pgx.Tx, ip string, evidence []ipcheck.Evidence, verdict *domain.IPPureResult) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext($1))`, "attributes:"+ip); err != nil {
		return err
	}
	var b []byte
	err := tx.QueryRow(ctx, `SELECT payload FROM node_explorer_ip_attributes WHERE ip=$1 FOR UPDATE`, ip).Scan(&b)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	v := domain.IPPureResult{IP: ip, Status: "error", FailureStage: "ippure-query", Error: "风险查询待完成"}
	if len(b) > 0 {
		if err = json.Unmarshal(b, &v); err != nil {
			return err
		}
	}
	all := evidence
	if v.Classification != nil {
		all = append(append([]ipcheck.Evidence{}, v.Classification.Evidence...), all...)
	}
	if verdict != nil {
		if verdict.Status == "success" {
			v = *verdict
		} else if v.Status != "success" {
			v.Error = verdict.Error
		}
	}
	now := time.Now().UTC()
	votes := ipcheck.Score(ip, all, now)
	v.Classification = &votes
	v.Attributes = votes.Attributes(v.Attributes)
	// Classification refreshes must not refresh the timestamp of an old risk score.
	if verdict != nil && (verdict.Status == "success" || v.Status != "success") {
		v.CheckedAt = now.Format(time.RFC3339Nano)
	}
	if verdict != nil && verdict.Status == "success" {
		v.TrafficRatioCheckedAt = now.Format(time.RFC3339Nano)
	}
	v.IP = ip
	b, err = json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_ip_attributes(ip,checked_at,payload) VALUES($1,$2,$3)
	ON CONFLICT(ip) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload`, ip, now, b)
	if err != nil {
		return err
	}
	if votes.State == "residential" {
		_, err = tx.Exec(ctx, `UPDATE node_explorer_node_registry n SET historical_residential=true FROM node_explorer_exits e WHERE e.identity=n.identity AND e.ip=$1 AND NOT n.historical_residential`, ip)
	}
	return err
}

func (r *Repository) IPUsed(ctx context.Context, ip string) (bool, error) {
	var used bool
	err := r.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM node_explorer_exits e JOIN node_explorer_node_registry n USING(identity) WHERE e.ip=$1 AND n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id))`, ip).Scan(&used)
	return used, err
}

func fresh(raw string, ttl time.Duration) bool {
	t, e := time.Parse(time.RFC3339Nano, raw)
	return e == nil && t.Before(time.Now().Add(time.Minute)) && time.Since(t) <= ttl
}
