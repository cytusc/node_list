package connectivitybench

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
)

type Instances struct {
	Normal      []transport.MihomoConfig `json:"normal"`
	Residential []transport.MihomoConfig `json:"residential"`
}

// Require benchmark-only DNS names, even if an operator accidentally supplies
// the production environment. Kernels have no published host ports.
func (i Instances) Validate() error {
	seen := map[string]bool{}
	for _, lane := range [][]transport.MihomoConfig{i.Normal, i.Residential} {
		if len(lane) == 0 {
			return fmt.Errorf("both lanes require dedicated benchmark kernels")
		}
		for _, c := range lane {
			a, e := url.Parse(c.Controller)
			b, f := url.Parse(c.ProxyURL)
			if e != nil || f != nil || a.Scheme != "http" || b.Scheme != "http" || a.User != nil || b.User != nil || !strings.HasPrefix(a.Hostname(), "cb-") || a.Hostname() != b.Hostname() || a.Port() != "9090" || b.Port() != "7890" || a.Path != "" || b.Path != "" || a.RawQuery != "" || b.RawQuery != "" || seen[a.Hostname()] {
				return fmt.Errorf("only distinct cb-* benchmark endpoints are allowed")
			}
			seen[a.Hostname()] = true
		}
	}
	return nil
}

type Result struct {
	ID       string `json:"id"`
	Lane     string `json:"lane"`
	Protocol string `json:"protocol"`
	Engine   string `json:"engine"`
	Retired  bool   `json:"retired"`
	Verdict
	QueueMS      float64 `json:"queue_ms"`
	AssessmentMS float64 `json:"assessment_ms"`
}
type LaneMetrics struct {
	Count            int     `json:"count"`
	Reachable        int     `json:"reachable"`
	ConfirmedDead    int     `json:"confirmed_dead"`
	QueueP95MS       float64 `json:"queue_p95_ms"`
	AssessmentP99MS  float64 `json:"assessment_p99_ms"`
	Reloads          int64   `json:"reloads"`
	Skipped          int64   `json:"reload_skipped"`
	Reservations     int     `json:"reservations"`
	ControllerErrors int     `json:"controller_errors"`
	ElapsedSeconds   float64 `json:"elapsed_seconds"`
	ControlRequests  int64   `json:"control_requests"`
	ControlJoins     int64   `json:"control_joins"`
}
type Report struct {
	StartedAt        time.Time   `json:"started_at"`
	SampleCapturedAt time.Time   `json:"sample_captured_at"`
	Concurrency      int         `json:"concurrency"`
	BatchSize        int         `json:"batch_size"`
	Normal           LaneMetrics `json:"normal"`
	Residential      LaneMetrics `json:"residential"`
	Results          []Result    `json:"results"`
	Host             HostMetrics `json:"host"`
	Aborted          string      `json:"aborted,omitempty"`
}
type Options struct {
	Concurrency, BatchSize int
	Policy                 Policy
	Progress               func(lane string, done, total int)
}

