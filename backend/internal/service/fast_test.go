package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func managedFixture(t *testing.T, status int) *fixture {
	t.Helper()
	f := setup(t)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/delay") {
			if r.URL.Query().Get("expected") != "204" || r.URL.Query().Get("url") != transport.DelayTarget {
				t.Errorf("unsafe delay target: %s", r.URL)
			}
			w.WriteHeader(status)
			if status == 200 {
				_, _ = w.Write([]byte(`{"delay":1}`))
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"extra": map[string]any{transport.DelayTarget: map[string]any{"alive": status == 200, "history": []map[string]any{{"time": time.Now(), "delay": 1}}}}})
	}))
	t.Cleanup(controller.Close)
	m := transport.NewManager(transport.MihomoConfig{Controller: controller.URL, ProxyURL: "http://localhost:7890"})
	entries, err := f.repo.kernelEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Sync(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(m.Handler("test"))
	t.Cleanup(api.Close)
	f.runner.Config.Manager = &transport.ManagerClient{URL: api.URL, Token: "test"}
	return f
}

func TestManagedFastSuccessProvesConnectivityAndFailuresRequireReview(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		retired, cleanup, success, strict bool
	}{
		{"retired failure", true, false, false, false}, {"active failure", false, false, false, true},
		{"cleanup failure", false, true, false, true}, {"retired 204", true, false, true, false}, {"active 204", false, false, true, false}, {"cleanup 204", false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := 503
			if tc.success {
				status = 200
			}
			f := managedFixture(t, status)
			ctx := context.Background()
			p := Payload{Identity: f.id, Session: "fast-test"}
			if tc.retired {
				p = f.retirement(t)
				p.Session = "fast-test"
			}
			if tc.cleanup {
				p.Action = "cleanup"
				if e := f.repo.Pool.QueryRow(ctx, `SELECT incarnation,md5(node::text) FROM node_explorer_node_registry WHERE identity=$1`, f.id).Scan(&p.Incarnation, &p.Reason); e != nil {
					t.Fatal(e)
				}
			}
			if e := f.repo.Schedule(ctx, KindFast, "fast-test", p, 1, time.Now()); e != nil {
				t.Fatal(e)
			}
			j := f.claim(t, KindFast)
			if e := f.runner.fast(ctx, j, false); e != nil {
				t.Fatal(e)
			}
			// Redelivery must not create duplicate dependencies or overwrite evidence.
			if e := f.runner.fast(ctx, j, false); !errors.Is(e, ErrLeaseLost) {
				t.Fatalf("duplicate result accepted: %v", e)
			}
			var strict, proofs, available int
			if e := f.repo.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM node_explorer_work WHERE kind=$1),(SELECT count(*) FROM node_explorer_connectivity),(SELECT count(*) FROM node_explorer_node_registry WHERE last_available_at IS NOT NULL)`, KindLive).Scan(&strict, &proofs, &available); e != nil {
				t.Fatal(e)
			}
			expected := 0
			if tc.strict {
				expected = 1
			}
			successProof := 0
			if tc.success {
				successProof = 1
			}
			if strict != expected || proofs != successProof || available != successProof {
				t.Fatalf("strict=%d proofs=%d availability=%d", strict, proofs, available)
			}
			if tc.success {
				var proof domain.ConnectivityState
				ready, e := f.repo.Probe(ctx, p.Session, KindLive, &proof)
				if e != nil || !ready || !proof.Reachable || proof.ProbeTarget != transport.DelayTarget {
					t.Fatalf("missing 204 session proof: %+v %v", proof, e)
				}
				var retired int
				if e := f.repo.Pool.QueryRow(ctx, `SELECT count(*) FROM node_explorer_retired_nodes`).Scan(&retired); e != nil || (retired == 1) != tc.retired {
					t.Fatalf("204 changed retirement: %d %v", retired, e)
				}
				expired, e := f.runner.assessmentExpired(ctx, p.Session, time.Time{})
				if e != nil || expired {
					t.Fatalf("missing execution capacity timestamp: %v %v", expired, e)
				}
			}
			var raw []byte
			if e := f.repo.Pool.QueryRow(ctx, `SELECT payload FROM node_explorer_fast_results WHERE identity=$1`, f.id).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var result transport.DelayResult
			if e := json.Unmarshal(raw, &result); e != nil || result.Success != tc.success {
				t.Fatalf("result %s: %v", raw, e)
			}
		})
	}
}

func TestManagedQueueTimeDoesNotExpireAssessment(t *testing.T) {
	f := managedFixture(t, 200)
	ctx := context.Background()
	expired, e := f.runner.assessmentExpired(ctx, "queued", time.Now().Add(-24*time.Hour))
	if e != nil || expired {
		t.Fatalf("queue counted as execution: %v %v", expired, e)
	}
}

func TestManagedProofFenceRejectsRegistryReplacement(t *testing.T) {
	f := managedFixture(t, 200)
	ctx := context.Background()
	bounded, release, paths, err := f.runner.proofFence(ctx, Payload{Identity: f.id})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	f.exec(t, `UPDATE node_explorer_node_registry SET incarnation='replacement' WHERE identity=$1`, f.id)
	err = f.repo.FinishDependencies(bounded, func(tx pgx.Tx) error { return validatePaths(bounded, tx, paths) })
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("replacement accepted old lease: %v", err)
	}
}

func TestManagedNormalAdmissionIncludesWaitingAssessments(t *testing.T) {
	f := setup(t)
	f.repo.NormalRecoveryConcurrency = 128
	ctx := context.Background()
	for i := 0; i < 129; i++ {
		key := fmt.Sprintf("bounded-%d", i)
		if err := f.repo.Schedule(ctx, KindNormalRecovery, key, Payload{Identity: f.id}, 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		var ref string
		if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND key=$2`, KindNormalRecovery, key).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		j, err := f.repo.Claim(ctx, KindNormalRecovery, ref, false)
		if err != nil {
			t.Fatal(err)
		}
		if (i < 128) != (j != nil) {
			t.Fatalf("admission %d: %+v", i, j)
		}
	}
}

