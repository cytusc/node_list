package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/sqlite"
	"github.com/cytusc/node_list/backend/internal/transport"
)

type markedTransport string

func (markedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled in test")
}

type testEngine struct{}

func (*testEngine) Name() string         { return "test" }
func (*testEngine) Supports(string) bool { return true }
func (*testEngine) Reserve(_ context.Context, nodes []domain.Node) (map[int]*transport.Outbound, func(), error) {
	out := make(map[int]*transport.Outbound)
	for i, node := range nodes {
		out[i] = &transport.Outbound{Client: &http.Client{Transport: markedTransport(node.Name)}}
	}
	return out, func() {}, nil
}

func TestPatrolSynchronizesExternalImportsAndRequiresControls(t *testing.T) {
	for _, tc := range []struct {
		name          string
		direct, proxy bool
		retired       int
	}{{"healthy controls", true, true, 1}, {"host outage", false, true, 0}, {"no healthy proxy", true, false, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite")
			reader, err := sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer, err := sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			st := store.New(reader, nil)
			if err := st.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			nodes := []domain.Node{}
			for i, name := range []string{"good", "bad"} {
				nodes = append(nodes, domain.Node{Name: name, Type: "ss", Server: fmt.Sprintf("192.0.2.%d", i+1), Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": fmt.Sprintf("192.0.2.%d", i+1), "port": 443}})
			}
			raw, _ := json.Marshal(domain.Snapshot{Nodes: nodes})
			if err := writer.Insert(ctx, "external", raw, "", 2); err != nil {
				t.Fatal(err)
			}
			runner := NewPatrol(st, transport.NewRouter(&testEngine{}), PatrolConfig{Concurrency: 2, RetryDelay: time.Nanosecond})
			var mu sync.Mutex
			calls := map[string]int{}
			runner.probe = func(_ context.Context, c *http.Client) (bool, error) {
				marker, proxy := c.Transport.(markedTransport)
				if !proxy {
					return tc.direct, nil
				}
				mu.Lock()
				calls[string(marker)]++
				mu.Unlock()
				if marker == "good" && tc.proxy {
					return true, nil
				}
				return false, errors.New("connection refused")
			}
			summary, err := runner.RunOnce(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Total != 2 || summary.Retired != tc.retired || calls["bad"] != 2 {
				t.Fatalf("summary=%+v attempts=%v", summary, calls)
			}
			if tc.retired == 0 && len(st.RetiredRows()) != 0 {
				t.Fatal("outage retired nodes without controls")
			}
		})
	}
}

func TestRecoverySynchronizesRetirementsAndRequiresTwoSuccesses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	reader, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	writer, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	st := store.New(reader, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	nodes := []domain.Node{}
	for i, name := range []string{"home", "manual", "datacenter"} {
		nodes = append(nodes, domain.Node{Name: name, Type: "ss", Server: fmt.Sprintf("192.0.2.%d", i+1), Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": fmt.Sprintf("192.0.2.%d", i+1), "port": 443}})
	}
	raw, _ := json.Marshal(domain.Snapshot{Nodes: nodes})
	if err := writer.Insert(ctx, "external", raw, "", 3); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	retiredAt := now.Add(-time.Hour).Format(time.RFC3339Nano)
	for i, node := range nodes {
		reason := PatrolRetireReason
		if i == 1 {
			reason = "manual"
		}
		attributes := []string{"住宅IP"}
		if i == 2 {
			attributes = []string{"机房IP"}
		}
		raw, _ := json.Marshal(domain.IPPureResult{Status: "success", Attributes: attributes})
		if err := writer.SaveIPPure(ctx, 1, i, raw, domain.AggregateIdentity(node)); err != nil {
			t.Fatal(err)
		}
		if err := writer.RetireNodes(ctx, []string{domain.AggregateIdentity(node)}, retiredAt, reason); err != nil {
			t.Fatal(err)
		}
	}
	runner := NewPatrol(st, transport.NewRouter(&testEngine{}), PatrolConfig{ResidentialVerifier: func(_ context.Context, client *http.Client) (string, error) {
		if string(client.Transport.(markedTransport)) == "home" {
			return "8.8.8.8", nil
		}
		return "", nil
	}})
	runner.probe = func(context.Context, *http.Client) (bool, error) { return true, nil }
	state := map[string]RecoveryState{}
	first, err := runner.RecoverRetiredNodes(ctx, state, now, 30*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Checked != 2 || first.Restored != 0 {
		t.Fatalf("first recovery=%+v", first)
	}
	second, err := runner.RecoverRetiredNodes(ctx, state, now.Add(31*time.Minute), 30*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Checked != 2 || second.Restored != 1 {
		t.Fatalf("second recovery=%+v", second)
	}
	if len(st.RetiredRows()) != 2 {
		t.Fatal("manual or datacenter retirement was incorrectly restored")
	}
}
