package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/transport"
)

// Backoff ladder for nodes that fail before any exit IP is observed. Escalating
// quickly keeps a backlog of dead nodes from consuming probe windows that fresh
// nodes need.
const (
	backoffBase     = 30 * time.Minute
	backoffSixHour  = 6 * time.Hour
	backoffOneDay   = 24 * time.Hour
	retireAttempts  = 4
	githubProbeWait = 12 * time.Second
)

// Scheduler drives IPPure collection across the registered engines.
type Scheduler struct {
	store       *store.Store
	router      *transport.Router
	apiOrigin   string
	concurrency int
	interval    time.Duration

	// slowShare is the fraction of the pool reserved for nodes already known to
	// fail. Without it a wave of dead nodes — each burning the full multi-provider
	// timeout budget — occupies every slot and starves never-checked nodes.
	slowShare float64

	mu       sync.Mutex
	running  bool
	lastErr  string
	lastRun  time.Time
	lastSize int
	probed   int64

	preferred sync.Map // identity -> []string of echo endpoints that won before

	githubProbe func(ctx context.Context, client *http.Client) (bool, error)
}

// SchedulerConfig configures collection.
type SchedulerConfig struct {
	Concurrency int
	Interval    time.Duration
	APIOrigin   string
	SlowShare   float64
}

