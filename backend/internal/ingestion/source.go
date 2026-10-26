package ingestion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/publicproxy"
	"github.com/cytusc/node_list/backend/internal/qilan"
	"github.com/cytusc/node_list/backend/internal/runtimeenv"
	"github.com/cytusc/node_list/backend/internal/servicelock"
	"github.com/cytusc/node_list/backend/internal/source"
)

type SourceProvider interface {
	Name() string
	Start(context.Context)
	Close()
}
type qilanProvider struct{ q *qilan.Source }

func (p *qilanProvider) Name() string              { return "qilan" }
func (p *qilanProvider) Start(ctx context.Context) { p.q.Start(ctx) }
func (p *qilanProvider) Close()                    { p.q.Close() }

func (r *Repository) RunSource(ctx context.Context) error {
	return servicelock.Run(ctx, r.Pool, "source-ingest", func(ctx context.Context) error { return r.runSource(ctx) })
}
func (r *Repository) runSource(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := r.Prepare(ctx); err != nil {
		return err
	}
	converter := DefaultConverter()
	if _, err := converter.Convert(ctx, map[string]any{"items": []any{}}); err != nil {
		return err
	}
	var mu sync.Mutex
	warnings := map[string]bool{}
	heartbeat := func(name string, err error) error {
		mu.Lock()
		defer mu.Unlock()
		if name != "" {
			warnings[name] = err != nil
		}
		count := 0
		for _, failed := range warnings {
			if failed {
				count++
			}
		}
		warning := ""
		if count > 0 {
			warning = fmt.Sprintf("%d source(s) require attention", count)
		}
		_, writeErr := r.Pool.Exec(ctx, `INSERT INTO node_explorer_service_status(service,updated_at,payload) VALUES('source-ingest',now(),jsonb_build_object('running',true,'error','','sourceWarning',$1::text)) ON CONFLICT(service) DO UPDATE SET updated_at=EXCLUDED.updated_at,payload=EXCLUDED.payload`, warning)
		return writeErr
	}
	if err := heartbeat("", nil); err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_, _ = r.Pool.Exec(cleanup, `UPDATE node_explorer_service_status SET payload=payload || '{"running":false}'::jsonb WHERE service='source-ingest'`)
	}()
	enabled := map[string]bool{}
	for _, name := range strings.Split(runtimeenv.Env("SOURCE_PROVIDERS", "subs-check,proxio-all,Jakee-http,xyzs996-all"), ",") {
		enabled[strings.TrimSpace(name)] = true
	}
	importer := func(name string) *reportingImporter {
		return &reportingImporter{Importer: &Importer{Repository: r, Converter: converter, Source: name}, report: func(err error) { _ = heartbeat(name, err) }}
	}
	var providers []SourceProvider
	if enabled["qilan"] {
		// Missing credentials remain a source error, rather than silently disabling an explicitly enabled provider.
		providers = append(providers, &qilanProvider{q: qilan.NewSource(importer("qilan"), qilan.SourceConfig{OnError: func(err error) { _ = heartbeat("qilan", err) }, Session: os.Getenv("QILAN_SESSION"), SessionFile: runtimeenv.Env("QILAN_SESSION_FILE", "/data/qilan/session.txt"), UserAgent: runtimeenv.Env("QILAN_USER_AGENT", "Mozilla/5.0"), Interval: runtimeenv.Seconds("QILAN_REFRESH_INTERVAL_SECONDS", 900)})})
	}
	for _, def := range publicproxy.Definitions() {
		if enabled[def.Name] {
			p := publicproxy.New(def, importer(def.Name), runtimeenv.Seconds("PUBLIC_PROXY_REFRESH_INTERVAL_SECONDS", 1800))
			name := def.Name
			p.OnResult = func(err error) { _ = heartbeat(name, err) }
			providers = append(providers, p)
		}
	}
	// No result-file matching, summary-based credential guessing, or upstream liveness filtering.
	if enabled["subs-check"] {
		dir := runtimeenv.Env("SUBS_CHECK_DIR", filepath.Join(runtimeenv.Root(), "data", "sub"))
		for _, file := range []struct{ name, path string }{{"subs-check-yaml", filepath.Join(dir, "output", "all.yaml")}, {"subs-check-uri", filepath.Join(dir, "output", "base64.txt")}} {
			entry := file
			in := importer(entry.name)
			ingest := func(ctx context.Context) error {
				raw, err := stableFile(entry.path)
				if err == nil {
					err = in.ImportText(ctx, string(raw))
				}
				in.report(err)
				return err
			}
			watcher := source.NewWatcher([]string{entry.path}, source.WatcherConfig{Quiet: 5 * time.Second, Sweep: 2 * time.Minute, Ingest: ingest})
			if err := watcher.Start(ctx); err != nil {
				return errors.New("source subscription directory unavailable")
			}
			defer watcher.Close()
		}
	}
	// Optional direct subscriptions: one URL per line, stable configured position identifies its source.
	var remote sync.WaitGroup
	defer func() { cancel(); remote.Wait() }()
	for index, address := range strings.Split(strings.TrimSpace(os.Getenv("SOURCE_SUBSCRIPTIONS")), "\n") {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		in := importer(fmt.Sprintf("subscription-%d", index+1))
		remote.Add(1)
		go func() {
			defer remote.Done()
			ticker := time.NewTicker(runtimeenv.Seconds("PUBLIC_PROXY_REFRESH_INTERVAL_SECONDS", 1800))
			defer ticker.Stop()
			for {
				raw, err := fetchSubscription(ctx, address)
				if err == nil {
					err = in.ImportText(ctx, string(raw))
				}
				in.report(err)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	for _, p := range providers {
		p.Start(ctx)
		defer p.Close()
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := heartbeat("", nil); err != nil {
				cancel()
				return err
			}
		}
	}
}

func stableFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("source file unavailable")
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil || len(raw) > maxInput {
		return nil, errors.New("source file exceeds limit or read failed")
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("source file changed during read")
	}
	return raw, nil
}
func fetchSubscription(ctx context.Context, address string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, errors.New("invalid subscription URL")
	}
	response, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		return nil, errors.New("subscription request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("subscription HTTP failure")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxInput+1))
	if err != nil || len(raw) > maxInput {
		return nil, errors.New("subscription read failed or exceeds limit")
	}
	return raw, nil
}
