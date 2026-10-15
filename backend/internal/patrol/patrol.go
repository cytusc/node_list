// Package patrol rechecks every visible node's reachability and revives
// connections that came back.
//
// Retirement is destructive, so it requires three independent signals: two
// failed proxy attempts, a reachable control host, and a working control proxy.
// An outage in GitHub, the host, DNS or Mihomo therefore defers deletion
// instead of removing the fleet.
package patrol

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/transport"
)

// PatrolRetireReason is the exact wording recovery matches. Any change must
// land here and at every retire call site.
const PatrolRetireReason = "Scheduled GitHub patrol: two failed proxy attempts; direct and proxy controls passed"

// Summary is the outcome of one patrol round.
type Summary struct {
	Total     int `json:"total"`
	Checked   int `json:"checked"`
	Reachable int `json:"reachable"`
	Retired   int `json:"retired"`
	Deferred  int `json:"deferred"`
	Skipped   int `json:"skipped"`
}

// RecoverySummary is the outcome of one revival round.
type RecoverySummary struct {
	ClassificationEnabled bool `json:"classificationEnabled"`
	Checked               int  `json:"checked"`
	Restored              int  `json:"restored"`
	Skipped               int  `json:"skipped"`
}

// Patrol rechecks nodes through the available engines.
type Patrol struct {
	store    *store.Store
	router   *transport.Router
	probe    func(context.Context, *http.Client) (bool, error)
	timeout  time.Duration
	retryGap time.Duration
	// concurrency is the number of nodes probed per wave. The JavaScript
	// service used two and slept ten seconds between waves, which made a round
	// last hours; Go can keep far more tunnels open without extra cost.
	concurrency           int
	residentialAssessment ResidentialAssessment
	residentialVerifier   ResidentialVerifier
	recoveryLimit         int
}

// PatrolConfig configures a patrol.
type PatrolConfig struct {
	ResidentialAssessment ResidentialAssessment
	ResidentialVerifier   ResidentialVerifier
	RecoveryLimit         int
	Concurrency           int
	Timeout               time.Duration
	RetryDelay            time.Duration
}

// NewPatrol builds a patrol runner.
func NewPatrol(st *store.Store, router *transport.Router, cfg PatrolConfig) *Patrol {
	if cfg.RecoveryLimit <= 0 {
		cfg.RecoveryLimit = 20
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 8
	}
	if cfg.Concurrency > 64 {
		cfg.Concurrency = 64
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 12 * time.Second
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 10 * time.Second
	}
	return &Patrol{
		store:                 st,
		residentialVerifier:   cfg.ResidentialVerifier,
		residentialAssessment: cfg.ResidentialAssessment,
		recoveryLimit:         cfg.RecoveryLimit,
		router:                router,
		probe:                 transport.ProbeHTTP,
		timeout:               cfg.Timeout,
		retryGap:              cfg.RetryDelay,
		concurrency:           cfg.Concurrency,
	}
}

