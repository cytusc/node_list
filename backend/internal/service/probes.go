package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cytusc/node_list/backend/internal/collector"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/ipcheck"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

func (r *Runner) router(high bool) *transport.Router {
	if high {
		return r.Config.HighRouter
	}
	return r.Config.NormalRouter
}

func (r *Runner) reserve(ctx context.Context, nodes []domain.Node, high bool) (map[string]*transport.Outbound, func(), error) {
	router := r.router(high)
	out := map[string]*transport.Outbound{}
	releases := []func(){}
	release := func() {
		for _, f := range releases {
			f()
		}
	}
	for engine, indexes := range router.Group(nodes) {
		group := make([]domain.Node, len(indexes))
		for i, index := range indexes {
			group[i] = nodes[index]
		}
		paths, done, err := engine.Reserve(ctx, group)
		if err != nil {
			release()
			return nil, nil, err
		}
		releases = append(releases, done)
		for i, path := range paths {
			out[domain.AggregateIdentity(group[i])] = path
		}
	}
	return out, release, nil
}

func (r *Runner) reference(ctx context.Context, exclude string) (*domain.Node, error) {
	var raw []byte
	err := r.Repo.Pool.QueryRow(ctx, `SELECT n.node FROM node_explorer_node_registry n JOIN node_explorer_connectivity c USING(identity)
	WHERE n.available AND n.identity<>$1 AND c.payload->>'reachable'='true' AND c.checked_at>now()-interval '30 minutes'
	AND NOT EXISTS(SELECT 1 FROM node_explorer_retired_nodes t WHERE t.identity=n.identity)
	AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id) ORDER BY c.checked_at DESC LIMIT 1`, exclude).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n := &domain.Node{}
	err = json.Unmarshal(raw, n)
	return n, err
}

func reachable(ctx context.Context, client *http.Client, timeout time.Duration) bool {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client.CloseIdleConnections()
	ok, err := transport.ProbeHTTP(probeCtx, client)
	return err == nil && ok
}