// NewScheduler builds a collector scheduler.
func NewScheduler(st *store.Store, router *transport.Router, cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.Concurrency < 1 || cfg.Concurrency > 64 {
		return nil, fmt.Errorf("IPPure 并发数必须是 1 到 64 的整数, got %d", cfg.Concurrency)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.APIOrigin == "" {
		cfg.APIOrigin = "https://api.123169.xyz"
	}
	if cfg.SlowShare <= 0 || cfg.SlowShare >= 1 {
		cfg.SlowShare = 0.25
	}
	return &Scheduler{
		store:       st,
		router:      router,
		apiOrigin:   cfg.APIOrigin,
		concurrency: cfg.Concurrency,
		interval:    cfg.Interval,
		slowShare:   cfg.SlowShare,
		githubProbe: ProbeGitHub,
	}, nil
}

// Status is the collector health surfaced by the API.
type Status struct {
	Enabled     bool   `json:"enabled"`
	Running     bool   `json:"running"`
	Concurrency int    `json:"concurrency"`
	Transport   string `json:"transport"`
	Queued      int    `json:"queued"`
	Probed      int64  `json:"probed"`
	LastRun     string `json:"lastRunAt,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Status reports the current collector state.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{
		Enabled:     true,
		Running:     s.running,
		Concurrency: s.concurrency,
		Transport:   "multi-engine",
		Queued:      s.lastSize,
		Probed:      atomic.LoadInt64(&s.probed),
		Error:       s.lastErr,
	}
	if !s.lastRun.IsZero() {
		out.LastRun = s.lastRun.UTC().Format(time.RFC3339)
	}
	return out
}

// Run executes one collection round.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.lastRun = time.Now()
		s.mu.Unlock()
	}()

	jobs, err := s.buildQueue(ctx)
	if err != nil {
		s.setError(err)
		return err
	}
	s.mu.Lock()
	s.lastSize = len(jobs)
	s.mu.Unlock()
	if len(jobs) == 0 {
		s.setError(nil)
		return nil
	}
	if err := s.runJobs(ctx, jobs); err != nil {
		s.setError(err)
		return err
	}
	s.setError(nil)
	return nil
}

func (s *Scheduler) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.lastErr = ""
		return
	}
	message := err.Error()
	// Never persist proxy credentials or controller response bodies.
	if len(message) > 160 {
		message = message[:160]
	}
	s.lastErr = "IPPure 采集暂时不可用：" + message
}

// job is one probe to perform.
type job struct {
	snapshotID int
	index      int
	identity   string
	node       domain.Node
	old        *domain.IPPureResult

	shareOnly  bool
	visible    bool
	priority   int
	firstSeen  int
	attempts   int
	backfill   bool
	knownDead  bool
	reuseExit  bool
	exitIP     string
	exitSource string
}

// buildQueue selects and orders the work for one round.
//
// Ordering is deliberate: raw snapshot index order structurally starves nodes
// parked near the tail of every batch, because a round's budget only reaches
// the first few hundred entries. Equal priority therefore falls back to the
// batch in which an identity first appeared, so a small country group is not
// permanently stuck behind a large one.
func (s *Scheduler) buildQueue(ctx context.Context) ([]job, error) {
	contextData := s.store.GetCollectionContext()
	snapshot := contextData.Snapshot
	historical := contextData.Candidates
	if snapshot == nil && len(historical) == 0 {
		return nil, nil
	}

	type entry struct {
		snapshotID int
		index      int
		node       domain.Node
		identity   string
	}
	var entries []entry
	if snapshot != nil {
		for i, node := range snapshot.Snapshot.Nodes {
			index := i
			if i < len(snapshot.Indexes) {
				index = snapshot.Indexes[i]
			}
			identity := ""
			if i < len(snapshot.Identities) {
				identity = snapshot.Identities[i]
			}
			if identity == "" {
				identity = domain.AggregateIdentity(node)
			}
			entries = append(entries, entry{snapshot.Snapshot.ID, index, node, identity})
		}
	}
	for _, candidate := range historical {
		if snapshot != nil && candidate.SnapshotID == snapshot.Snapshot.ID {
			continue
		}
		entries = append(entries, entry{candidate.SnapshotID, candidate.Index, candidate.Node, candidate.Identity})
	}

	firstSeen := make(map[string]int, len(entries))
	for _, e := range entries {
		if previous, ok := firstSeen[e.identity]; !ok || e.snapshotID < previous {
			firstSeen[e.identity] = e.snapshotID
		}
	}

	jobs := make([]job, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		shareOnly := len(e.node.RawConfig) == 0 && e.node.RawLink != ""
		if shareOnly {
			if _, ok := seen[e.identity]; ok {
				continue
			}
			seen[e.identity] = struct{}{}
		}
		saved := contextData.AggregateIP[e.identity]
		if saved == nil {
			saved = e.node.IPPure
		}
		outdatedLink := saved != nil && saved.FailureStage == "share-link" &&
			saved.ShareLinkVersion != domain.ShareLinkVersion
		outdatedProbe := saved != nil && saved.Status == "error" && saved.IP == "" &&
			saved.FailureStage != "share-link" &&
			saved.ExitIPProbeVersion != domain.ExitIPProbeVersion
		priority := 0
		switch {
		case !shareOnly:
			priority = 0
		case outdatedLink || outdatedProbe:
			priority = 4
		case saved == nil:
			priority = 2
		}
		if _, visible := contextData.VisibleSet[fmt.Sprintf("%d:%s", e.snapshotID, e.identity)]; visible {
			priority++
		}
		if shareOnly && saved != nil && saved.FailureStage == "share-link" &&
			saved.ShareLinkVersion == domain.ShareLinkVersion {
			continue
		}
		currentExitIP := domain.HasVerifiedExitIP(saved)
		currentRetry := saved != nil && saved.ExitIPMode == "proxy-echo" &&
			saved.QueryMode == "specified-exit" && saved.TransportMode == "http" &&
			saved.SignatureVersion == 2 && saved.ExitIPVersion == domain.ExitIPVersion &&
			(saved.Status != "error" || saved.IP != "" || saved.ExitIPProbeVersion == domain.ExitIPProbeVersion)
		backfill := saved != nil && saved.Status == "success" && currentExitIP && saved.Organization == ""
		needsCountry := saved != nil && saved.Status == "success" && currentExitIP && saved.Country == ""
		if saved != nil && saved.Status == "success" && currentExitIP && !backfill && !needsCountry {
			continue
		}
		attempts := 0
		if currentRetry && saved != nil {
			attempts = saved.Attempts
		}
		lastAttempt := ""
		switch {
		case backfill && saved != nil && saved.SignatureVersion == 2:
			lastAttempt = saved.OrganizationCheckedAt
		case currentRetry && saved != nil:
			lastAttempt = saved.CheckedAt
		}
		exitProbeFailures := saved != nil && saved.Status == "error" && saved.IP == "" &&
			saved.FailureStage == "exit-ip"
		backoff := backoffBase
		if exitProbeFailures {
			switch {
			case attempts >= 3:
				backoff = backoffOneDay
			case attempts >= 2:
				backoff = backoffSixHour
			}
		}
		if lastAttempt != "" {
			if at, err := time.Parse(time.RFC3339, lastAttempt); err == nil {
				if time.Since(at) < backoff {
					continue
				}
			}
		}
		reuseExit := currentExitIP && saved != nil && saved.IP != "" && isIPv4(saved.IP)
		jobs = append(jobs, job{
			snapshotID: e.snapshotID,
			index:      e.index,
			identity:   e.identity,
			node:       e.node,
			old:        saved,
			shareOnly:  shareOnly,
			priority:   priority,
			firstSeen:  firstSeen[e.identity],
			attempts:   attempts,
			backfill:   backfill,
			knownDead:  exitProbeFailures,
			reuseExit:  reuseExit,
			exitIP:     savedIP(saved),
			exitSource: savedSource(saved),
		})
	}
	sort.SliceStable(jobs, func(a, b int) bool {
		if jobs[a].priority != jobs[b].priority {
			return jobs[a].priority > jobs[b].priority
		}
		return jobs[a].firstSeen < jobs[b].firstSeen
	})
	return jobs, nil
}

func savedIP(r *domain.IPPureResult) string {
	if r == nil {
		return ""
	}
	return r.IP
}

func savedSource(r *domain.IPPureResult) string {
	if r == nil {
		return ""
	}
	return r.ExitIPSource
}

func isIPv4(s string) bool {
	if s == "" {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			dots++
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return dots == 3
}

// runJobs executes the queue, splitting it across engines and lanes.
func (s *Scheduler) runJobs(ctx context.Context, jobs []job) error {
	groups := make(map[transport.Engine][]job)
	var order []transport.Engine
	for _, j := range jobs {
		engine, ok := s.router.EngineFor(j.node)
		if !ok {
			continue
		}
		if _, seen := groups[engine]; !seen {
			order = append(order, engine)
		}
		groups[engine] = append(groups[engine], j)
	}
	var firstErr error
	var errOnce sync.Once
	record := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() { firstErr = err })
	}

	total := s.concurrency
	slowSlots := int(float64(total) * s.slowShare)
	if slowSlots < 1 {
		slowSlots = 1
	}
	fastSlots := total - slowSlots
	if fastSlots < 1 {
		fastSlots = 1
	}

	for _, engine := range order {
		engineJobs := groups[engine]
		waveSize := total * 3
		for offset := 0; offset < len(engineJobs); offset += waveSize {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			end := offset + waveSize
			if end > len(engineJobs) {
				end = len(engineJobs)
			}
			wave := engineJobs[offset:end]
			wave = s.dropRetired(wave)
			if len(wave) == 0 {
				continue
			}
			if err := s.runWave(ctx, engine, wave, fastSlots, slowSlots); err != nil {
				record(err)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		}
	}
	return firstErr
}

// dropRetired filters a wave against the in-memory retirement set. The
// JavaScript collector asked the database worker once per job; here the check
// is a map lookup, which removes an IPC round-trip from the hot path.
func (s *Scheduler) dropRetired(wave []job) []job {
	retired := s.store.RetiredNodeIdentities()
	if len(retired) == 0 {
		return wave
	}
	kept := wave[:0:0]
	for _, j := range wave {
		if _, ok := retired[j.identity]; ok {
			continue
		}
		kept = append(kept, j)
	}
	return kept
}

// runWave reserves one engine configuration and drains the wave through two
// lanes with independent budgets.
func (s *Scheduler) runWave(ctx context.Context, engine transport.Engine, wave []job, fastSlots, slowSlots int) error {
	nodes := make([]domain.Node, len(wave))
	for i, j := range wave {
		nodes[i] = j.node
	}
	outbounds, release, err := engine.Reserve(ctx, nodes)
	if err != nil {
		return err
	}
	if release != nil {
		defer release()
	}

	fast := make(chan job, len(wave))
	slow := make(chan job, len(wave))
	for _, j := range wave {
		if j.knownDead {
			slow <- j
		} else {
			fast <- j
		}
	}
	close(fast)
	close(slow)

	asnCache := NewASNCache()
	// A node that succeeds in this wave proves the probe infrastructure works,
	// which is the control required before retiring anything.
	var healthy atomic.Bool

	group, groupCtx := errgroup.WithContext(ctx)
	for lane, slots := range map[string]int{"fast": fastSlots, "slow": slowSlots} {
		laneSlots := slots
		source := fast
		if lane == "slow" {
			source = slow
		}
		for worker := 0; worker < laneSlots; worker++ {
			group.Go(func() error {
				for j := range source {
					if groupCtx.Err() != nil {
						return groupCtx.Err()
					}
					outbound := outbounds[j.index]
					if outbound == nil {
						continue
					}
					result := s.runJob(groupCtx, j, outbound, asnCache)
					if result != nil && result.Status == "success" {
						healthy.Store(true)
					}
					if err := s.finish(groupCtx, j, outbound, result, healthy.Load()); err != nil {
						// A single node's persistence failure must not abort the wave.
						fmt.Printf("collector: persist failed for %s: %v\n", j.identity, err)
					}
					atomic.AddInt64(&s.probed, 1)
				}
				return nil
			})
		}
	}
	err = group.Wait()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// runJob performs one probe and returns the result to persist.
func (s *Scheduler) runJob(ctx context.Context, j job, outbound *transport.Outbound, asnCache *ASNCache) *domain.IPPureResult {
	result := domain.IPPureResult{
		CheckedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		Attempts:           j.attempts + 1,
		QueryMode:          "specified-exit",
		ExitIPMode:         "proxy-echo",
		TransportMode:      "http",
		SignatureVersion:   2,
		ExitIPVersion:      domain.ExitIPVersion,
		ExitIPProbeVersion: domain.ExitIPProbeVersion,
	}
	stage := "exit-ip"
	client := outbound.Client
	timeout := 5 * time.Second

	var preferred []string
	if cached, ok := s.preferred.Load(j.identity); ok {
		if list, ok := cached.([]string); ok {
			preferred = list
		}
	}
	var resolved *ExitOutcome
	if j.reuseExit && j.exitIP != "" {
		resolved = &ExitOutcome{
			IP:            j.exitIP,
			Provider:      j.exitSource,
			AddressFamily: 4,
		}
		if j.old != nil {
			resolved.Country = j.old.Country
		}
		if resolved.Country == "" {
			if country, err := ResolveExitCountry(ctx, client, timeout); err == nil {
				resolved.Country = country
			}
		}
	} else {
		var attempts []domain.ExitIPAttempt
		outcome, err := ResolveExitIPInfo(ctx, client, timeout, preferred, func(a ExitAttempt) {
			attempts = append(attempts, domain.ExitIPAttempt{
				Provider:  a.Provider,
				Endpoint:  a.Endpoint,
				Round:     a.Round,
				ElapsedMs: a.ElapsedMs,
				Outcome:   a.Outcome,
				Code:      a.Code,
			})
		})
		result.ExitIPAttempts = attempts
		if err != nil || outcome == nil {
			return s.failureResult(ctx, j, result, stage, err)
		}
		resolved = outcome
		if outcome.Endpoint != "" {
			s.preferred.Store(j.identity, []string{outcome.Endpoint})
		}
	}
	result.IP = resolved.IP
	result.ExitIPSource = resolved.Provider
	if resolved.Country != "" {
		result.Country = resolved.Country
	}
	result.AddressFamily = resolved.AddressFamily
	result.QueryTarget = resolved.IP
	if resolved.AddressFamily == 6 {
		result.Status = "unsupported"
		result.FailureStage = "unsupported-ipv6"
		result.Error = "已获取 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6"
		return &result
	}

	stage = "ippure-query"
	verdict, err := QueryIPPure(ctx, NewIPPureClient(client, s.apiOrigin, 10*time.Second), resolved.IP, asnCache)
	if err != nil {
		return s.failureResult(ctx, j, result, stage, err)
	}
	result.Status = "success"
	result.ASN = verdict.ASN
	result.Organization = verdict.Organization
	result.Source = verdict.Source
	result.Attributes = verdict.Attributes
	result.HumanPercent = verdict.HumanPercent
	result.BotPercent = verdict.BotPercent
	result.Score = verdict.Score
	result.Label = verdict.Label
	return &result
}

// failureResult shapes a failed probe, preserving an already observed IPv6 exit
// so a later unreachable echo service cannot turn it into a node failure.
func (s *Scheduler) failureResult(ctx context.Context, j job, result domain.IPPureResult, stage string, cause error) *domain.IPPureResult {
	if stage == "exit-ip" && j.old != nil && j.old.Status == "unsupported" &&
		domain.HasVerifiedExitIP(j.old) && isIPv6(j.old.IP) {
		result.Status = "unsupported"
		result.IP = j.old.IP
		result.ExitIPSource = j.old.ExitIPSource
		result.AddressFamily = 6
		result.FailureStage = "unsupported-ipv6"
		result.Error = j.old.Error
		return &result
	}
	if j.backfill && j.old != nil {
		// Keep previously collected metrics when only the organization refresh fails.
		preserved := *j.old
		preserved.SignatureVersion = 2
		if j.old.SignatureVersion == 2 {
			preserved.OrganizationAttempts = j.old.OrganizationAttempts + 1
		} else {
			preserved.OrganizationAttempts = 1
		}
		preserved.OrganizationCheckedAt = result.CheckedAt
		return &preserved
	}
	result.Status = "error"
	result.FailureStage = stage
	result.Error = sanitizeFailure(cause, stage)
	return &result
}

func sanitizeFailure(cause error, stage string) string {
	if cause == nil {
		return ""
	}
	message := cause.Error()
	if len(message) > 160 {
		message = message[:160]
	}
	if containsAny(message, "IPPure", "IP.SB", "代理配置") {
		return message
	}
	if stage == "exit-ip" {
		return "节点 IPv4 出口探测失败，请检查节点连接后重试"
	}
	return "IPPure 查询代理连接失败，请稍后重试"
}

func containsAny(s string, parts ...string) bool {
	for _, part := range parts {
		if len(part) > 0 && indexOf(s, part) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func isIPv6(s string) bool {
	return s != "" && indexOf(s, ":") >= 0
}

// finish persists one result and retires the node when the evidence is
// conclusive: four failed exit-IP attempts plus a healthy run, corroborated by
// a GitHub probe over the node's own tunnel so a blocked echo service cannot
// delete a reachable node.
func (s *Scheduler) finish(ctx context.Context, j job, outbound *transport.Outbound, result *domain.IPPureResult, healthy bool) error {
	if result == nil {
		return nil
	}
	if err := s.store.SaveIPPure(j.snapshotID, j.index, *result, j.identity); err != nil {
		return err
	}
	if result.Status != "error" || result.FailureStage != "exit-ip" ||
		result.Attempts < retireAttempts || !healthy {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, githubProbeWait)
	defer cancel()
	alive, err := s.githubProbe(probeCtx, outbound.Client)
	if err != nil {
		return nil
	}
	if alive {
		corroborated := *result
		corroborated.GitHubReachable = true
		return s.store.SaveIPPure(j.snapshotID, j.index, corroborated, j.identity)
	}
	_, err = s.store.RetireNodes([]domain.Node{j.node},
		fmt.Sprintf("IPPure: exit-IP probe failed %d times", result.Attempts))
	return err
}

// ProbeGitHub reports whether GitHub is reachable through a node's tunnel.
func ProbeGitHub(ctx context.Context, client *http.Client) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "node-explorer-github-patrol")
	response, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
	}()
	return response.StatusCode == 200, nil
}