// RunOnce walks every visible node in waves.
func (p *Patrol) RunOnce(ctx context.Context, onProgress func(Summary)) (Summary, error) {
	var summary Summary
	if err := p.store.Sync(ctx); err != nil {
		return summary, err
	}
	nodes := []domain.Node{}
	if aggregate := p.store.Aggregate(false); aggregate != nil {
		nodes = aggregate.Nodes
	}
	summary.Total = len(nodes)
	// A node that already passed is the best first candidate for the proxy
	// control, so it goes first.
	sort.SliceStable(nodes, func(i, j int) bool {
		return rankSuccess(nodes[i]) > rankSuccess(nodes[j])
	})

	var reference *domain.Node
	var deferred []domain.Node
	var mu sync.Mutex

	for offset := 0; offset < len(nodes); offset += p.concurrency {
		if err := ctx.Err(); err != nil {
			return summary, ctx.Err()
		}
		wave := nodes[offset:min(offset+p.concurrency, len(nodes))]
		wave = p.dropRetiredAndUnusable(&summary, wave)
		if len(wave) == 0 {
			continue
		}
		if reference != nil && p.isRetired(*reference) {
			reference = nil
		}
		targets := make([]domain.Node, len(wave))
		copy(targets, wave)
		if reference != nil && !containsNode(targets, *reference) {
			targets = append(targets, *reference)
		}
		outbounds, release, err := p.reserve(ctx, targets)
		if err != nil {
			return summary, err
		}
		prepared := make([]domain.Node, 0, len(wave))
		for _, node := range wave {
			if outbounds[identityOf(node)] == nil {
				summary.Skipped++
				continue
			}
			prepared = append(prepared, node)
		}
		wave = prepared
		if len(wave) == 0 {
			if release != nil {
				release()
			}
			if onProgress != nil {
				onProgress(summary)
			}
			continue
		}
		first := p.probeAll(ctx, wave, outbounds)
		mu.Lock()
		summary.Checked += len(wave)
		for _, ok := range first {
			if ok {
				summary.Reachable++
			}
		}
		mu.Unlock()

		var failures []domain.Node
		for i, ok := range first {
			if !ok {
				failures = append(failures, wave[i])
			}
		}
		var good *domain.Node
		for i, ok := range first {
			if ok {
				node := wave[i]
				good = &node
				break
			}
		}
		if len(failures) > 0 {
			select {
			case <-ctx.Done():
				if release != nil {
					release()
				}
				return summary, ctx.Err()
			case <-time.After(p.retryGap):
			}
			second := p.probeAll(ctx, failures, outbounds)
			mu.Lock()
			for _, ok := range second {
				if ok {
					summary.Reachable++
				}
			}
			mu.Unlock()
			var confirmed []domain.Node
			for i, ok := range second {
				if ok {
					if good == nil {
						node := failures[i]
						good = &node
					}
					continue
				}
				confirmed = append(confirmed, failures[i])
			}
			if len(confirmed) > 0 {
				control := good
				if control == nil {
					control = reference
				}
				hostOK := p.probeDirect(ctx)
				proxyOK := hostOK && control != nil && p.probeNode(ctx, *control, outbounds)
				if proxyOK {
					count, err := p.store.RetireNodes(confirmed, PatrolRetireReason)
					if err != nil {
						if release != nil {
							release()
						}
						return summary, err
					}
					mu.Lock()
					summary.Retired += count
					mu.Unlock()
				} else {
					deferred = append(deferred, confirmed...)
				}
			}
		}
		if good != nil {
			reference = good
		}
		mu.Lock()
		summary.Deferred = len(deferred)
		current := summary
		mu.Unlock()
		if release != nil {
			release()
		}
		if onProgress != nil {
			onProgress(current)
		}
	}
	summary.Deferred = len(deferred)
	if onProgress != nil {
		onProgress(summary)
	}
	return summary, nil
}

func (p *Patrol) dropRetiredAndUnusable(summary *Summary, wave []domain.Node) []domain.Node {
	retired := p.store.RetiredNodeIdentities()
	kept := make([]domain.Node, 0, len(wave))
	for _, node := range wave {
		if _, ok := retired[domain.AggregateIdentity(node)]; ok {
			summary.Skipped++
			continue
		}
		if _, err := outboundFor(node); err != nil {
			summary.Skipped++
			continue
		}
		kept = append(kept, node)
	}
	return kept
}

func (p *Patrol) reserve(ctx context.Context, targets []domain.Node) (map[string]*transport.Outbound, func(), error) {
	groups := p.router.Group(targets)
	outbounds := make(map[string]*transport.Outbound, len(targets))
	var releases []func()
	for engine, indexes := range groups {
		nodes := make([]domain.Node, 0, len(indexes))
		for _, i := range indexes {
			nodes = append(nodes, targets[i])
		}
		reserved, release, err := engine.Reserve(ctx, nodes)
		if err != nil {
			for _, r := range releases {
				r()
			}
			return nil, nil, err
		}
		if release != nil {
			releases = append(releases, release)
		}
		// Reserve keys by position inside this engine's node slice.
		for slot, i := range indexes {
			if outbound, ok := reserved[slot]; ok && outbound != nil && outbound.Error == nil {
				outbounds[identityOf(targets[i])] = outbound
			}
		}
	}
	return outbounds, func() {
		for _, r := range releases {
			r()
		}
	}, nil
}