func (r *Runner) live(ctx context.Context, j Job, high bool) error {
	t, err := r.Repo.Target(ctx, j.Payload.Identity)
	if err != nil {
		return err
	}
	if !matchesRetirement(t, j.Payload) {
		return r.Repo.Finish(ctx, j, nil)
	}
	nodes := []domain.Node{t.Node}
	reference, err := r.reference(ctx, t.Identity)
	if err != nil {
		return err
	}
	if reference != nil {
		nodes = append(nodes, *reference)
	}
	paths, release, err := r.reserve(ctx, nodes, high)
	if err != nil {
		return err
	}
	defer release()
	v := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	path := paths[t.Identity]
	if path == nil || path.Error != nil {
		v.Error = "连接配置不支持"
	} else {
		v.Reachable = reachable(ctx, path.Client, r.Config.Timeout)
		if !v.Reachable {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.Config.RetryDelay):
			}
			v.Reachable = reachable(ctx, path.Client, r.Config.Timeout)
			if !v.Reachable {
				direct := &http.Client{Transport: &http.Transport{Proxy: nil}}
				defer direct.CloseIdleConnections()
				if reference != nil {
					control := paths[domain.AggregateIdentity(*reference)]
					v.ConfirmedDead = control != nil && control.Error == nil && reachable(ctx, direct, r.Config.Timeout) && reachable(ctx, control.Client, r.Config.Timeout)
				}
				v.Error = "两次连通性检测失败"
				if !v.ConfirmedDead {
					v.Error = "连通性未确认，对照不足"
				}
			}
		}
	}
	v.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		ok, err := guard(ctx, tx, j.Payload)
		if err != nil || !ok {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_connectivity(identity,checked_at,payload) VALUES($1,$2,$3)
		ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload
		WHERE node_explorer_connectivity.checked_at<=EXCLUDED.checked_at`, t.Identity, v.CheckedAt, b); err != nil {
			return err
		}
		if err = writeProbe(ctx, tx, j.Payload.Session, KindLive, t.Identity, v); err != nil {
			return err
		}
		if v.ConfirmedDead && t.Retired == nil {
			p := j.Payload
			p.Action = "retire"
			p.Session = j.Ref()
			p.CheckedAt = v.CheckedAt
			return once(ctx, tx, KindLifecycle, "retire:"+j.Ref(), p, j.Priority)
		}
		return nil
	})
}

// StrongExit requires independent HTTPS echoes and rejects host egress. It is
// shared by first and final recovery checks; normal monitoring uses fallbacks.
func StrongExit(ctx context.Context, tunnel, host *http.Client) (string, error) {
	get := func(client *http.Client) (string, error) {
		observed := ""
		for _, endpoint := range []string{"https://api.ipify.org", "https://checkip.amazonaws.com"} {
			req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if e != nil {
				return "", e
			}
			client.CloseIdleConnections()
			resp, e := client.Do(req)
			if e != nil {
				return "", fmt.Errorf("exit echo unavailable")
			}
			b, re := io.ReadAll(io.LimitReader(resp.Body, 128))
			resp.Body.Close()
			ip := net.ParseIP(strings.TrimSpace(string(b)))
			if re != nil || resp.StatusCode != 200 || ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
				return "", fmt.Errorf("invalid public IPv4 echo")
			}
			if observed != "" && observed != ip.String() {
				return "", fmt.Errorf("exit echoes disagree")
			}
			observed = ip.String()
		}
		return observed, nil
	}
	ip, err := get(tunnel)
	if err != nil {
		return "", err
	}
	direct, err := get(host)
	if err != nil || direct == ip {
		return "", fmt.Errorf("exit cannot be distinguished from host egress")
	}
	return ip, nil
}

func (r *Runner) exit(ctx context.Context, j Job, high bool) error {
	t, err := r.Repo.Target(ctx, j.Payload.Identity)
	if err != nil {
		return err
	}
	if !matchesRetirement(t, j.Payload) {
		return r.Repo.Finish(ctx, j, nil)
	}
	paths, release, err := r.reserve(ctx, []domain.Node{t.Node}, high)
	if err != nil {
		return err
	}
	defer release()
	v := domain.ExitState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	path := paths[t.Identity]
	if path == nil || path.Error != nil {
		v.Error = "连接配置不支持"
	} else if j.Payload.Session != "" {
		probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		direct := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
		defer direct.CloseIdleConnections()
		ip, e := StrongExit(probeCtx, path.Client, direct)
		if e != nil {
			v.Error = "真实出口核验失败"
		} else {
			v.IP, v.Source, v.Verified, v.AddressFamily = ip, "ipify+aws", true, 4
		}
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		defer cancel()
		o, e := collector.ResolveExitIPInfo(probeCtx, path.Client, 5*time.Second, nil, nil)
		if e != nil || o == nil {
			v.Error = "出口探测失败"
		} else {
			v.IP, v.Source, v.Country, v.AddressFamily = o.IP, o.Provider, o.Country, o.AddressFamily
		}
	}
	v.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if j.Payload.ExpectedIP != "" && v.IP != j.Payload.ExpectedIP {
		v.Error = "复核期间出口变化"
	}
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		ok, e := guard(ctx, tx, j.Payload)
		if e != nil || !ok {
			return e
		}
		// Retain the last observed address for diagnostics on failure, while
		// clearing verification so it cannot authorize recovery or preference.
		if v.IP == "" {
			_ = tx.QueryRow(ctx, `SELECT ip FROM node_explorer_exits WHERE identity=$1`, t.Identity).Scan(&v.IP)
		}
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		e = tx.QueryRow(ctx, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,$2,$3,$4)
		ON CONFLICT(identity) DO UPDATE SET ip=EXCLUDED.ip,checked_at=EXCLUDED.checked_at,payload=EXCLUDED.payload,
		version=node_explorer_exits.version+CASE WHEN node_explorer_exits.ip<>EXCLUDED.ip THEN 1 ELSE 0 END
		WHERE node_explorer_exits.checked_at<=EXCLUDED.checked_at RETURNING version`, t.Identity, v.IP, v.CheckedAt, b).Scan(&v.Version)
		if e != nil && e != pgx.ErrNoRows {
			return e
		}
		if e = writeProbe(ctx, tx, j.Payload.Session, KindExit, t.Identity, v); e != nil {
			return e
		}
		if v.Error == "" && v.AddressFamily == 4 && !strings.HasPrefix(j.Payload.Session, "recovery:") {
			if e = scheduleIP(ctx, tx, v.IP, t.Identity, j.Priority); e != nil {
				return e
			}
		}
		if v.IP != "" && v.Error == "" {
			_, e = tx.Exec(ctx, `UPDATE node_explorer_node_registry SET historical_residential=true WHERE identity=$1 AND
		EXISTS(SELECT 1 FROM node_explorer_ip_attributes a WHERE a.ip=$2 AND a.payload->'classification'->>'state'='residential')`, t.Identity, v.IP)
		}
		return e
	})
}

