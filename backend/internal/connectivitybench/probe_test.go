package connectivitybench

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
)

func TestProbeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		target      []bool
		direct, ref bool
		missing     bool
		want        Verdict
	}{
		{name: "success", target: []bool{true}, want: Verdict{Reachable: true, Attempts: 1}},
		{name: "second success", target: []bool{false, true}, want: Verdict{Reachable: true, Attempts: 2}},
		{name: "dead with controls", target: []bool{false, false}, direct: true, ref: true, want: Verdict{ConfirmedDead: true, Attempts: 2, Error: "two-failures"}},
		{name: "direct fails", target: []bool{false, false}, ref: true, want: Verdict{Attempts: 2, Error: "insufficient-controls"}},
		{name: "reference fails", target: []bool{false, false}, direct: true, want: Verdict{Attempts: 2, Error: "insufficient-controls"}},
		{name: "no reference", target: []bool{false, false}, direct: true, missing: true, want: Verdict{Attempts: 2, Error: "insufficient-controls"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, reference := &http.Client{}, &http.Client{}
			attempts := 0
			controls := 0
			p := Policy{Timeout: time.Second, RetryDelay: time.Millisecond, Probe: func(ctx context.Context, c *http.Client) (bool, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("attempt without deadline")
				}
				switch c {
				case target:
					ok := tc.target[attempts]
					attempts++
					return ok, nil
				case reference:
					controls++
					return tc.ref, nil
				default:
					controls++
					return tc.direct, nil
				}
			}}
			ref := &transport.Outbound{Client: reference}
			if tc.missing {
				ref = nil
			}
			got := p.Evaluate(context.Background(), &transport.Outbound{Client: target}, ref)
			tc.want.Trace = got.Trace
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			if tc.want.Reachable && controls != 0 {
				t.Fatal("unnecessary controls")
			}
		})
	}
}
func TestProbeCancelTimeoutAndInvalid(t *testing.T) {
	var calls atomic.Int32
	p := Policy{Timeout: 5 * time.Millisecond, RetryDelay: time.Millisecond, Probe: func(ctx context.Context, c *http.Client) (bool, error) {
		calls.Add(1)
		<-ctx.Done()
		return false, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := p.Evaluate(ctx, &transport.Outbound{Client: &http.Client{}}, nil); got.Error != "cancelled" || got.ConfirmedDead {
		t.Fatal(got)
	}
	if got := p.Evaluate(context.Background(), &transport.Outbound{Error: errors.New("invalid")}, nil); got.Error != "unsupported" || calls.Load() != 0 {
		t.Fatal(got)
	}
	got := p.Evaluate(context.Background(), &transport.Outbound{Client: &http.Client{}}, nil)
	if got.Attempts != 2 || got.ConfirmedDead || calls.Load() != 2 {
		t.Fatal(got)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Millisecond)
	defer cancel()
	got = p.Evaluate(ctx, &transport.Outbound{Client: &http.Client{}}, nil)
	if got.Error != "cancelled" || got.ConfirmedDead {
		t.Fatal(got)
	}
}

func TestReferenceFailoverKeepsEvidenceAndBound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		host        bool
		passing     int
		rateLimited bool
		cancelFirst bool
		wantCalls   int
		wantDead    bool
	}{
		{name: "second reference succeeds", host: true, passing: 1, wantCalls: 2, wantDead: true},
		{name: "three-reference limit", host: true, passing: 3, wantCalls: 3},
		{name: "host failure prevents confirmation", passing: 0},
		{name: "rate limit remains visible", host: true, passing: 1, rateLimited: true, wantCalls: 2, wantDead: true},
		{name: "cancellation prevents fallback", host: true, passing: 1, cancelFirst: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			target := &http.Client{}
			var refs []Reference
			for i := range 4 {
				refs = append(refs, Reference{ID: string(rune('a' + i)), Path: &transport.Outbound{Client: &http.Client{}}})
			}
			targetCalls, controlCalls := 0, 0
			p := Policy{Timeout: time.Second, RetryDelay: time.Millisecond,
				Probe: func(ctx context.Context, c *http.Client) (bool, error) {
					if c != target {
						t.Fatal("control bypassed bounded callback")
					}
					targetCalls++
					return false, nil
				},
				ControlProbe: func(ctx context.Context, c *http.Client, phase, id string) (bool, error) {
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("control without timeout")
					}
					if phase == "host-control" {
						return tc.host, nil
					}
					index := controlCalls
					controlCalls++
					if c != refs[index].Path.Client || id != refs[index].ID {
						t.Fatal("reference identity changed")
					}
					if tc.cancelFirst {
						cancel()
					}
					if tc.rateLimited && index == 0 {
						return false, httpStatusError(429)
					}
					return index == tc.passing, nil
				},
			}
			v := p.EvaluateReferences(ctx, &transport.Outbound{Client: target}, refs)
			if targetCalls != 2 || v.Attempts != 2 || controlCalls != tc.wantCalls || v.ConfirmedDead != tc.wantDead {
				t.Fatalf("wrong evidence/bound: %+v target=%d controls=%d", v, targetCalls, controlCalls)
			}
			if len(v.Trace) != 3+tc.wantCalls {
				t.Fatal("attempt evidence missing", v)
			}
			for i, attempt := range v.Trace[3:] {
				if attempt.ReferenceID != refs[i].ID {
					t.Fatal("missing reference identity", v)
				}
			}
			if tc.rateLimited && v.Trace[3].Outcome != "http-429" {
				t.Fatal("rate limit hidden", v)
			}
			if tc.cancelFirst && v.Error != "cancelled" {
				t.Fatal("cancellation hidden", v)
			}
			if tc.wantDead && v.ReferenceID != refs[tc.passing].ID {
				t.Fatal("successful control misattributed", v)
			}
		})
	}
}
func TestSamplingDeterministicAndProportional(t *testing.T) {
	var in []Candidate
	for i := range 100 {
		proto := "ss"
		if i < 25 {
			proto = "trojan"
		}
		in = append(in, Candidate{Identity: string(rune(i + 1)), Node: domain.Node{Type: proto}})
	}
	a := Stratified(in, 20)
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
	b := Stratified(in, 20)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("row order changed sample")
	}
	trojan := 0
	for _, c := range a {
		if c.Node.Type == "trojan" {
			trojan++
		}
	}
	if len(a) != 20 || trojan != 5 {
		t.Fatal("quota", len(a), trojan)
	}
}
func TestProductionEndpointsRejected(t *testing.T) {
	c := func(host string) transport.MihomoConfig {
		return transport.MihomoConfig{Controller: "http://" + host + ":9090", ProxyURL: "http://" + host + ":7890"}
	}
	good := Instances{Normal: []transport.MihomoConfig{c("cb-normal-01")}, Residential: []transport.MihomoConfig{c("cb-residential-01")}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	good.Normal[0] = c("connectivity-normal-proxy")
	if err := good.Validate(); err == nil {
		t.Fatal("production controller accepted")
	}
	good.Normal[0] = good.Residential[0]
	if err := good.Validate(); err == nil {
		t.Fatal("lanes share kernel")
	}
}