func (p *Patrol) probeAll(ctx context.Context, nodes []domain.Node, outbounds map[string]*transport.Outbound) []bool {
	results := make([]bool, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node domain.Node) {
			defer wg.Done()
			outbound := outbounds[identityOf(node)]
			if outbound == nil {
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
			defer cancel()
			results[i], _ = p.probe(probeCtx, outbound.Client)
		}(i, node)
	}
	wg.Wait()
	return results
}

func (p *Patrol) probeNode(ctx context.Context, node domain.Node, outbounds map[string]*transport.Outbound) bool {
	outbound := outbounds[identityOf(node)]
	if outbound == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	ok, _ := p.probe(probeCtx, outbound.Client)
	return ok
}

// probeDirect verifies the host itself can reach GitHub, so a host or DNS
// outage cannot be mistaken for dead nodes.
func (p *Patrol) probeDirect(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	ok, err := p.probe(probeCtx, &http.Client{})
	if err != nil {
		return false
	}
	return ok
}

func (p *Patrol) isRetired(node domain.Node) bool {
	_, ok := p.store.RetiredNodeIdentities()[domain.AggregateIdentity(node)]
	return ok
}

// RecoverRetiredNodes requires two fresh connectivity and residential checks,
// at least 30 minutes apart, on the same current exit and retirement row.
func (p *Patrol) RecoverRetiredNodes(ctx context.Context, state map[string]RecoveryState, now time.Time, interval time.Duration, onProgress func(RecoverySummary)) (RecoverySummary, error) {
	var summary RecoverySummary
	roundStarted := time.Now()
	summary.ClassificationEnabled = (p.residentialVerifier != nil || p.residentialAssessment != nil)
	if err := p.store.Sync(ctx); err != nil {
		return summary, err
	}
	if interval < 30*time.Minute {
		interval = 30 * time.Minute
	}
	if p.residentialVerifier == nil && p.residentialAssessment == nil {
		return summary, nil
	}
	retired := p.store.RetiredRows()
	identities := make(map[string]struct{}, len(retired))
	candidates := make([]store.RetiredRow, 0, len(retired))
	for _, row := range retired {
		if row.Reason != PatrolRetireReason && !strings.HasPrefix(row.Reason, "IPPure:") {
			continue
		}
		identities[row.Identity] = struct{}{}
		previous, ok := state[row.Identity]
		last, err := time.Parse(time.RFC3339, row.RetiredAt)
		if ok && previous.RetiredAt == row.RetiredAt {
			if parsed, err2 := time.Parse(time.RFC3339, previous.CheckedAt); err2 == nil {
				last = parsed
			}
		}
		if err != nil {
			continue
		}
		if now.Sub(last) >= interval {
			candidates = append(candidates, row)
		}
	}
	for identity := range state {
		if _, ok := identities[identity]; !ok {
			delete(state, identity)
		}
	}
	if len(candidates) == 0 {
		return summary, nil
	}

	nodes := make(map[string]domain.Node, 2048)
	if aggregate := p.store.Aggregate(true); aggregate != nil {
		for _, node := range aggregate.Nodes {
			nodes[domain.AggregateIdentity(node)] = node
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := state[candidates[i].Identity], state[candidates[j].Identity]
		left, right := candidates[i].RetiredAt, candidates[j].RetiredAt
		if a.RetiredAt == candidates[i].RetiredAt && a.CheckedAt != "" {
			left = a.CheckedAt
		}
		if b.RetiredAt == candidates[j].RetiredAt && b.CheckedAt != "" {
			right = b.CheckedAt
		}
		leftTime, _ := time.Parse(time.RFC3339Nano, left)
		rightTime, _ := time.Parse(time.RFC3339Nano, right)
		if leftTime.Equal(rightTime) {
			return candidates[i].Identity < candidates[j].Identity
		}
		return leftTime.Before(rightTime)
	})
	attempted := 0
	for _, row := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if attempted >= p.recoveryLimit {
			break
		}
		attempted++
		// Recording skipped attempts keeps an unusable configuration from
		// permanently occupying the front of this bounded recovery queue.
		previous := state[row.Identity]
		state[row.Identity] = RecoveryState{RetiredAt: row.RetiredAt, CheckedAt: now.UTC().Format(time.RFC3339Nano), Classification: "unknown"}

		node, ok := nodes[row.Identity]
		config, err := outboundFor(node)
		if err != nil || !ok {
			summary.Skipped++
			continue
		}
		engine, ok := p.router.EngineFor(node)
		if !ok {
			summary.Skipped++
			continue
		}
		_ = config
		outbounds, release, err := engine.Reserve(ctx, []domain.Node{node})
		if err != nil {
			summary.Skipped++
			continue
		}
		var success bool
		var currentIP string
		classification := "unknown"
		var voting *ipcheck.Result
		if outbound := outbounds[0]; outbound != nil && outbound.Error == nil {
			probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
			success, _ = p.probe(probeCtx, outbound.Client)
			cancel()
			if success {
				verifyCtx, verifyCancel := context.WithTimeout(ctx, 60*time.Second)
				if p.residentialAssessment != nil {
					var votes ipcheck.Result
					currentIP, votes, err = p.residentialAssessment(verifyCtx, outbound.Client)
					voting = &votes
				} else {
					currentIP, err = p.residentialVerifier(verifyCtx, outbound.Client)
				}
				verifyCancel()
				ip := net.ParseIP(currentIP)
				success = err == nil && ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsPrivate()
				if success {
					classification = "residential"
				} else if errors.Is(err, ErrResidentialDisputed) {
					classification = "disputed"
				} else if errors.Is(err, ErrResidentialHosting) {
					classification = "datacenter"
				}
			}
		} else {
			if release != nil {
				release()
			}
			summary.Skipped++
			continue
		}
		if release != nil {
			release()
		}
		successes := 0
		if success {
			lastCheck, parseErr := time.Parse(time.RFC3339Nano, previous.CheckedAt)
			if previous.RetiredAt == row.RetiredAt && previous.VerifiedIP == currentIP && previous.ClassificationVersion == ipcheck.VotingVersion && parseErr == nil && now.Sub(lastCheck) <= 24*time.Hour {
				successes = previous.Successes + 1
			} else {
				successes = 1
			}
		}
		state[row.Identity] = RecoveryState{
			RetiredAt:             row.RetiredAt,
			CheckedAt:             now.Add(time.Since(roundStarted)).UTC().Format(time.RFC3339Nano),
			Successes:             successes,
			Success:               success,
			VerifiedIP:            currentIP,
			ClassificationVersion: ipcheck.VotingVersion,
			Voting:                voting,
			Classification:        classification,
		}
		summary.Checked++
		if successes >= 2 {
			restored, err := p.store.RestoreNode(row.Identity, row.RetiredAt, row.Reason)
			if err != nil {
				return summary, err
			}
			summary.Restored += int(restored)
			delete(state, row.Identity)
		}
		if onProgress != nil {
			onProgress(summary)
		}
	}
	return summary, nil
}