func Run(ctx context.Context, s Sample, instances Instances, o Options) (Report, error) {
	r := Report{StartedAt: time.Now().UTC(), SampleCapturedAt: s.CapturedAt, Concurrency: o.Concurrency, BatchSize: o.BatchSize}
	if err := instances.Validate(); err != nil {
		return r, err
	}
	if o.Concurrency < 1 || o.Concurrency > 512 || o.BatchSize < 1 || o.BatchSize > 128 {
		return r, fmt.Errorf("concurrency must be 1..512; batch size 1..128")
	}
	if len(s.References) < 2 {
		return r, fmt.Errorf("at least two independent recent reference nodes required")
	}
	if o.Policy.Probe == nil {
		o.Policy = ProductionPolicy()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var normal, residential []Result
	wg.Add(2)
	go func() {
		defer wg.Done()
		normal, r.Normal = runLane(runCtx, s.Targets, s.References, instances.Normal, o, "normal", cancel)
	}()
	go func() {
		defer wg.Done()
		residential, r.Residential = runLane(runCtx, s.Residential, s.References, instances.Residential, Options{Concurrency: 16, BatchSize: 8, Policy: o.Policy, Progress: o.Progress}, "residential", cancel)
	}()
	wg.Wait()
	r.Results = append(normal, residential...)
	if runCtx.Err() != nil {
		return r, runCtx.Err()
	}
	return r, nil
}

func runLane(ctx context.Context, targets, refs []Candidate, instances []transport.MihomoConfig, o Options, lane string, abort context.CancelFunc) ([]Result, LaneMetrics) {
	start := time.Now()
	metrics := LaneMetrics{}
	engine := transport.NewMihomoEngine(instances, 15*time.Second)
	router := transport.NewRouter(transport.NewNativeEngine(), engine).WithFallback(engine)
	permits := make(chan struct{}, o.Concurrency)
	controls := newControlBudget(refs, 16, 2)
	flights := newControlFlights()
	type wave struct {
		targets []Candidate
		queued  time.Time
	}
	jobs := make(chan wave)
	var mu sync.Mutex
	var results []Result
	progressDone := make(chan struct{})
	defer close(progressDone)
	if o.Progress != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-progressDone:
					return
				case <-ticker.C:
					mu.Lock()
					done := len(results)
					mu.Unlock()
					o.Progress(lane, done, len(targets))
				}
			}
		}()
	}
	var workers sync.WaitGroup
	for range len(instances) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for w := range jobs {
				nodes := make([]domain.Node, 0, len(w.targets)+len(refs))
				for _, c := range w.targets {
					nodes = append(nodes, c.Node)
				}
				// Two references suffice for identity/endpoint exclusion; keep extras if
				// all selected targets happen to share an endpoint with one control.
				selected := referenceSet(w.targets, refs)
				for _, c := range selected {
					nodes = append(nodes, c.Node)
				}
				prepareStarted := time.Now()
				paths, release, err := reserve(ctx, router, nodes)
				prepareDuration := time.Since(prepareStarted)
				mu.Lock()
				metrics.Reservations++
				if err != nil && ctx.Err() == nil {
					metrics.ControllerErrors++
					if float64(metrics.ControllerErrors)/float64(metrics.Reservations) > .01 {
						abort()
					}
				}
				mu.Unlock()
				if err != nil {
					mu.Lock()
					for _, c := range w.targets {
						results = append(results, Result{ID: Token(c.Identity), Lane: lane, Protocol: c.Node.Type, Retired: c.Retired, Verdict: Verdict{Error: "reservation-failed"}, QueueMS: float64(time.Since(w.queued).Microseconds()) / 1000})
					}
					mu.Unlock()
					continue
				}
				var probes sync.WaitGroup
				for j, c := range w.targets {
					probes.Add(1)
					go func(j int, c Candidate) {
						defer probes.Done()
						select {
						case permits <- struct{}{}:
						case <-ctx.Done():
							mu.Lock()
							results = append(results, Result{ID: Token(c.Identity), Lane: lane, Protocol: c.Node.Type, Retired: c.Retired, Verdict: Verdict{Error: "cancelled"}})
							mu.Unlock()
							return
						}
						defer func() { <-permits }()
						admitted := time.Now()
						var references []Reference
						for k := range selected {
							index := (j + k) % len(selected)
							control := selected[index]
							path := paths[len(w.targets)+index]
							if independent(c, control) && path != nil && path.Error == nil && path.Client != nil {
								references = append(references, Reference{ID: Token(control.Identity), Path: path})
								if len(references) == 3 {
									break
								}
							}
						}
						assessmentCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
						policy := o.Policy
						policy.ControlProbe = func(ctx context.Context, c *http.Client, phase, referenceID string) (bool, error) {
							key, id := phase, ""
							if phase == "reference-control" {
								key += ":" + referenceID
								id = referenceID
							}
							return flights.do(ctx, key, func() (bool, error) {
								release, err := controls.acquire(ctx, id)
								if err != nil {
									return false, err
								}
								defer release()
								return policy.Probe(ctx, c)
							})
						}
						v := policy.EvaluateReferences(assessmentCtx, paths[j], references)
						cancel()
						result := Result{ID: Token(c.Identity), Lane: lane, Protocol: c.Node.Type, Retired: c.Retired, Verdict: v, QueueMS: float64(admitted.Sub(w.queued).Microseconds()) / 1000, AssessmentMS: float64((prepareDuration + time.Since(admitted)).Microseconds()) / 1000}
						if engine, ok := router.EngineFor(c.Node); ok {
							result.Engine = engine.Name()
						}
						mu.Lock()
						results = append(results, result)
						mu.Unlock()
					}(j, c)
				}
				probes.Wait()
				release() // immutable configuration until EVERY probe finishes
			}
		}()
	}