func TestManaged204RecoveryStillWaitsForRealExit(t *testing.T) {
	f := managedFixture(t, 200)
	ctx := context.Background()
	j := queueNormal(t, f)
	if err := f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("assessment skipped checks: %v", err)
	}
	var ref string
	if err := f.repo.Pool.QueryRow(ctx, `SELECT id::text||':'||generation::text FROM node_explorer_work WHERE kind=$1 AND state='queued'`, KindFast).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	quick, err := f.repo.Claim(ctx, KindFast, ref, false)
	if err != nil || quick == nil {
		t.Fatalf("fast admission: %v", err)
	}
	if err := f.runner.fast(ctx, *quick, false); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.recovery(ctx, j); !isWaiting(err) {
		t.Fatalf("204 restored without exit: %v", err)
	}
	var exits, lifecycle int
	if err := f.repo.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM node_explorer_work WHERE kind=$1),(SELECT count(*) FROM node_explorer_work WHERE kind=$2)`, KindExit, KindLifecycle).Scan(&exits, &lifecycle); err != nil || exits != 1 || lifecycle != 0 {
		t.Fatalf("missing exit barrier: exits=%d lifecycle=%d err=%v", exits, lifecycle, err)
	}
	f.exec(t, `UPDATE node_explorer_work SET payload=payload||jsonb_build_object('capacityAt',now()-interval '4 minutes') WHERE kind=$1`, KindFast)
	expired, err := f.runner.assessmentExpired(ctx, "recovery:"+j.Ref(), time.Time{})
	if err != nil || !expired {
		t.Fatalf("fast capacity deadline ignored: %v %v", expired, err)
	}
}