// RecoveryState tracks consecutive revival successes across restarts.
type RecoveryState struct {
	Voting                *ipcheck.Result `json:"voting,omitempty"`
	Classification        string          `json:"classification,omitempty"`
	VerifiedIP            string          `json:"verifiedIP,omitempty"`
	ClassificationVersion int             `json:"classificationVersion,omitempty"`
	RetiredAt             string          `json:"retiredAt"`
	CheckedAt             string          `json:"checkedAt"`
	Successes             int             `json:"successes"`
	Success               bool            `json:"success"`
}

func rankSuccess(node domain.Node) int {
	if node.IPPure != nil && node.IPPure.Status == "success" {
		return 1
	}
	return 0
}

func containsNode(nodes []domain.Node, target domain.Node) bool {
	identity := domain.AggregateIdentity(target)
	for _, node := range nodes {
		if domain.AggregateIdentity(node) == identity {
			return true
		}
	}
	return false
}

func identityOf(node domain.Node) string { return domain.AggregateIdentity(node) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// outboundFor resolves a node's outbound configuration, either from the stored
// Clash definition or by converting its archived share link.
func outboundFor(node domain.Node) (domain.ProxyConfig, error) {
	if len(node.RawConfig) > 0 {
		return node.RawConfig, nil
	}
	if node.RawLink != "" {
		return domain.ShareLinkProxy(node.RawLink, node.Name)
	}
	return nil, errors.New("missing connection configuration")
}
