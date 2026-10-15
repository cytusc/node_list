package service

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/cytusc/node_list/backend/internal/qilan"
	"github.com/cytusc/node_list/backend/internal/source"
	"github.com/cytusc/node_list/backend/internal/store"
)

func (r *Repository) RunSource(ctx context.Context, st *store.Store) error {
	ingestor := store.NewIngestor(st)
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
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if time.Since(lastSweep) >= time.Hour {
				// Network sessions cannot remain valid for more than three minutes.
				_, _ = r.Pool.Exec(ctx, `DELETE FROM node_explorer_probe_results WHERE checked_at<now()-interval '2 days'`)
				_, _ = r.Pool.Exec(ctx, `DELETE FROM node_explorer_work WHERE state='done' AND due_at<now()-interval '2 days'`)
				_, _ = r.Pool.Exec(ctx, `DELETE FROM node_explorer_lifecycle_events WHERE created_at<now()-interval '30 days'`)
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

type Status struct {
	Service           string    `json:"service"`
	UpdatedAt         time.Time `json:"updatedAt"`
	Healthy           bool      `json:"healthy"`
	Error             string    `json:"error,omitempty"`
	Queued            int64     `json:"queued"`
	Running           int64     `json:"running"`
	OldestWaitSeconds float64   `json:"oldestWaitSeconds"`
	Restored          int64     `json:"restored"`
}

func (r *Repository) Statuses(ctx context.Context) ([]Status, error) {
	rows, err := r.Pool.Query(ctx, `WITH expected(service) AS (VALUES ('api'),('source-ingest'),('connectivity'),('ip-service'),('residential-recovery'),('node-lifecycle')),
 s AS (SELECT e.service,h.updated_at,h.payload FROM expected e LEFT JOIN node_explorer_service_status h USING(service))
 SELECT s.service,coalesce(s.updated_at,'0001-01-01'::timestamptz),coalesce(s.updated_at>now()-interval '90 seconds' AND coalesce(s.payload->>'error','')='',false),
 CASE WHEN s.updated_at IS NULL THEN '服务尚未启动' WHEN s.updated_at<=now()-interval '90 seconds' THEN '服务心跳已过期' ELSE coalesce(s.payload->>'error','') END,
 (SELECT count(*) FROM node_explorer_work w WHERE w.kind=ANY(CASE s.service WHEN 'ip-service' THEN ARRAY['exit','attributes','risk'] WHEN 'residential-recovery' THEN ARRAY['recovery'] WHEN 'node-lifecycle' THEN ARRAY['lifecycle'] WHEN 'connectivity' THEN ARRAY['connectivity'] ELSE ARRAY[]::text[] END) AND w.state='queued'),
 (SELECT count(*) FROM node_explorer_work w WHERE w.kind=ANY(CASE s.service WHEN 'ip-service' THEN ARRAY['exit','attributes','risk'] WHEN 'residential-recovery' THEN ARRAY['recovery'] WHEN 'node-lifecycle' THEN ARRAY['lifecycle'] WHEN 'connectivity' THEN ARRAY['connectivity'] ELSE ARRAY[]::text[] END) AND w.state='running'),
 coalesce((SELECT greatest(0,extract(epoch FROM now()-min(due_at))) FROM node_explorer_work w WHERE w.state='queued' AND w.kind=ANY(CASE s.service WHEN 'ip-service' THEN ARRAY['exit','attributes','risk'] WHEN 'residential-recovery' THEN ARRAY['recovery'] WHEN 'node-lifecycle' THEN ARRAY['lifecycle'] WHEN 'connectivity' THEN ARRAY['connectivity'] ELSE ARRAY[]::text[] END)),0),
 CASE WHEN s.service='node-lifecycle' THEN (SELECT count(*) FROM node_explorer_lifecycle_events WHERE action='restore' AND NOT dry_run) ELSE 0 END
 FROM s ORDER BY service`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Status{}
	for rows.Next() {
		var s Status
		if err = rows.Scan(&s.Service, &s.UpdatedAt, &s.Healthy, &s.Error, &s.Queued, &s.Running, &s.OldestWaitSeconds, &s.Restored); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
