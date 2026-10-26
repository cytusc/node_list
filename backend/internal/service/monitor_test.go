package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMonitorSamplesSeparateDueScheduledAndRunning(t *testing.T) {
	f := setup(t)
	f.exec(t, `DELETE FROM node_explorer_work`)
	f.exec(t, `UPDATE node_explorer_node_registry SET available=true WHERE identity=$1`, f.id)
	f.exec(t, `INSERT INTO node_explorer_work(kind,key,due_at,state,lease_until,payload) VALUES
 ('exit','due',now()-interval '5 minutes','queued',NULL,'{"password":"never-export"}'),
 ('exit','future',now()+interval '1 hour','queued',NULL,'{}'),
 ('exit','active',now()-interval '1 minute','running',now()-interval '1 second','{}'),
 ('exit','done',now(),'done',NULL,'{}'),
 ('normal-recovery','stalled',now(),'queued',NULL,jsonb_build_object('startedAt',now()-interval '11 minutes'))`)
	f.exec(t, `INSERT INTO node_explorer_connectivity VALUES($1,now(),'{"reachable":false}')`, f.id)
	f.exec(t, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,'192.0.2.2',now(),'{"verified":true}')`, f.id)
	f.exec(t, `INSERT INTO node_explorer_ip_attributes VALUES('192.0.2.2',now(),'{"classification":{"state":"unknown"}}')`)
	f.exec(t, `INSERT INTO node_explorer_monitor_samples VALUES(now()-interval '8 days','{}')`)
	ctx := context.Background()
	if err := f.repo.sampleMonitor(ctx); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_monitor_samples ORDER BY sampled_at DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Queues []struct {
			Kind                             string
			Due, Scheduled, Running, Expired int
			Oldest                           int `json:"oldest_seconds"`
		}
		Coverage struct{ Total, Connectivity, Exit, Attributes int }
		Recovery struct{ Active, Stalled int }
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Queues) != 1 || got.Queues[0].Kind != "exit" || got.Queues[0].Due != 1 || got.Queues[0].Scheduled != 1 || got.Queues[0].Running != 1 || got.Queues[0].Expired != 1 || got.Queues[0].Oldest < 300 {
		t.Fatalf("queues: %+v", got.Queues)
	}
	if got.Coverage.Total != 1 || got.Coverage.Connectivity != 1 || got.Coverage.Exit != 1 || got.Coverage.Attributes != 1 {
		t.Fatalf("coverage/recovery: %+v", got)
	}
	if strings.Contains(string(raw), "never-export") || strings.Contains(string(raw), f.id) {
		t.Fatal("private work or node data exported")
	}
	// Restarts within the same minute must not overwrite history, nor insert duplicates.
	if err := f.repo.sampleMonitor(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_monitor_samples`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retention/dedup: count=%d err=%v", count, err)
	}
}
