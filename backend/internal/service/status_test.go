package service

import (
	"context"
	"testing"
)

func TestSingleServiceStatusCountsOnlyItsWorkKinds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO node_explorer_work(kind,key,state,due_at) VALUES
	 ('exit','a','queued',now()-interval '1 minute'),
	 ('risk','b','queued',now()+interval '1 hour'),
	 ('attributes','c','running',now()),('risk','done','done',now()),
	 ('connectivity','d','queued',now()),('recovery','e','running',now()),
 ('recovery','later','queued',now()+interval '1 hour')`)
	f.exec(t, `INSERT INTO node_explorer_work(kind,key,state,due_at,payload) VALUES
 ('recovery','checked','done',now(),'{"recoveryResult":"not-residential"}'),
 ('recovery','timeout','done',now(),'{"recoveryResult":"timed-out"}'),
 ('recovery','skipped','done',now(),'{"recoveryResult":"skipped-datacenter-asn"}')`)
	if err := f.repo.Heartbeat(ctx, "ip-service", ""); err != nil {
		t.Fatal(err)
	}
	single, err := f.repo.Status(ctx, "ip-service")
	if err != nil {
		t.Fatal(err)
	}
	if single.Queued != 1 || single.Scheduled != 1 || single.Running != 1 || single.Restored != 0 || !single.Healthy || single.OldestWaitSeconds < 60 {
		t.Fatalf("single service status: %+v", single)
	}
	all, err := f.repo.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("expected six service statuses, got %d", len(all))
	}
	for _, status := range all {
		if status.Service == single.Service && (status.Queued != single.Queued || status.Scheduled != single.Scheduled || status.Running != single.Running || status.Healthy != single.Healthy || status.Error != single.Error) {
			t.Fatalf("single and full status disagree: %+v / %+v", single, status)
		}
		if status.Service == "residential-recovery" && (status.Checked != 2 || status.Skipped != 1 || status.Running != 1 || status.Queued != 0 || status.Scheduled != 1 || status.OldestWaitSeconds != 0) {
			t.Fatalf("recovery counters include queued work or lost results: %+v", status)
		}
		if status.Service == "api" && (status.Queued != 0 || status.Running != 0) {
			t.Fatalf("API counted worker tasks: %+v", status)
		}
	}
}
