package service

import (
	"context"
	"log"
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

// RejectUnreachableEndpoints drops candidates whose TCP endpoint does not
// answer at all, before they ever reach the registry. It is a necessary-not-
// sufficient test: a completed dial says nothing about the protocol behind it,
// so Cloudflare-fronted endpoints still pass and are judged later by a real
// handshake. UDP transports never answer a TCP dial and are left untouched.
//
// The budget matters more than coverage: ingestion is on the critical path, so
// a slow probe environment must degrade to "admit everything", never to a
// stalled or emptied source.
func (r *Repository) RejectUnreachableEndpoints(ctx context.Context, nodes []domain.Node) (map[int]string, error) {
	timeout := Seconds("SOURCE_PROBE_SECONDS", 3)
	if timeout <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, Seconds("SOURCE_PROBE_BUDGET_SECONDS", 45))
	defer cancel()

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

	denied := make(map[int]string)
	var mu sync.Mutex
	sem := make(chan struct{}, Number("SOURCE_PROBE_CONCURRENCY", 32))
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if dialEndpoint(ctx, t.server, t.port, timeout) {
				return
			}
			// A dial killed by the budget says nothing about the endpoint.
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			denied[t.index] = "endpoint-unreachable"
			mu.Unlock()
		}(t)
	}
	wg.Wait()

	// If almost everything looks dead, the probe path is broken (DNS, egress,
	// container network) rather than the batch. Trusting it would silence the
	// source entirely, so admit the batch and let the real probes decide.
	if len(denied) > 0 && len(denied)*10 >= len(targets)*9 {
		log.Printf("source-ingest: probe path looks broken (%d/%d endpoints unreachable), admitting the batch", len(denied), len(targets))
		return nil, nil
	}
	return denied, nil
}
