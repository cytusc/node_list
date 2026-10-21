package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

const trafficRatioRetryInterval = 6 * time.Hour
const trafficRatioErrorRetryInterval = 30 * time.Minute

type scheduledWork struct {
	Kind     string    `json:"kind"`
	Key      string    `json:"key"`
	Payload  Payload   `json:"payload"`
	Priority int       `json:"priority"`
	Due      time.Time `json:"due_at"`
	Mode     string    `json:"mode"`
}

// scheduleMany filters unchanged work in SQL before attempting an upsert. A
// batch takes one database round trip instead of one per current node.
func scheduleMany(ctx context.Context, tx pgx.Tx, work []scheduledWork) error {
	unique := make([]scheduledWork, 0, len(work))
	positions := make(map[string]int, len(work))
	for _, w := range work {
		key := w.Kind + ":" + w.Key
		if i, ok := positions[key]; ok {
			unique[i].Priority = max(unique[i].Priority, w.Priority)
			if w.Due.Before(unique[i].Due) {
				unique[i].Due = w.Due
			}
			continue
		}
		positions[key] = len(unique)
		unique = append(unique, w)
	}
	for start := 0; start < len(unique); start += 250 {
		b, err := json.Marshal(unique[start:min(start+250, len(unique))])
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at)
		SELECT d.kind,d.key,d.payload,d.priority,d.due_at
		FROM jsonb_to_recordset($1::jsonb) AS d(kind text,key text,payload jsonb,priority integer,due_at timestamptz,mode text)
		LEFT JOIN node_explorer_work w ON w.kind=d.kind AND w.key=d.key
		 WHERE w.id IS NULL OR (d.mode<>'once' AND (w.state='done' OR
			 (w.state='queued' AND NOT (w.error LIKE $2 AND w.due_at>now())
		 AND (d.due_at<w.due_at OR ((d.mode='risk' OR d.kind='exit') AND d.priority>w.priority)))))
		ORDER BY d.kind,d.key
		ON CONFLICT(kind,key) DO UPDATE SET
		payload=CASE WHEN EXCLUDED.kind='risk' AND node_explorer_work.state='queued' THEN node_explorer_work.payload ELSE EXCLUDED.payload END,
		priority=CASE WHEN EXCLUDED.kind='risk' AND node_explorer_work.state='queued' THEN greatest(node_explorer_work.priority,EXCLUDED.priority) ELSE EXCLUDED.priority END,
		due_at=CASE WHEN EXCLUDED.kind='risk' AND node_explorer_work.state='queued' THEN least(node_explorer_work.due_at,EXCLUDED.due_at) ELSE EXCLUDED.due_at END,
		state='queued',generation=node_explorer_work.generation+1,attempts=0,error=''
		 WHERE EXCLUDED.kind NOT IN ('recovery','normal-recovery') AND (node_explorer_work.state='done' OR
			 (node_explorer_work.state='queued' AND NOT (node_explorer_work.error LIKE $2 AND node_explorer_work.due_at>now())
		 AND (EXCLUDED.due_at<node_explorer_work.due_at OR
		 (EXCLUDED.kind IN ('risk','exit') AND EXCLUDED.priority>node_explorer_work.priority))))`, b, configRejectedPattern())
		if err != nil {
			return err
		}
	}
	return nil
}

func once(ctx context.Context, tx pgx.Tx, kind, key string, p Payload, priority int) error {
	return onceDue(ctx, tx, kind, key, p, time.Now(), priority)
}

func onceDue(ctx context.Context, tx pgx.Tx, kind, key string, p Payload, due time.Time, priority int) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(kind,key) DO NOTHING`, kind, key, b, priority, due)
	return e
}