func (r *Runner) attributes(ctx context.Context, j Job) error {
	used, err := r.Repo.IPUsed(ctx, j.Payload.IP)
	if err != nil {
		return err
	}
	if !used {
		return r.Repo.Finish(ctx, j, nil)
	}
	// Classification does not wait for optional risk metrics.
	evidence := r.Classification.Lookup(ctx, j.Payload.IP)
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if err := mergeAttributes(ctx, tx, j.Payload.IP, evidence, nil); err != nil {
			return err
		}
		delay := 24 * time.Hour
		valid := false
		for _, e := range evidence {
			if e.QueryStatus == "success" && (e.UsageType != "" || e.ASNType != "" || e.CompanyType != "") {
				valid = true
			}
		}
		if !valid {
			delay = 30 * time.Minute
			if j.Priority > 0 {
				delay = r.Config.ResidentialInterval
			}
		}
		return recur(ctx, tx, j, delay)
	})
}

func (r *Runner) risk(ctx context.Context, j Job, high bool) error {
	used, err := r.Repo.IPUsed(ctx, j.Payload.IP)
	if err != nil {
		return err
	}
	if !used {
		return r.Repo.Finish(ctx, j, nil)
	}
	// Risk traffic uses the normal IP lane so slow remote APIs cannot occupy
	// the reserved residential exit-probe instance.
	reference, err := r.reference(ctx, "")
	if err != nil {
		return err
	}
	if reference == nil {
		t, e := r.Repo.Target(ctx, j.Payload.Identity)
		if e != nil {
			return e
		}
		if t == nil {
			return waiting{30 * time.Second}
		}
		reference = &t.Node
	}
	paths, release, err := r.reserve(ctx, []domain.Node{*reference}, false)
	if err != nil {
		return err
	}
	defer release()
	path := paths[domain.AggregateIdentity(*reference)]
	if path == nil || path.Error != nil {
		return waiting{30 * time.Second}
	}
	verdict, queryErr := collector.QueryIPPure(ctx, collector.NewIPPureClient(path.Client, r.Config.APIOrigin, 10*time.Second), j.Payload.IP, r.asnCache)
	v := &domain.IPPureResult{IP: j.Payload.IP, Status: "error", FailureStage: "ippure-query", Error: "风险查询失败"}
	e := ipcheck.Evidence{Provider: "ippure", IP: j.Payload.IP, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Outcome: "unknown", QueryStatus: "request-failed"}
	if queryErr == nil {
		v.Status = "success"
		v.FailureStage = ""
		v.Error = ""
		v.ASN, v.Organization, v.Country, v.Source = verdict.ASN, verdict.Organization, verdict.Country, verdict.Source
		v.Attributes, v.Score, v.Label = verdict.Attributes, verdict.Score, verdict.Label
		v.HumanPercent, v.BotPercent = verdict.HumanPercent, verdict.BotPercent
		v.TrafficRatioStatus = verdict.TrafficRatioStatus
		e = ipcheck.IPPureEvidence(j.Payload.IP, verdict.Attributes, e.CheckedAt)
		e.QueryStatus = "success"
	}
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if err := mergeAttributes(ctx, tx, j.Payload.IP, []ipcheck.Evidence{e}, v); err != nil {
			return err
		}
		delay := 24 * time.Hour
		if queryErr != nil {
			delay = 30 * time.Minute
			if j.Priority > 0 {
				delay = r.Config.ResidentialInterval
			}
		} else if v.HumanPercent == nil || v.BotPercent == nil {
			delay = trafficRatioRetryInterval
		}
		return recur(ctx, tx, j, delay)
	})
}
