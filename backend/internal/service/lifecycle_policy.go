package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
)

const retirementRoundInterval = 2 * time.Minute
const retirementFailedRounds = 3

// Only independently confirmed rounds count. Success or inconclusive controls
// reset the streak. Closely spaced deliveries cannot accelerate retirement.
func accumulateFailures(previous, current domain.ConnectivityState) domain.ConnectivityState {
	if current.Reachable || !current.ConfirmedDead {
		return current
	}
	now, err := time.Parse(time.RFC3339Nano, current.CheckedAt)
	if err != nil {
		current.ConfirmedDead = false
		return current
	}
	last, e := time.Parse(time.RFC3339Nano, previous.LastFailureAt)
	first, f := time.Parse(time.RFC3339Nano, previous.FirstFailureAt)
	if previous.ProbeTarget != current.ProbeTarget || previous.Reachable || previous.FailedRounds < 1 || e != nil || f != nil || first.After(last) || last.After(now) || now.Sub(last) > 2*time.Hour {
		current.FailedRounds = 1
		current.FirstFailureAt, current.LastFailureAt = current.CheckedAt, current.CheckedAt
	} else {
		current.FailedRounds = previous.FailedRounds
		current.FirstFailureAt, current.LastFailureAt = previous.FirstFailureAt, previous.LastFailureAt
		if now.Sub(last) >= retirementRoundInterval {
			current.FailedRounds = min(retirementFailedRounds, current.FailedRounds+1)
			current.LastFailureAt = current.CheckedAt
		}
	}
	current.ConfirmedDead = retirementConfirmed(current)
	if !current.ConfirmedDead {
		current.Error = "连通性检测失败，等待跨周期复测"
	}
	return current
}

func retirementConfirmed(v domain.ConnectivityState) bool {
	first, e := time.Parse(time.RFC3339Nano, v.FirstFailureAt)
	last, f := time.Parse(time.RFC3339Nano, v.LastFailureAt)
	return v.ConfirmedDead && !v.Reachable && v.FailedRounds >= retirementFailedRounds && e == nil && f == nil && last.Sub(first) >= (retirementFailedRounds-1)*retirementRoundInterval
}

func checkExpectedExit(v *domain.ExitState, expected string) {
	// A failed echo has no new IP to compare. Preserve its real failure reason.
	if expected != "" && v.Verified && v.Error == "" && v.IP != "" && v.IP != expected {
		v.Error = "复核期间出口变化"
	}
}

func recoveryBackoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return 5 * time.Minute
	case attempts == 2:
		return 15 * time.Minute
	case attempts == 3:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}

// Keep the durable marker and its history; a new generation creates new probe
// sessions. Exact retirement, completed state and generation fence every retry.
func (r *Repository) retryResidentialRecovery(ctx context.Context) error {
	rows, err := r.Pool.Query(ctx, `SELECT w.id,w.generation,w.payload FROM node_explorer_work w
	 JOIN node_explorer_node_registry n ON n.identity=w.payload->>'identity'
	 JOIN node_explorer_retired_nodes t ON t.identity=n.identity
	 WHERE w.kind='recovery' AND w.state='done' AND n.available AND n.historical_residential
	 AND w.key=n.identity||':'||t.retired_at
	 AND w.payload->>'retiredAt'=t.retired_at AND w.payload->>'reason'=t.reason
	 AND w.payload->>'recoveryResult' IN ('probe-failed','exit-probe-failed','exit-changed','timed-out','classification-unconfirmed','config-rejected')
	 AND w.payload->>'recoveryResult'<>'config-rejected-dead'
	 AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)
	 ORDER BY w.due_at,w.id`)
	if err != nil {
		return err
	}
	type retry struct {
		id, generation int64
		payload        Payload
	}
	var ready []retry
	now := time.Now()
	for rows.Next() {
		var v retry
		var raw []byte
		if err = rows.Scan(&v.id, &v.generation, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &v.payload); err != nil {
			rows.Close()
			return err
		}
		stamp, e := time.Parse(time.RFC3339Nano, v.payload.CheckedAt)
		delay := recoveryBackoff(v.payload.RecoveryAttempts)
		if v.payload.RecoveryResult == "config-rejected" {
			delay = 6 * time.Hour
		}
		if e == nil && automatic(v.payload.Reason) && !now.Before(stamp.Add(delay)) {
			ready = append(ready, v)
		}
		if len(ready) >= 32 {
			break
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range ready {
		_, err = r.Pool.Exec(ctx, `UPDATE node_explorer_work w SET state='queued',generation=generation+1,
		 due_at=now(),payload=payload-'startedAt',lease_token=NULL,lease_until=NULL,attempts=0,error=''
		 WHERE id=$1 AND generation=$2 AND state='done' AND payload->>'checkedAt'=$3
		 AND EXISTS(SELECT 1 FROM node_explorer_retired_nodes t JOIN node_explorer_node_registry n USING(identity)
		 WHERE t.identity=w.payload->>'identity' AND t.retired_at=w.payload->>'retiredAt'
		 AND t.reason=w.payload->>'reason' AND n.available AND n.historical_residential
		 AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id))`, v.id, v.generation, v.payload.CheckedAt)
		if err != nil {
			return err
		}
	}
	return nil
}

// Return a bounded category, never a transport error containing proxy credentials.
func probeFailure(err error) string {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "DNS failure"
	}
	return "connection or TLS failure"
}
