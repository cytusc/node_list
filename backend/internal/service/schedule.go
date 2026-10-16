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

func once(ctx context.Context, tx pgx.Tx, kind, key string, p Payload, priority int) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at) VALUES($1,$2,$3,$4,now()) ON CONFLICT(kind,key) DO NOTHING`, kind, key, b, priority)
	return e
}

func (r *Runner) schedule(ctx context.Context) error {
	if r.Config.Role != "connectivity" && r.Config.Role != "ip" && r.Config.Role != "residential" {
		return nil
	}
	rows, err := r.Repo.Pool.Query(ctx, `SELECT n.identity,n.historical_residential,c.payload,e.payload,a.payload,t.retired_at,t.reason
	FROM node_explorer_node_registry n LEFT JOIN node_explorer_connectivity c USING(identity)
	LEFT JOIN node_explorer_exits e USING(identity) LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
	LEFT JOIN node_explorer_retired_nodes t USING(identity)
	WHERE n.available AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`)
	if err != nil {
		return err
	}
	type item struct {
		id              string
		historical      bool
		c, e, a         []byte
		retired, reason *string
	}
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.historical, &i.c, &i.e, &i.a, &i.retired, &i.reason); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	tx, err := r.Repo.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, i := range items {
		var live domain.ConnectivityState
		var exit domain.ExitState
		var attr domain.IPPureResult
		if len(i.c) > 0 {
			if err = json.Unmarshal(i.c, &live); err != nil {
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
		residential := domain.FreshResidential(&attr, exit.IP, time.Now())
		interval := r.Config.NormalInterval
		priority := 0
		if residential || (i.retired != nil && i.historical) {
			interval = r.Config.ResidentialInterval
			priority = 1
		}
		p := Payload{Identity: i.id}
		if i.retired != nil {
			if r.Config.Role != "residential" || !automatic(*i.reason) {
				continue
			}
			p.RetiredAt, p.Reason = *i.retired, *i.reason
			// Completed recovery cycles are rescheduled by the cycle itself.
			if err = once(ctx, tx, KindRecovery, i.id+":"+*i.retired, p, priority); err != nil {
				return err
			}
			continue
		}
		switch r.Config.Role {
		case "connectivity":
			if err = schedule(ctx, tx, KindLive, i.id, p, priority, nextDue(live.CheckedAt, interval)); err != nil {
				return err
			}
		case "ip":
			if err = schedule(ctx, tx, KindExit, i.id, p, priority, nextDue(exit.CheckedAt, interval)); err != nil {
				return err
			}
			if exit.IP != "" {
				if err = scheduleMissingTrafficRatio(ctx, tx, exit.IP, i.id, priority, attr); err != nil {
					return err
				}
			}
		}
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
	var id string
	err := tx.QueryRow(ctx, `SELECT identity FROM node_explorer_node_registry WHERE identity=$1 AND available
	AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=snapshot_id) FOR UPDATE`, p.Identity).Scan(&id)
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

func scheduleMissingTrafficRatio(ctx context.Context, tx pgx.Tx, ip, id string, priority int, attr domain.IPPureResult) error {
	if attr.Status == "success" && attr.HumanPercent != nil && attr.BotPercent != nil {
		return nil
	}
	if attr.Status != "success" {
		return nil
	}
	retryInterval := trafficRatioRetryInterval
	if attr.TrafficRatioStatus == "query-failed" {
		retryInterval = trafficRatioErrorRetryInterval
	}
	if attr.TrafficRatioCheckedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, attr.TrafficRatioCheckedAt); err == nil && time.Since(t) < retryInterval {
			return nil
		}
	}
	p := Payload{IP: ip, Identity: id}
	if err := once(ctx, tx, KindRisk, ip, p, priority); err != nil {
		return err
	}
	if err := scheduleCompletedIP(ctx, tx, KindRisk, ip, p, priority); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE node_explorer_work SET due_at=now() WHERE kind=$1 AND key=$2 AND state='queued' AND due_at>now()`, KindRisk, ip); err != nil {
		return err
	}
	if priority > 0 {
		_, err := tx.Exec(ctx, `UPDATE node_explorer_work SET priority=1 WHERE kind=$1 AND key=$2 AND state='queued' AND priority=0`, KindRisk, ip)
		return err
	}
	return nil
}

func scheduleCompletedIP(ctx context.Context, tx pgx.Tx, kind, ip string, p Payload, priority int) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE node_explorer_work SET state='queued',generation=generation+1,due_at=now(),payload=$3,priority=$4,attempts=0,error='' WHERE kind=$1 AND key=$2 AND state='done'`, kind, ip, b, priority)
	return err
}