dispatch:
	for offset := 0; offset < len(targets); offset += o.BatchSize {
		w := wave{targets: targets[offset:min(offset+o.BatchSize, len(targets))], queued: start}
		select {
		case jobs <- w:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	metrics.ElapsedSeconds = time.Since(start).Seconds()
	metrics.Reloads, metrics.Skipped = engine.Stats()
	metrics.ControlRequests, metrics.ControlJoins = flights.stats()
	metrics.Count = len(results)
	q := make([]float64, 0, len(results))
	a := make([]float64, 0, len(results))
	for _, v := range results {
		if v.Reachable {
			metrics.Reachable++
		}
		if v.ConfirmedDead {
			metrics.ConfirmedDead++
		}
		q = append(q, v.QueueMS)
		a = append(a, v.AssessmentMS)
	}
	metrics.QueueP95MS = percentile(q, .95)
	metrics.AssessmentP99MS = percentile(a, .99)
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	return results, metrics
}
func independent(a, b Candidate) bool {
	return a.Identity != b.Identity && !strings.EqualFold(a.Node.Server, b.Node.Server)
}
func referenceSet(targets, refs []Candidate) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	offset := 0
	if len(targets) > 0 && len(refs) > 0 {
		for _, b := range []byte(Token(targets[0].Identity)) {
			offset = (offset*31 + int(b)) % len(refs)
		}
	}
	for j := range len(refs) {
		r := refs[(j+offset)%len(refs)]
		server := strings.ToLower(r.Node.Server)
		if seen[server] {
			continue
		}
		needed := len(out) < 8
		for _, t := range targets {
			has := false
			for _, s := range out {
				has = has || independent(t, s)
			}
			needed = needed || !has
		}
		if needed {
			out = append(out, r)
			seen[server] = true
		}
	}
	return out
}

type controlBudget struct {
	global chan struct{}
	byID   map[string]chan struct{}
}

func newControlBudget(refs []Candidate, total, perReference int) *controlBudget {
	b := &controlBudget{global: make(chan struct{}, total), byID: map[string]chan struct{}{}}
	for _, r := range refs {
		b.byID[Token(r.Identity)] = make(chan struct{}, perReference)
	}
	return b
}
func (b *controlBudget) acquire(ctx context.Context, id string) (func(), error) {
	local := b.byID[id]
	if local != nil {
		select {
		case local <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case b.global <- struct{}{}:
	case <-ctx.Done():
		if local != nil {
			<-local
		}
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-b.global
			if local != nil {
				<-local
			}
		})
	}, nil
}
func reserve(ctx context.Context, router *transport.Router, nodes []domain.Node) (map[int]*transport.Outbound, func(), error) {
	out := map[int]*transport.Outbound{}
	var releases []func()
	release := func() {
		for _, f := range releases {
			f()
		}
	}
	for e, indexes := range router.Group(nodes) {
		group := make([]domain.Node, len(indexes))
		for j, k := range indexes {
			group[j] = nodes[k]
		}
		paths, done, err := e.Reserve(ctx, group)
		if err != nil {
			release()
			return nil, nil, err
		}
		releases = append(releases, done)
		for j, p := range paths {
			out[indexes[j]] = p
		}
	}
	return out, release, nil
}
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	i := int(float64(len(v))*p+.999999) - 1
	return v[max(0, min(i, len(v)-1))]
}
