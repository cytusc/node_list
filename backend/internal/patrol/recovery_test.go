package patrol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/sqlite"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func recoveryFixture(t *testing.T, count int, verify ResidentialVerifier) (*Patrol, []domain.Node, time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "recovery.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	nodes := make([]domain.Node, count)
	for i := range nodes {
		nodes[i] = domain.Node{Name: fmt.Sprint(i), Type: "ss", Server: fmt.Sprintf("192.0.2.%d", i+1), Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": fmt.Sprintf("192.0.2.%d", i+1), "port": 443}}
	}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: nodes})
	if err := db.Insert(ctx, "fixture", raw, "", count); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, node := range nodes {
		// A failed latest enrichment with a machine label must not prevent a
		// fresh current residential verification from being attempted.
		raw, _ := json.Marshal(domain.IPPureResult{Status: "error", Attributes: []string{"机房IP"}})
		if err := db.SaveIPPure(ctx, 1, i, raw, domain.AggregateIdentity(node)); err != nil {
			t.Fatal(err)
		}
		if err := db.RetireNodes(ctx, []string{domain.AggregateIdentity(node)}, now.Add(-time.Hour).Format(time.RFC3339Nano), PatrolRetireReason); err != nil {
			t.Fatal(err)
		}
	}
	st := store.New(db, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	runner := NewPatrol(st, transport.NewRouter(&testEngine{}), PatrolConfig{ResidentialVerifier: verify})
	runner.probe = func(context.Context, *http.Client) (bool, error) { return true, nil }
	return runner, nodes, now
}

func TestRecoveryNeverTrustsLegacyCountsOrShortIntervals(t *testing.T) {
	runner, nodes, now := recoveryFixture(t, 1, func(context.Context, *http.Client) (string, error) { return "8.8.8.8", nil })
	id := domain.AggregateIdentity(nodes[0])
	retiredAt := runner.store.RetiredRows()[0].RetiredAt
	state := map[string]RecoveryState{id: {RetiredAt: retiredAt, CheckedAt: now.Add(-31 * time.Minute).Format(time.RFC3339Nano), Success: true, Successes: 10}}
	first, err := runner.RecoverRetiredNodes(context.Background(), state, now, time.Second, nil)
	if err != nil || first.Restored != 0 || state[id].Successes != 1 {
		t.Fatalf("legacy count authorized recovery: %+v %+v %v", first, state, err)
	}
	second, err := runner.RecoverRetiredNodes(context.Background(), state, now.Add(time.Minute), time.Second, nil)
	if err != nil || second.Checked != 0 {
		t.Fatalf("short interval bypassed: %+v %v", second, err)
	}
	third, err := runner.RecoverRetiredNodes(context.Background(), state, now.Add(31*time.Minute), time.Second, nil)
	if err != nil || third.Restored != 1 {
		t.Fatalf("fresh residential node with old error/hosting label was missed: %+v %v", third, err)
	}
}

func TestRecoveryResetsOnChangedExitOrClassificationDispute(t *testing.T) {
	for _, disputed := range []bool{false, true} {
		t.Run(fmt.Sprint(disputed), func(t *testing.T) {
			calls := 0
			runner, _, now := recoveryFixture(t, 1, func(context.Context, *http.Client) (string, error) {
				calls++
				if calls == 1 {
					return "8.8.8.8", nil
				}
				if disputed {
					return "", ErrResidentialDisputed
				}
				return "1.1.1.1", nil
			})
			state := map[string]RecoveryState{}
			if _, err := runner.RecoverRetiredNodes(context.Background(), state, now, 0, nil); err != nil {
				t.Fatal(err)
			}
			result, err := runner.RecoverRetiredNodes(context.Background(), state, now.Add(31*time.Minute), 0, nil)
			if err != nil || result.Restored != 0 || len(runner.store.RetiredRows()) != 1 {
				t.Fatalf("unsafe recovery: %+v %v", result, err)
			}
			for _, v := range state {
				if disputed && (v.Classification != "disputed" || v.Successes != 0) {
					t.Fatalf("dispute lost: %+v", v)
				}
			}
		})
	}
}

func TestRecoveryBudgetRotatesThroughCandidates(t *testing.T) {
	runner, _, now := recoveryFixture(t, 3, func(context.Context, *http.Client) (string, error) { return "", nil })
	runner.recoveryLimit = 1
	state := map[string]RecoveryState{}
	for i := 0; i < 3; i++ {
		result, err := runner.RecoverRetiredNodes(context.Background(), state, now.Add(time.Duration(i)*31*time.Minute), 0, nil)
		if err != nil || result.Checked != 1 || result.Restored != 0 {
			t.Fatalf("budget exceeded: %+v %v", result, err)
		}
	}
	if len(state) != 3 {
		t.Fatalf("recovery queue starved candidates: %d", len(state))
	}
}

func TestRecoveryWithoutKeyDoesNotRestore(t *testing.T) {
	runner, _, now := recoveryFixture(t, 1, nil)
	result, err := runner.RecoverRetiredNodes(context.Background(), map[string]RecoveryState{}, now, 0, nil)
	if err != nil || result.ClassificationEnabled || result.Checked != 0 || len(runner.store.RetiredRows()) != 1 {
		t.Fatalf("unverified recovery: %+v %v", result, err)
	}
}
