package service

import (
	"context"
	"errors"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
	"net/http"
	"testing"
	"time"
)

func TestOrdinaryExitBackoffAndFreshEvidence(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Minute)
	exit := domain.ExitState{CheckedAt: old.Format(time.RFC3339Nano), Incarnation: "same", Error: "failed", FailureStreak: 4}
	live := domain.ConnectivityState{}
	want := old.Add(4 * time.Hour)
	if got := ordinaryExitDue(exit, live, "same", 30*time.Minute, now); !got.Equal(want) {
		t.Fatalf("retry did not back off: %s", got)
	}
	exit.FailureStreak = 16
	if got := ordinaryExitDue(exit, live, "same", 30*time.Minute, now); !got.Equal(old.Add(6 * time.Hour)) {
		t.Fatalf("backoff unbounded: %s", got)
	}
	if got := ordinaryExitDue(exit, live, "new", 30*time.Minute, now); !got.Equal(now) {
		t.Fatal("new config delayed")
	}
	live = domain.ConnectivityState{Reachable: true, CheckedAt: now.Format(time.RFC3339Nano)}
	if got := ordinaryExitDue(exit, live, "same", 30*time.Minute, now); !got.Equal(now) {
		t.Fatal("new successful proof delayed")
	}
	live.Reachable = false
	if got := ordinaryExitDue(exit, live, "same", 30*time.Minute, now); got.Equal(now) {
		t.Fatal("failed proof bypassed backoff")
	}
	exit.Error = ""
	exit.FailureStreak = 0
	if got := ordinaryExitDue(exit, live, "same", 30*time.Minute, now); !got.Equal(old.Add(30 * time.Minute)) {
		t.Fatal("success retained failure delay")
	}
}

func TestExitFailureBackoffPersistsAndFreshConnectivityAdvancesQueuedWork(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.runner.Config.Role = "exit-provision"
	f.runner.Config.HighRouter = transport.NewRouter(cleanupEngine{&http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) { return nil, errors.New("unreachable") })}})
	for attempt := 1; attempt <= 2; attempt++ {
		if err := f.repo.Schedule(ctx, KindExit, f.id, Payload{Identity: f.id}, 1, time.Now()); err != nil {
			t.Fatal(err)
		}
		j := f.claim(t, KindExit)
		if err := f.runner.exit(ctx, j, true); err != nil {
			t.Fatal(err)
		}
		observed, err := f.repo.Exit(ctx, f.id)
		if err != nil || observed.FailureStreak != attempt {
			t.Fatalf("missing failure history: %+v %v", observed, err)
		}
	}
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	var due time.Time
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at FROM node_explorer_work WHERE kind='exit' AND key=$1`, f.id).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.Until(due) < 59*time.Minute {
		t.Fatal("scheduler ignored persisted exit backoff")
	}
	f.exec(t, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,now(),jsonb_build_object('reachable',true,'checkedAt',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')))`, f.id)
	if err := f.runner.schedule(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at FROM node_explorer_work WHERE kind='exit' AND key=$1`, f.id).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.Until(due) > time.Second {
		t.Fatal("new connectivity did not advance queued exit")
	}
}
