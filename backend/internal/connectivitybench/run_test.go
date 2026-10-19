package connectivitybench

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func TestNativeMappingBudgetsAndLaneIsolation(t *testing.T) {
	makeCandidate := func(id string) Candidate {
		return Candidate{Identity: id, Node: domain.Node{Type: "http", Server: id + ".example", Port: "8080"}}
	}
	sample := Sample{References: []Candidate{makeCandidate("ref-a"), makeCandidate("ref-b")}}
	for j := range 40 {
		sample.Targets = append(sample.Targets, makeCandidate(fmt.Sprintf("normal-%d", j)))
	}
	for j := range 16 {
		sample.Residential = append(sample.Residential, makeCandidate(fmt.Sprintf("residential-%d", j)))
	}
	instances := Instances{Normal: []transport.MihomoConfig{{Controller: "http://cb-n:9090", ProxyURL: "http://cb-n:7890"}}, Residential: []transport.MihomoConfig{{Controller: "http://cb-r:9090", ProxyURL: "http://cb-r:7890"}}}
	var active, peak, residentialActive atomic.Int32
	p := Policy{Timeout: time.Second, RetryDelay: time.Millisecond, Probe: func(ctx context.Context, c *http.Client) (bool, error) {
		req, _ := http.NewRequest("GET", "https://github.com/", nil)
		u, err := c.Transport.(*http.Transport).Proxy(req)
		if err != nil {
			return false, err
		}
		if strings.HasPrefix(u.Hostname(), "normal-") {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			time.Sleep(time.Millisecond)
		} else if strings.HasPrefix(u.Hostname(), "residential-") {
			residentialActive.Add(1)
		} else {
			t.Error("unexpected reference on passing node")
		}
		return true, nil
	}}
	report, err := Run(context.Background(), sample, instances, Options{Concurrency: 3, BatchSize: 8, Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	if report.Normal.Count != 40 || report.Residential.Count != 16 || peak.Load() > 3 || residentialActive.Load() != 16 {
		t.Fatalf("bad counts or budgets %+v peak=%d", report, peak.Load())
	}
	seen := map[string]bool{}
	for _, r := range report.Results {
		if seen[r.ID] || !r.Reachable {
			t.Fatal("mapping lost", r)
		}
		seen[r.ID] = true
	}
	for _, c := range append(sample.Targets, sample.Residential...) {
		if !seen[Token(c.Identity)] {
			t.Fatal("identity missing")
		}
	}
}

func TestControlBudgetCancellationAndIdempotentRelease(t *testing.T) {
	refs := []Candidate{{Identity: "one"}, {Identity: "two"}}
	budget := newControlBudget(refs, 2, 1)
	ctx := context.Background()
	one, err := budget.acquire(ctx, Token("one"))
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if _, err = budget.acquire(waiting, Token("one")); err == nil {
		t.Fatal("reference limit ignored")
	}
	two, err := budget.acquire(ctx, Token("two"))
	if err != nil {
		t.Fatal("one blocked unrelated reference", err)
	}
	waiting2, cancel2 := context.WithTimeout(ctx, time.Millisecond)
	defer cancel2()
	if _, err = budget.acquire(waiting2, "unknown"); err == nil {
		t.Fatal("global control limit ignored")
	}
	one()
	one()
	two()
	again, err := budget.acquire(ctx, Token("one"))
	if err != nil {
		t.Fatal(err)
	}
	again()
}
func TestReferencesSpreadAcrossNodesAndExcludeDuplicateEndpoints(t *testing.T) {
	var refs []Candidate
	for i := range 32 {
		refs = append(refs, Candidate{Identity: fmt.Sprint(i), Node: domain.Node{Server: fmt.Sprintf("ref-%d.example", i)}})
	}
	a := referenceSet([]Candidate{{Identity: "target-a"}}, refs)
	b := referenceSet([]Candidate{{Identity: "target-b"}}, refs)
	if len(a) != 8 || len(b) != 8 {
		t.Fatal("reference shard size")
	}
	if a[0].Identity == b[0].Identity {
		t.Fatal("all waves use one reference shard")
	}
	for i := range refs {
		refs[i].Node.Server = "same.example"
	}
	if got := referenceSet([]Candidate{{Identity: "target"}}, refs); len(got) != 1 {
		t.Fatal("duplicate endpoint controls", len(got))
	}
}

func TestDeadWaveUsesFreshControlsWithoutDuplicateBurst(t *testing.T) {
	makeCandidate := func(id string) Candidate {
		return Candidate{Identity: id, Node: domain.Node{Type: "http", Server: id + ".example", Port: "8080"}}
	}
	sample := Sample{}
	for i := range 100 {
		sample.Targets = append(sample.Targets, makeCandidate(fmt.Sprintf("target-%d", i)))
	}
	for i := range 16 {
		sample.References = append(sample.References, makeCandidate(fmt.Sprintf("ref-%d", i)))
	}
	instances := Instances{Normal: []transport.MihomoConfig{{Controller: "http://cb-n:9090", ProxyURL: "http://cb-n:7890"}}, Residential: []transport.MihomoConfig{{Controller: "http://cb-r:9090", ProxyURL: "http://cb-r:7890"}}}
	var targets, controls atomic.Int32
	policy := Policy{Timeout: time.Second, RetryDelay: time.Millisecond, Probe: func(ctx context.Context, c *http.Client) (bool, error) {
		transport := c.Transport.(*http.Transport)
		if transport.Proxy != nil {
			req, _ := http.NewRequest("GET", "https://github.com/", nil)
			u, _ := transport.Proxy(req)
			if strings.HasPrefix(u.Hostname(), "target-") {
				targets.Add(1)
				return false, nil
			}
		}
		controls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return true, nil
	}}
	r, err := Run(context.Background(), sample, instances, Options{Concurrency: 64, BatchSize: 128, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if r.Normal.ConfirmedDead != 100 || targets.Load() != 200 {
		t.Fatal("target evidence weakened", r.Normal, targets.Load())
	}
	if controls.Load() >= 200 || r.Normal.ControlJoins == 0 || int64(controls.Load()) != r.Normal.ControlRequests {
		t.Fatal("duplicate control burst", r.Normal)
	}
	for _, v := range r.Results {
		if len(v.Trace) != 4 || v.Trace[2].Outcome != "http-200" || v.Trace[3].Outcome != "http-200" {
			t.Fatal("missing fresh controls", v)
		}
	}
}