func (r *Runner) schedule(ctx context.Context) error {
	switch r.Config.Role {
	case "node-cleanup":
		return r.scheduleCleanup(ctx)
	case "recovery":
		// Strict recovery upkeep moves here with the merged role: requeue
		// finished markers on their backoff, skip hosting-gone candidates, and
		// advance freshly failed normal-recovery rows.
		if err := r.Repo.retryResidentialRecovery(ctx); err != nil {
			return err
		}
		if err := r.Repo.skipHostingRecovery(ctx); err != nil {
			return err
		}
		return r.scheduleNormalRecovery(ctx)
	case "exit-attributes":
		// 属性/风险任务由 exit 结果投递与 recur 自续期产生，本服务无需周期扫描。
		return nil
	case "exit-provision":
	default:
		// Per-node liveness scheduling moved into the sweeper round (§3.3);
		// retired recoveries are promoted by the round commit.
		return nil
	}
	// exit-provision keeps re-deriving exit checks and risk top-ups for the
	// unretired set ahead of their due time; completed rows refresh so repeated
	// scans stay a no-op for unchanged state.
	rows, err := r.Repo.Pool.Query(ctx, `SELECT n.identity,c.payload,e.payload,a.payload
	FROM node_explorer_node_registry n LEFT JOIN node_explorer_connectivity c USING(identity)
	LEFT JOIN node_explorer_exits e USING(identity) LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
	LEFT JOIN node_explorer_retired_nodes t USING(identity)
	WHERE n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)
	AND t.identity IS NULL`)
	if err != nil {
		return err
	}
	type item struct {
		id      string
		c, e, a []byte
	}
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.c, &i.e, &i.a); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	tx, err := r.Repo.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	work := make([]scheduledWork, 0, len(items))
	now := time.Now()
	for _, i := range items {
		var connectivity domain.ConnectivityState
		var exit domain.ExitState
		var attr domain.IPPureResult
		if len(i.c) > 0 {
			if err = json.Unmarshal(i.c, &connectivity); err != nil {
				return err
			}
		}
		if len(i.e) > 0 {
			if err = json.Unmarshal(i.e, &exit); err != nil {
				return err
			}
		}
		if len(i.a) > 0 {
			if err = json.Unmarshal(i.a, &attr); err != nil {
				return err
			}
		}
		priority := 0
		if domain.FreshResidential(&attr, exit.IP, now) || (connectivity.Reachable && exit.IP == "") {
			priority = 1
		}
		p := Payload{Identity: i.id}
		work = append(work, scheduledWork{Kind: KindExit, Key: i.id, Payload: p, Priority: priority, Due: nextDue(exit.CheckedAt, r.Config.NormalInterval)})
		if exit.IP != "" && missingTrafficRatio(attr, now) {
			work = append(work, scheduledWork{Kind: KindRisk, Key: exit.IP, Payload: Payload{IP: exit.IP, Identity: i.id}, Priority: priority, Due: now, Mode: "risk"})
		}
	}
	if err = scheduleMany(ctx, tx, work); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nextDue(stamp string, interval time.Duration) time.Time {
	t, e := time.Parse(time.RFC3339Nano, stamp)
	if e != nil {
		return time.Now()
	}
	return t.Add(interval)
}

// guard rechecks retirement after the network requests and serializes against
// lifecycle transitions. Manual disable always wins over in-flight probes.
func guard(ctx context.Context, tx pgx.Tx, p Payload) (bool, error) {
	if p.Action == "cleanup" {
		return cleanupGuard(ctx, tx, p)
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT identity FROM node_explorer_node_registry WHERE identity=$1 AND available AND ($2::text='' OR incarnation=$2)
	AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=snapshot_id) FOR UPDATE`, p.Identity, p.Incarnation).Scan(&id)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var row store.RetiredRow
	err = tx.QueryRow(ctx, `SELECT retired_at,reason FROM node_explorer_retired_nodes WHERE identity=$1`, p.Identity).Scan(&row.RetiredAt, &row.Reason)
	if err == pgx.ErrNoRows {
		return p.RetiredAt == "", nil
	}
	if err != nil {
		return false, err
	}
	return p.RetiredAt != "" && row.RetiredAt == p.RetiredAt && row.Reason == p.Reason && automatic(row.Reason), nil
}

func scheduleIP(ctx context.Context, tx pgx.Tx, ip, id string, priority int) error {
	if ip == "" {
		return nil
	}
	p := Payload{IP: ip, Identity: id}
	for _, kind := range []string{KindAttributes, KindRisk} {
		// A completed lookup has its own next_due; regular exit checks do not
		// reset it. Initial lookups are created once and refresh themselves.
		if err := once(ctx, tx, kind, ip, p, priority); err != nil {
			return err
		}
		if err := scheduleCompletedIP(ctx, tx, kind, ip, p, priority); err != nil {
			return err
		}
		if priority > 0 {
			if _, err := tx.Exec(ctx, `UPDATE node_explorer_work SET priority=1 WHERE kind=$1 AND key=$2 AND state='queued' AND priority=0`, kind, ip); err != nil {
				return err
			}
		}
	}
	return nil
}

func missingTrafficRatio(attr domain.IPPureResult, now time.Time) bool {
	if attr.Status == "success" && attr.HumanPercent != nil && attr.BotPercent != nil {
		return false
	}
	if attr.Status != "success" {
		return false
	}
	// A failed required query retains the last successful verdict. Its old
	// ratio timestamp must not bypass the failed query's retry backoff.
	if attr.RiskQueryStatus == "query-failed" {
		if t, err := time.Parse(time.RFC3339Nano, attr.RiskQueryCheckedAt); err == nil && now.Sub(t) < trafficRatioErrorRetryInterval {
			return false
		}
	}
	retryInterval := trafficRatioRetryInterval
	if attr.TrafficRatioStatus == "query-failed" {
		retryInterval = trafficRatioErrorRetryInterval
	}
	if attr.TrafficRatioCheckedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, attr.TrafficRatioCheckedAt); err == nil && now.Sub(t) < retryInterval {
			return false
		}
	}
	return true
}

func scheduleCompletedIP(ctx context.Context, tx pgx.Tx, kind, ip string, p Payload, priority int) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE node_explorer_work SET state='queued',generation=generation+1,due_at=now(),payload=$3,priority=$4,attempts=0,error='' WHERE kind=$1 AND key=$2 AND state='done'`, kind, ip, b, priority)
	return err
}

// Risk refreshes distinguish optional statistics outages from absent data.
func riskRefreshInterval(v domain.IPPureResult) time.Duration {
	if v.TrafficRatioStatus == "query-failed" {
		return trafficRatioErrorRetryInterval
	}
	if v.HumanPercent == nil || v.BotPercent == nil {
		return trafficRatioRetryInterval
	}
	return 24 * time.Hour
}
