package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cytusc/node_list/backend/internal/qilan"
	"github.com/cytusc/node_list/backend/internal/source"
	"github.com/cytusc/node_list/backend/internal/store"
)

func (r *Repository) RunSource(ctx context.Context, st *store.Store) error {
	ingestor := store.NewPersistentIngestor(st)
	dir := Env("SUBS_CHECK_DIR", filepath.Join(Root(), "data", "sub"))
	results := Env("RESULTS_FILE", filepath.Join(dir, "config", "cache", "config", "results.json"))
	clash, links := filepath.Join(dir, "output", "all.yaml"), filepath.Join(dir, "output", "base64.txt")
	ingest := func(ctx context.Context) error {
		_, err := ingestor.ImportFile(ctx, results, clash, links)
		warning := store.SourceWarningFor(err)
		st.SetSourceWarning(warning)
		_, writeErr := r.Pool.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('source-ingest',now(),jsonb_build_object('error',''::text,'sourceWarning',$1::text,'running',true)) ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, warning)
		if writeErr != nil {
			return writeErr
		}
		return r.SyncRegistry(ctx, st)
	}
	_ = ingest(ctx)
	watcher := source.NewWatcher([]string{results, clash, links}, source.WatcherConfig{Quiet: 5 * time.Second, Sweep: 2 * time.Minute, Ingest: ingest})
	_ = watcher.Start(ctx)
	sessionFile := Env("QILAN_SESSION_FILE", "/data/qilan/session.txt")
	if _, err := os.Stat(sessionFile); os.Getenv("QILAN_SESSION") != "" || err == nil {
		q := qilan.NewSource(st, ingestor, qilan.SourceConfig{Session: os.Getenv("QILAN_SESSION"), SessionFile: sessionFile, UserAgent: Env("QILAN_USER_AGENT", "Mozilla/5.0"), Interval: Seconds("QILAN_REFRESH_INTERVAL_SECONDS", 900), Webhook: os.Getenv("FEISHU_WEBHOOK_URL")})
		q.Start(ctx)
		defer q.Close()
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastIngest := time.Now()
	lastSweep := time.Time{}
	retentionDays := 7
	if value := os.Getenv("SNAPSHOT_RETENTION_DAYS"); value != "" {
		if days, err := strconv.Atoi(value); err == nil {
			retentionDays = days
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if time.Since(lastSweep) >= time.Hour {
				// Network sessions cannot remain valid for more than three minutes.
				_, _ = r.Pool.Exec(ctx, `DELETE FROM node_explorer_probe_results WHERE checked_at<now()-interval '2 days'`)
				_ = r.pruneCompletedWork(ctx)
				_, _ = r.Pool.Exec(ctx, `DELETE FROM node_explorer_lifecycle_events WHERE created_at<now()-interval '30 days'`)
				if err := r.pruneSources(ctx, retentionDays); err != nil {
					return err
				}
				lastSweep = time.Now()
			}
			if time.Since(lastIngest) >= 30*time.Second {
				watcher.Trigger(ctx)
				lastIngest = time.Now()
			}
			message := ""
			if err := r.SyncRegistry(ctx, st); err != nil {
				message = "节点注册同步失败"
			}
			_, err := r.Pool.Exec(ctx, `UPDATE node_explorer_service_status SET updated_at=now(),payload=jsonb_set(payload,'{error}',to_jsonb($1::text)) WHERE service='source-ingest'`, message)
			if err != nil {
				return err
			}
		}
	}
}

// Preserve the existing source retention policy without starting a projector.
func (r *Repository) pruneSources(ctx context.Context, days int) error {
	if days <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339Nano)
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_ippure_results WHERE snapshot_id IN (SELECT id FROM node_explorer_snapshots s WHERE imported_at<$1 AND NOT EXISTS(SELECT 1 FROM node_explorer_node_registry n WHERE n.snapshot_id=s.id AND n.available))`, cutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM node_explorer_snapshots s WHERE imported_at<$1 AND NOT EXISTS(SELECT 1 FROM node_explorer_node_registry n WHERE n.snapshot_id=s.id AND n.available)`, cutoff); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Recovery rows are durable verification markers, not expiring probe sessions.
func (r *Repository) pruneCompletedWork(ctx context.Context) error {
	_, err := r.Pool.Exec(ctx, `DELETE FROM node_explorer_work WHERE state='done' AND kind NOT IN ('recovery','normal-recovery') AND due_at<now()-interval '2 days'`)
	return err
}
