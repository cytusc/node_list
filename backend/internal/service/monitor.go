package service

import (
	"context"
	"log"
	"time"
)

// Monitoring is isolated from source fetches and probe/lease transactions.
// A timeout loses one sample, not collection work. Missing hours remain gaps.
func (r *Repository) runMonitor(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		sample, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := r.sampleMonitor(sample)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("monitor: sample unavailable: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (r *Repository) sampleMonitor(ctx context.Context) error {
	// One statement gives queue and coverage figures the same database snapshot.
	// Only unfinished work is read; the partial delivery index excludes history.
	_, err := r.Pool.Exec(ctx, `WITH queues AS (
 SELECT kind,
 count(*) FILTER (WHERE state='queued' AND due_at<=now()) AS due,
 count(*) FILTER (WHERE state='queued' AND due_at>now()) AS scheduled,
 count(*) FILTER (WHERE state='running') AS running,
 count(*) FILTER (WHERE state='running' AND lease_until<now()) AS expired,
 coalesce(max(greatest(0,extract(epoch FROM now()-due_at))) FILTER (WHERE state='queued' AND due_at<=now()),0)::bigint AS oldest_seconds
 FROM node_explorer_work WHERE state<>'done' AND kind IN ('exit','attributes','risk') GROUP BY kind
), coverage AS (
 SELECT count(*) AS total,
 count(*) FILTER (WHERE c.identity IS NOT NULL) AS connectivity,
 count(*) FILTER (WHERE e.ip<>'' AND e.payload->>'verified'='true') AS exit,
 count(*) FILTER (WHERE e.ip<>'' AND e.payload->>'verified'='true'
 AND coalesce(a.payload->'classification'->>'state','')<>'') AS attributes
 FROM node_explorer_node_registry n
 LEFT JOIN node_explorer_connectivity c USING(identity)
 LEFT JOIN node_explorer_exits e USING(identity)
 LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
 WHERE n.available
)
INSERT INTO node_explorer_monitor_samples(sampled_at,payload)
SELECT date_trunc('minute',now()),jsonb_build_object(
 'queues',coalesce((SELECT jsonb_agg(to_jsonb(q) ORDER BY kind) FROM queues q),'[]'::jsonb),
 'coverage',(SELECT to_jsonb(c) FROM coverage c))
ON CONFLICT(sampled_at) DO NOTHING`)
	if err != nil {
		return err
	}
	_, err = r.Pool.Exec(ctx, `DELETE FROM node_explorer_monitor_samples WHERE sampled_at<now()-interval '7 days'`)
	return err
}
