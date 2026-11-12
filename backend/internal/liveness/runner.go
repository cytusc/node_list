package liveness

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const Target = "https://www.gstatic.com/generate_204"
const BackupTarget = "https://cp.cloudflare.com/generate_204"

type Store interface {
	Due(context.Context, int) ([]Candidate, error)
	Save(context.Context, Candidate, Result, time.Duration) error
	SaveBatch(context.Context, []SaveItem) error
	Heartbeat(context.Context, any) error
}
type completed struct {
	task   task
	result Result
	xray   bool
}
type task struct {
	node        Candidate
	proxy       Proxy
	fallback    string
	configError string
}
type Runner struct {
	Store                      Store
	Xray, Mihomo               Engine
	XrayWorkers, MihomoWorkers int
	Interval, Retry            time.Duration
	Control                    func(context.Context) bool
	// SaveBatchSize caps how many completed results are flushed in one
	// SaveBatch call (default 128); SaveFlushInterval caps how long a
	// result waits in the buffer (default 5s).
	SaveBatchSize     int
	SaveFlushInterval time.Duration
}

func (r *Runner) Run(parent context.Context) error {
	if r.Store == nil || r.Xray == nil || r.Mihomo == nil || r.Control == nil || r.XrayWorkers < 1 || r.MihomoWorkers < 1 || r.Interval <= 0 || r.Retry <= 0 {
		return errors.New("invalid liveness runner configuration")
	}
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	xq, mq := make(chan task), make(chan task)
	results := make(chan completed, r.XrayWorkers+r.MihomoWorkers)
	// Share the direct control result across failed probes during a host outage.
	var controlMu sync.Mutex
	var controlAt time.Time
	var controlOK bool
	control := func(ctx context.Context) bool {
		controlMu.Lock()
		defer controlMu.Unlock()
		if time.Since(controlAt) > 15*time.Second {
			controlOK = r.Control(ctx)
			controlAt = time.Now()
		}
		return controlOK
	}
	worker := func(q <-chan task, e Engine, xray bool) {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-q:
				res := Result{Status: "config_error", Reason: t.configError, CheckedAt: time.Now().UTC()}
				if t.proxy != nil {
					res = e.Probe(ctx, t.proxy)
				}
				if !xray && res.Status == "unreachable" && !control(ctx) {
					res.Status = "infrastructure"
					res.Reason = "control_unavailable"
				}
				select {
				case results <- completed{t, res, xray}:
				case <-ctx.Done():
					return
				}
			}
		}
	}
	for i := 0; i < r.XrayWorkers; i++ {
		wg.Add(1)
		go worker(xq, r.Xray, true)
	}
	for i := 0; i < r.MihomoWorkers; i++ {
		wg.Add(1)
		go worker(mq, r.Mihomo, false)
	}
	type batch struct {
		nodes []Candidate
		err   error
	}
	inputs := make(chan batch, 1)
	fetch := make(chan struct{}, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-fetch:
				query, stop := context.WithTimeout(ctx, 5*time.Second)
				nodes, err := r.Store.Due(query, 1024)
				stop()
				select {
				case inputs <- batch{nodes, err}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	// One event loop owns bounded pending lists. A full Mihomo pool never
	// blocks dispatch into Xray; fallback results enter the Mihomo pending list.
	var xs, ms []task
	pending := map[string]bool{}
	counts := map[string]int64{}
	last := time.Time{}
	inputError := ""
	fetching := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastBeat := time.Now()
	beat := func() error {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return r.Store.Heartbeat(check, map[string]any{"running": true, "error": inputError, "counts": counts, "lastCompletedAt": last, "xrayQueued": len(xs), "mihomoQueued": len(ms), "inFlight": len(pending)})
	}
	batchSize := r.SaveBatchSize
	if batchSize <= 0 {
		batchSize = 128
	}
	flushInterval := r.SaveFlushInterval
	if flushInterval <= 0 {
		flushInterval = 5 * time.Second
	}
	// Completed results are buffered and persisted in batches instead of
	// one transaction per probe. A node stays in pending until its result
	// is flushed, so it cannot be re-fetched and re-probed while its
	// verdict is still buffered. A failed flush keeps the buffer for the
	// next attempt (capped, oldest dropped) instead of killing the
	// runner; dropped results are re-probed because their next_at was
	// never advanced.
	const maxSaveBuf = 1024
	var saveBuf []SaveItem
	lastFlush := time.Now()
	flushSave := func(flushCtx context.Context) {
		if len(saveBuf) == 0 {
			return
		}
		write, stop := context.WithTimeout(flushCtx, 10*time.Second)
		err := r.Store.SaveBatch(write, saveBuf)
		stop()
		if err != nil {
			if len(saveBuf) > maxSaveBuf {
				drop := len(saveBuf) - maxSaveBuf
				for _, item := range saveBuf[:drop] {
					delete(pending, item.Node.Identity)
				}
				saveBuf = append(saveBuf[:0], saveBuf[drop:]...)
			}
			return
		}
		for _, item := range saveBuf {
			counts[item.Result.Engine+":"+item.Result.Status]++
			delete(pending, item.Node.Identity)
		}
		last = time.Now().UTC()
		saveBuf = saveBuf[:0]
		lastFlush = time.Now()
	}
	if err := beat(); err != nil {
		return err
	}
	fetch <- struct{}{}
	fetching = true
	for {
		var xc, mc chan task
		var xt, mt task
		if len(xs) > 0 {
			xc = xq
			xt = xs[0]
		}
		if len(ms) > 0 {
			mc = mq
			mt = ms[0]
		}
		select {
		case <-ctx.Done():
			// Best-effort flush of buffered results before exit.
			flushSave(context.Background())
			return nil
		case xc <- xt:
			xs[0] = task{}
			xs = xs[1:]
		case mc <- mt:
			ms[0] = task{}
			ms = ms[1:]
		case b := <-inputs:
			fetching = false
			if b.err != nil {
				inputError = "input_query_failed"
				continue
			}
			inputError = ""
			for _, n := range b.nodes {
				if pending[n.Identity] {
					continue
				}
				pending[n.Identity] = true
				p, err := Decode(n.Node)
				if err != nil {
					ms = append(ms, task{node: n, configError: err.Error()})
					continue
				}
				t := task{node: n, proxy: p}
				_, reason := XrayOutbound(p)
				if reason == "" {
					xs = append(xs, t)
				} else {
					t.fallback = "direct:" + reason
					ms = append(ms, t)
				}
			}
		case done := <-results:
			if done.xray && !done.result.Reachable {
				t := done.task
				t.fallback = done.result.Status + ":" + done.result.Reason
				ms = append(ms, t)
				continue
			}
			done.result.Fallback = done.task.fallback
			interval := r.Interval
			if !done.result.Valid() || done.result.Status == "unreachable" {
				interval = r.Retry
			}
			saveBuf = append(saveBuf, SaveItem{Node: done.task.node, Result: done.result, Interval: interval})
			if len(saveBuf) >= batchSize {
				flushSave(ctx)
			}
		case <-tick.C:
			// The 1s tick drives Due fetching; the status heartbeat only
			// needs to refresh every 30s.
			if time.Since(lastBeat) >= 30*time.Second {
				if err := beat(); err != nil {
					return err
				}
				lastBeat = time.Now()
			}
			if len(saveBuf) > 0 && time.Since(lastFlush) >= flushInterval {
				flushSave(ctx)
			}
			if !fetching && len(pending) < 256 {
				fetch <- struct{}{}
				fetching = true
			}
		}
	}
}
func DirectControl(ctx context.Context) bool {
	check, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_, _, _, err := requestTargets(check, &http.Transport{Proxy: nil, DisableKeepAlives: true}, []string{Target, BackupTarget}, http.StatusNoContent)
	return err == nil
}
