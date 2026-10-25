package service

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

// RejectConfirmedFaults prevents repeated imports of the same automatically
// retired connection, including equivalent share-link/Clash variants. The
// original registry/source row remains available to scheduled recovery. A
// restored connection or changed configuration is eligible on the next import.
// Neither a lone exit failure nor a policy/manual retirement is evidence here.
func (r *Repository) RejectConfirmedFaults(ctx context.Context, nodes []domain.Node) (map[int]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys := make([]string, 0, len(nodes))
	positions := make(map[string][]int, len(nodes))
	for index, node := range nodes {
		key := domain.ConnectionKey(node)
		if key == "" {
			continue
		}
		if _, exists := positions[key]; !exists {
			keys = append(keys, key)
		}
		positions[key] = append(positions[key], index)
	}
	denied := make(map[int]string)
	// Bound SQL array/response size without one query per candidate. No
	// network probes or shared-kernel reloads run in the ingestion critical path.
	for start := 0; start < len(keys); start += 1000 {
		rows, err := r.Pool.Query(ctx, `SELECT DISTINCT n.connection_key
 FROM node_explorer_node_registry n JOIN node_explorer_retired_nodes t USING(identity)
 WHERE n.connection_key=ANY($1::text[]) AND n.connection_key<>'' AND n.available AND t.reason=$2
 AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`, keys[start:min(start+1000, len(keys))], store.RetireRestoreReason)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			for _, index := range positions[key] {
				denied[index] = "confirmed-retired-connection"
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return denied, nil
}

// RejectRecentlyDeleted stops a connection that was already judged dead from
// re-entering through the same source that keeps re-delivering it. Without this
// the deletion is undone within minutes: the identity leaves the registry, the
// next import sees an unknown identity and re-admits it with a fresh
// first_seen_at, which also restarts the seven-day age gate. The cooldown is
// deliberately finite so a genuinely repaired node still comes back.
func (r *Repository) RejectRecentlyDeleted(ctx context.Context, nodes []domain.Node) (map[int]string, error) {
	// The cooldown is a throttle, not a verdict: this deployment judges a node
	// dead after fifteen minutes, so blocking it for days would delay the
	// recovery of a repaired one far beyond the evidence. Half a day is long
	// enough to break the import thrash and short enough to notice a fix.
	hours := EnvInt("SOURCE_DELETED_COOLDOWN_HOURS", 12, 0, 8760)
	if hours <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	identities := make([]string, 0, len(nodes))
	positions := make(map[string][]int, len(nodes))
	for index, node := range nodes {
		id := domain.AggregateIdentity(node)
		if id == "" {
			continue
		}
		if _, exists := positions[id]; !exists {
			identities = append(identities, id)
		}
		positions[id] = append(positions[id], index)
	}
	denied := make(map[int]string)
	for start := 0; start < len(identities); start += 1000 {
		rows, err := r.Pool.Query(ctx, `SELECT identity FROM node_explorer_deleted_nodes
 WHERE identity=ANY($1::text[]) AND deleted_at>now()-make_interval(hours=>$2)`,
			identities[start:min(start+1000, len(identities))], hours)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			for _, index := range positions[id] {
				denied[index] = "recently-deleted"
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return denied, nil
}

// probePathHealthy answers the only question that matters when a batch looks
// mostly dead: is the probe path working? These sources are routinely 80-90%
// garbage, so the dead ratio says nothing about our own egress and must never be
// used to override the verdicts. A known-good endpoint can answer it directly.
func probePathHealthy(ctx context.Context, timeout time.Duration) bool {
	controls := strings.Split(Env("SOURCE_PROBE_CONTROL", "1.1.1.1:443,8.8.8.8:53,223.5.5.5:443"), ",")
	ctx, cancel := context.WithTimeout(ctx, min(timeout, 5*time.Second))
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	healthy := false
	for _, control := range controls {
		host, port, err := net.SplitHostPort(strings.TrimSpace(control))
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(host, port string) {
			defer wg.Done()
			if !dialEndpoint(ctx, host, port, timeout) {
				return
			}
			mu.Lock()
			healthy = true
			mu.Unlock()
		}(host, port)
	}
	wg.Wait()
	return healthy
}

// RejectUnreachableEndpoints drops candidates whose TCP endpoint does not
// answer at all, before they ever reach the registry. It is a necessary-not-
// sufficient test: a completed dial says nothing about the protocol behind it,
// so Cloudflare-fronted endpoints still pass and are judged later by a real
// handshake. UDP transports never answer a TCP dial and are left untouched.
//
// Two things matter more than coverage, because ingestion is on the critical
// path: a broken probe path must degrade to "admit everything" rather than
// silence a source, and a slow batch must not stall the import. Both are
// handled by a control probe and a hard budget, never by the dead ratio.
func (r *Repository) RejectUnreachableEndpoints(ctx context.Context, nodes []domain.Node) (map[int]string, error) {
	timeout := time.Duration(EnvInt("SOURCE_PROBE_SECONDS", 3, 0, 30)) * time.Second
	if timeout <= 0 {
		return nil, nil
	}

	type candidate struct {
		index  int
		server string
		port   string
	}
	targets := make([]candidate, 0, len(nodes))
	for index, node := range nodes {
		if !tcpVerifiable(node) {
			continue
		}
		targets = append(targets, candidate{index: index, server: node.Server, port: node.Port})
	}
	if len(targets) == 0 {
		return nil, nil
	}
	if !probePathHealthy(ctx, timeout) {
		log.Printf("source-ingest: control endpoints unreachable, admitting %d candidates unprobed", len(targets))
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(EnvInt("SOURCE_PROBE_BUDGET_SECONDS", 120, 1, 600))*time.Second)
	defer cancel()

	denied := make(map[int]string)
	var mu sync.Mutex
	queue := make(chan candidate)
	var wg sync.WaitGroup
	workers := min(EnvInt("SOURCE_PROBE_CONCURRENCY", 256, 1, 512), len(targets))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range queue {
				if ctx.Err() != nil {
					return
				}
				if dialEndpoint(ctx, t.server, t.port, timeout) {
					continue
				}
				// A dial killed by the budget says nothing about the endpoint.
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				denied[t.index] = "endpoint-unreachable"
				mu.Unlock()
			}
		}()
	}
	queued := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		queue <- t
		queued++
	}
	close(queue)
	wg.Wait()

	if unprobed := len(targets) - queued; unprobed > 0 {
		log.Printf("source-ingest: probe budget spent, %d of %d candidates admitted unprobed", unprobed, len(targets))
	}
	return denied, nil
}
