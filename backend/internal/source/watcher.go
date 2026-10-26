// Package source watches the files subs-check publishes and drives ingestion.
//
// subs-check is a third-party service: its code is not ours to change, but its
// schedule is. The previous Node.js service polled three files every five
// seconds and rebuilt the whole projection whenever the dirty flag was set,
// which put a constant CPU floor on the host. Watching the filesystem instead
// makes ingestion event-driven: work happens once per published round, and a
// quiet period guarantees the last file of the round has landed before the
// batch is read.
package source

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher turns filesystem events into debounced ingestion callbacks.
type Watcher struct {
	paths []string
	// quiet is how long the watched files must stay unchanged before a round is
	// considered complete. subs-check writes the results file first and the
	// subscription exports afterwards, so reading on the first event would pick
	// up a half-published round.
	quiet time.Duration
	// sweep is the safety-net interval. Filesystem events can be missed when a
	// directory is replaced, so ingestion also runs periodically.
	sweep  time.Duration
	ingest func(context.Context) error

	cancel  context.CancelFunc
	done    chan struct{}
	runMu   sync.Mutex
	mu      sync.Mutex
	timer   *time.Timer
	lastRun time.Time
	runs    int64
	events  int64
}

// WatcherConfig configures the watcher.
type WatcherConfig struct {
	Quiet  time.Duration
	Sweep  time.Duration
	Ingest func(context.Context) error
}

// NewWatcher builds a watcher over the given files.
func NewWatcher(paths []string, cfg WatcherConfig) *Watcher {
	if cfg.Quiet <= 0 {
		cfg.Quiet = 5 * time.Second
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = time.Minute
	}
	return &Watcher{paths: paths, quiet: cfg.Quiet, sweep: cfg.Sweep, ingest: cfg.Ingest}
}

// Start watches until the context is cancelled. It returns immediately; the
// goroutine exits when the context does.
func (w *Watcher) Start(ctx context.Context) error {
	observer, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create file watcher: %w", err)
	}
	// Watch the parent directories: editors and subs-check replace files rather
	// than writing in place, which drops a watch held on the inode.
	watched := make(map[string]struct{}, len(w.paths))
	relevant := make(map[string]struct{}, len(w.paths))
	for _, path := range w.paths {
		relevant[filepath.Base(path)] = struct{}{}
		dir := filepath.Dir(path)
		if _, ok := watched[dir]; ok {
			continue
		}
		if err := observer.Add(dir); err != nil {
			observer.Close()
			return fmt.Errorf("watch %s: %w", dir, err)
		}
		watched[dir] = struct{}{}
	}

	ctx, cancel := context.WithCancel(ctx)
	w.cancel, w.done = cancel, make(chan struct{})
	go func() {
		defer close(w.done)
		defer observer.Close()
		defer func() {
			w.mu.Lock()
			if w.timer != nil {
				w.timer.Stop()
			}
			w.mu.Unlock()
		}()
		sweep := time.NewTicker(w.sweep)
		defer sweep.Stop()
		// One immediate pass covers rounds published while the service was down.
		w.trigger(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-observer.Events:
				if !ok {
					return
				}
				if _, matches := relevant[filepath.Base(event.Name)]; !matches {
					continue
				}
				if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
					continue
				}
				w.mu.Lock()
				w.events++
				w.mu.Unlock()
				w.debounce(ctx)
			case err, ok := <-observer.Errors:
				if !ok {
					return
				}
				fmt.Printf("file watcher error: %v\n", err)
			case <-sweep.C:
				w.trigger(ctx)
			}
		}
	}()
	return nil
}

// debounce collapses a burst of writes into one ingestion after the quiet period.
func (w *Watcher) debounce(ctx context.Context) {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(w.quiet, func() { w.trigger(ctx) })
	w.mu.Unlock()
}

// trigger serializes callbacks from the timer, sweep and polling fallback.
func (w *Watcher) trigger(ctx context.Context) {
	w.runMu.Lock()
	defer w.runMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if w.ingest == nil {
		return
	}
	if err := w.ingest(ctx); err != nil {
		fmt.Printf("source ingest failed: %v\n", err)
		return
	}
	w.mu.Lock()
	w.lastRun = time.Now()
	w.runs++
	w.mu.Unlock()
}

// Stats reports watcher activity for health output.
func (w *Watcher) Stats() (runs, events int64, lastRun time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs, w.events, w.lastRun
}

// Trigger runs ingestion immediately. Used by the polling fallback when
// inotify is unavailable.
func (w *Watcher) Trigger(ctx context.Context) { w.trigger(ctx) }

// Close cancels file events and drains callbacks before ownership is released.
func (w *Watcher) Close() {
	if w.cancel == nil {
		return
	}
	w.cancel()
	<-w.done
	w.runMu.Lock()
	w.runMu.Unlock()
}
