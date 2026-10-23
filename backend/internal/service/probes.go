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
	t, err := r.Repo.probeTarget(ctx, j.Payload)
	if err != nil {
		return err
	}
	if t == nil || (j.Payload.Action != "cleanup" && !matchesRetirement(t, j.Payload)) {
		return r.Repo.Finish(ctx, j, nil)
	}
	if j.Payload.Incarnation == "" {
		j.Payload.Incarnation = t.Incarnation
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
	ctx, cancelLease := pathContext(ctx, paths)
	defer cancelLease()
	if r.Config.Manager != nil {
		if _, err = r.Repo.Pool.Exec(ctx, `UPDATE node_explorer_work SET payload=payload||jsonb_build_object('capacityAt',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) WHERE id=$1 AND generation=$2 AND lease_token=$3 AND NOT(payload ? 'capacityAt')`, j.ID, j.Generation, j.Token); err != nil {
			return err
		}
	}
	v := domain.ConnectivityState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), ProbeTarget: transport.DelayTarget}
	path := paths[t.Identity]
	if path != nil {
		v.KernelGeneration = path.Generation
	}
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
		if err := validatePaths(ctx, tx, paths); err != nil {
			return err
		}
		return r.saveConnectivity(ctx, tx, j, t, v)
	})
}

// Both delay success and failure review persist the same fenced connectivity proof.
func (r *Runner) saveConnectivity(ctx context.Context, tx pgx.Tx, j Job, t *Target, v domain.ConnectivityState) error {
	if v.Reachable && v.Error == "" {
		if err := recordAvailable(ctx, tx, t.Identity, j.Payload.Incarnation, v.CheckedAt); err != nil {
			return err
		}
	}
	ok, err := guard(ctx, tx, j.Payload)
	if err != nil || !ok {
		return err
	}
	// guard holds the registry lock, serializing streak updates with lifecycle.
	var previous domain.ConnectivityState
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT payload FROM node_explorer_connectivity WHERE identity=$1`, t.Identity).Scan(&raw)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &previous); err != nil {
			return err
		}
		if old, e := time.Parse(time.RFC3339Nano, previous.CheckedAt); e == nil {
			stamp, _ := time.Parse(time.RFC3339Nano, v.CheckedAt)
			if !stamp.After(old) {
				return nil
			}
		}
	}
	if j.Payload.Action != "cleanup" {
		v = accumulateFailures(previous, v)
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
	if v.ConfirmedDead && t.Retired == nil && j.Payload.Action != "cleanup" {
		p := j.Payload
		p.Action = "retire"
		p.Session = j.Ref()
		p.CheckedAt = v.CheckedAt
		return once(ctx, tx, KindLifecycle, "retire:"+j.Ref(), p, j.Priority)
	}
	return nil
}

// StrongExit requires independent HTTPS echoes and rejects host egress. It is
// shared by first and final recovery checks; normal monitoring uses fallbacks.
func StrongExit(ctx context.Context, tunnel, host *http.Client) (string, error) {
	get := func(client *http.Client, route string) (string, error) {
		observed := ""
		for _, endpoint := range []string{"https://api.ipify.org", "https://checkip.amazonaws.com"} {
			req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if e != nil {
				return "", e
			}
			client.CloseIdleConnections()
			resp, e := client.Do(req)
			if e != nil {
				return "", fmt.Errorf("%s %s: %s", route, req.URL.Hostname(), probeFailure(e))
			}
			b, re := io.ReadAll(io.LimitReader(resp.Body, 128))
			resp.Body.Close()
			ip := net.ParseIP(strings.TrimSpace(string(b)))
			if re != nil {
				return "", fmt.Errorf("%s %s: response read failed (%s)", route, req.URL.Hostname(), probeFailure(re))
			}
			if resp.StatusCode != 200 {
				return "", fmt.Errorf("%s %s: HTTP %d", route, req.URL.Hostname(), resp.StatusCode)
			}
			if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
				return "", fmt.Errorf("%s %s: invalid public IPv4 echo", route, req.URL.Hostname())
			}
			if observed != "" && observed != ip.String() {
				return "", fmt.Errorf("%s exit echoes disagree", route)
			}
			observed = ip.String()
		}
		return observed, nil
	}
	ip, err := get(tunnel, "proxy")
	if err != nil {
		return "", err
	}
	direct, err := get(host, "host")
	if err != nil {
		return "", err
	}
	if direct == ip {
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
	if j.Payload.Incarnation == "" {
		j.Payload.Incarnation = t.Incarnation
	}
	previous, err := r.Repo.Exit(ctx, t.Identity)
	if err != nil {
		return err
	}
	started := time.Now()
	paths, release, err := r.reserve(ctx, []domain.Node{t.Node}, high)
	if err != nil {
		if errors.Is(err, transport.ErrConfigRejected) {
			return r.rejectedExit(ctx, j, t, err)
		}
		return err
	}
	defer release()
	ctx, cancelLease := pathContext(ctx, paths)
	defer cancelLease()
	v := domain.ExitState{Incarnation: t.Incarnation, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	path := paths[t.Identity]
	if path != nil {
		v.KernelGeneration = path.Generation
	}
	if path == nil || path.Error != nil {
		v.Error = "连接配置不支持"
	} else if j.Payload.Session != "" {
		probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		direct := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
		defer direct.CloseIdleConnections()
		ip, e := StrongExit(probeCtx, path.Client, direct)
		if e != nil {
			v.Error = "真实出口核验失败：" + e.Error()
		} else {
			v.IP, v.Source, v.Verified, v.AddressFamily = ip, "ipify+aws", true, 4
		}
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
		defer cancel()
		var preferred []string
		if previous != nil && previous.Incarnation == t.Incarnation && previous.Endpoint != "" {
			preferred = []string{previous.Endpoint}
		}
		o, e := collector.ResolveExitIPInfo(probeCtx, path.Client, 5*time.Second, preferred, nil)
		if e != nil || o == nil {
			v.Error = "出口探测失败"
		} else {
			v.IP, v.Source, v.Country, v.AddressFamily, v.Endpoint = o.IP, o.Provider, o.Country, o.AddressFamily, o.Endpoint
		}
	}
	v.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	v.ProbeMillis = time.Since(started).Milliseconds()
	checkExpectedExit(&v, j.Payload.ExpectedIP)
	if j.Payload.Session == "" && v.Error != "" {
		v.FailureStreak = 1
		if previous != nil && previous.Error != "" && (previous.Incarnation == "" || previous.Incarnation == t.Incarnation) {
			v.FailureStreak = min(16, max(1, previous.FailureStreak)+1)
			v.Endpoint = previous.Endpoint
		}
	}
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if err := validatePaths(ctx, tx, paths); err != nil {
			return err
		}
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
			// 住宅快判：ip-api 批量端点加权结果确认住宅时跳过完整属性加权，
			// 直接投 IPPure 风险查询；误判由 attributes() 的属性新鲜度门回补。
			strong := r.residentialQuickVerdict(ctx, v.IP)
			kinds := []string{KindAttributes, KindRisk}
			if strong {
				kinds = kinds[1:]
			}
			payload := Payload{IP: v.IP, Identity: t.Identity}
			for _, kind := range kinds {
				// A completed lookup has its own next_due; regular exit checks
				// do not reset it. Lookups are created once and refresh themselves.
				if e = once(ctx, tx, kind, v.IP, payload, j.Priority); e != nil {
					return e
				}
				if e = scheduleCompletedIP(ctx, tx, kind, v.IP, payload, j.Priority); e != nil {
					return e
				}
				if j.Priority > 0 {
					if _, e = tx.Exec(ctx, `UPDATE node_explorer_work SET priority=1 WHERE kind=$1 AND key=$2 AND state='queued' AND priority=0`, kind, v.IP); e != nil {
						return e
					}
				}
			}
		}
		if v.IP != "" && v.Error == "" {
			_, e = tx.Exec(ctx, `UPDATE node_explorer_node_registry SET historical_residential=true WHERE identity=$1 AND
		EXISTS(SELECT 1 FROM node_explorer_ip_attributes a WHERE a.ip=$2 AND a.payload->'classification'->>'state'='residential')`, t.Identity, v.IP)
		}
		return e
	})
}

// Persist an explanation even when no outbound can be reserved. Work and
// observation updates share the task lease and registry incarnation fences.
func (r *Runner) rejectedExit(ctx context.Context, j Job, t *Target, cause error) error {
	message := configRejectedMessage() + ": " + transport.ConfigRejectionCode(cause)
	v := domain.ExitState{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Error: "节点配置无法加载: " + transport.ConfigRejectionCode(cause)}
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		ok, err := guard(ctx, tx, j.Payload)
		if err != nil || !ok {
			return err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO node_explorer_exits(identity,ip,checked_at,payload) VALUES($1,'',$2,$3)
		ON CONFLICT(identity) DO UPDATE SET checked_at=EXCLUDED.checked_at,
		payload=EXCLUDED.payload||jsonb_build_object('ip',node_explorer_exits.ip)
		WHERE node_explorer_exits.checked_at<=EXCLUDED.checked_at`, t.Identity, v.CheckedAt, b); err != nil {
			return err
		}
		if err = writeProbe(ctx, tx, j.Payload.Session, KindExit, t.Identity, v); err != nil {
			return err
		}
		if err = recur(ctx, tx, j, 6*time.Hour); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE node_explorer_work SET error=$2 WHERE id=$1`, j.ID, message)
		return err
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
	// The ip-api share of the evidence fan-out comes from the batch endpoint:
	// when this exit IP is not cached yet, one request also covers the other
	// addresses already queued for classification. Cache hits skip the priming
	// and the database query entirely.
	if !r.Classification.IPAPICached(j.Payload.IP) {
		if pending, err := r.Repo.PendingAttributeIPs(ctx, 100); err == nil && len(pending) > 0 {
			_ = r.Classification.PrimeIPAPIBatch(ctx, pending)
		}
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
	var paths map[string]*transport.Outbound
	var release func()
	if r.Config.Manager != nil {
		engine := &transport.ManagedEngine{Manager: r.Config.Manager, Kind: "risk", High: high}
		reserved, done, e := engine.Reserve(ctx, []domain.Node{*reference})
		err = e
		release = done
		if e != nil {
			return e
		}
		paths = map[string]*transport.Outbound{domain.AggregateIdentity(*reference): reserved[0]}
	} else {
		paths, release, err = r.reserve(ctx, []domain.Node{*reference}, false)
	}
	if err != nil {
		return err
	}
	defer release()
	ctx, cancelLease := pathContext(ctx, paths)
	defer cancelLease()
	path := paths[domain.AggregateIdentity(*reference)]
	if path == nil || path.Error != nil {
		return waiting{30 * time.Second}
	}
	verdict, queryErr := collector.QueryIPPure(ctx, collector.NewIPPureClient(path.Client, r.Config.APIOrigin, 10*time.Second), j.Payload.IP, r.asnCache)
	v := &domain.IPPureResult{IP: j.Payload.IP, Status: "error", FailureStage: "ippure-query", Error: "风险查询失败"}
	e := ipcheck.Evidence{Provider: "ippure", IP: j.Payload.IP, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Outcome: "unknown", QueryStatus: "request-failed"}
	v.RiskQueryStatus = "query-failed"
	if queryErr != nil {
		v.FailureStage, v.RiskQueryError = collector.IPPureFailure(queryErr)
		v.Error = "风险查询失败: " + v.RiskQueryError
	}
	if queryErr == nil {
		v.Status = "success"
		v.RiskQueryStatus = "success"
		v.FailureStage = ""
		v.Error = ""
		v.ASN, v.Organization, v.Country, v.Source = verdict.ASN, verdict.Organization, verdict.Country, verdict.Source
		v.Attributes, v.Score, v.Label = verdict.Attributes, verdict.Score, verdict.Label
		v.HumanPercent, v.BotPercent = verdict.HumanPercent, verdict.BotPercent
		v.TrafficRatioStatus = verdict.TrafficRatioStatus
		v.TrafficRatioError = verdict.TrafficRatioError
		e = ipcheck.IPPureEvidence(j.Payload.IP, verdict.Attributes, e.CheckedAt)
		e.QueryStatus = "success"
	}
	return r.Repo.Finish(ctx, j, func(tx pgx.Tx) error {
		if err := validatePaths(ctx, tx, paths); err != nil {
			return err
		}
		if err := mergeAttributes(ctx, tx, j.Payload.IP, []ipcheck.Evidence{e}, v); err != nil {
			return err
		}
		if queryErr == nil && v.Classification != nil && v.Classification.State != "residential" {
			// 兜底④：IPPure 否定快判的住宅结论时回补完整属性加权。once 幂等，
			// 完整流程已存在的 attributes 行不会被重复创建。
			if err := once(ctx, tx, KindAttributes, j.Payload.IP, Payload{IP: j.Payload.IP, Identity: j.Payload.Identity}, j.Priority); err != nil {
				return err
			}
		}
		delay := 24 * time.Hour
		if queryErr != nil {
			delay = 30 * time.Minute
			if j.Priority > 0 {
				delay = r.Config.ResidentialInterval
			}
		} else {
			delay = riskRefreshInterval(*v)
		}
		return recur(ctx, tx, j, delay)
	})
}
