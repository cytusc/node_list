package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/taskqueue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	KindLive       = "connectivity"
	KindExit       = "exit"
	KindAttributes = "attributes"
	KindRisk       = "risk"
	KindRecovery   = "recovery"
	KindLifecycle  = "lifecycle"
)

type RecoveryAssessment struct {
	Result     string `json:"result"`
	IP         string `json:"ip,omitempty"`
	CheckedAt  string `json:"checkedAt"`
	Generation int64  `json:"generation,omitempty"`
}

type Payload struct {
	RecoveryAttempts int                  `json:"recoveryAttempts,omitempty"`
	RecoveryHistory  []RecoveryAssessment `json:"recoveryHistory,omitempty"`
	RecoveryResult   string               `json:"recoveryResult,omitempty"`
	Identity         string               `json:"identity,omitempty"`
	CheckedAt        string               `json:"checkedAt,omitempty"`
	IP               string               `json:"ip,omitempty"`
	ExitVersion      int64                `json:"exitVersion,omitempty"`
	RetiredAt        string               `json:"retiredAt,omitempty"`
	Reason           string               `json:"reason,omitempty"`
	Session          string               `json:"session,omitempty"`
	ExpectedIP       string               `json:"expectedIP,omitempty"`
	Action           string               `json:"action,omitempty"`
	StartedAt        time.Time            `json:"startedAt,omitempty,omitzero"`
}

type Job struct {
	ID               int64
	Generation       int64
	Kind, Key, Token string
	Priority         int
	Attempts         int
	Payload          Payload
	Due              time.Time
}

func (j Job) Ref() string { return fmt.Sprintf("%d:%d", j.ID, j.Generation) }

type Repository struct {
	Pool            *pgxpool.Pool
	registryMu      sync.Mutex
	registryVersion int64
	registryReady   bool
}

// schedule is used inside the same transaction as the fact that creates work.
func schedule(ctx context.Context, tx pgx.Tx, kind, key string, p Payload, priority int, due time.Time) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO node_explorer_work(kind,key,payload,priority,due_at)
	VALUES($1,$2,$3,$4,$5) ON CONFLICT(kind,key) DO UPDATE SET
	payload=EXCLUDED.payload,priority=EXCLUDED.priority,due_at=EXCLUDED.due_at,
	state='queued',generation=node_explorer_work.generation+1,attempts=0,error=''
	WHERE node_explorer_work.state='done' OR
	(node_explorer_work.state='queued' AND EXCLUDED.due_at<node_explorer_work.due_at)`, kind, key, b, priority, due)
	return err
}

func (r *Repository) Schedule(ctx context.Context, kind, key string, p Payload, priority int, due time.Time) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = schedule(ctx, tx, kind, key, p, priority, due); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *Repository) Publish(ctx context.Context, q taskqueue.Queue, kinds []string) error {
	rows, err := r.Pool.Query(ctx, `SELECT id,generation,kind,priority,due_at FROM (
	SELECT id,generation,kind,priority,due_at,published_at,published_generation,published_due_at,published_priority,
	row_number() OVER(PARTITION BY kind,(priority>0) ORDER BY CASE WHEN kind='recovery' AND payload ? 'startedAt' THEN 0 ELSE 1 END,due_at,id) AS lane_position FROM node_explorer_work
	WHERE kind=ANY($1) AND ((state='queued' AND due_at<=now()+interval '10 seconds')
	OR (state='running' AND lease_until<now()))) pending
	WHERE lane_position<=CASE WHEN kind='recovery' THEN 2 ELSE 1000 END
	AND (published_at IS NULL OR published_at<now()-interval '30 seconds'
	 OR published_generation IS DISTINCT FROM generation OR published_due_at IS DISTINCT FROM due_at
	 OR published_priority IS DISTINCT FROM priority)
	ORDER BY priority DESC,due_at,id`, kinds)
	if err != nil {
		return err
	}
	var jobs []Job
	for rows.Next() {
		var j Job
		if err = rows.Scan(&j.ID, &j.Generation, &j.Kind, &j.Priority, &j.Due); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(jobs) == 0 {
		return err
	}
	// Release the query connection before talking to Redis. Record publication
	// only after the whole wave succeeds; partial delivery is safe to repeat.
	deliveries := make([]taskqueue.Delivery, 0, len(jobs))
	ids, generations, priorities := make([]int64, 0, len(jobs)), make([]int64, 0, len(jobs)), make([]int, 0, len(jobs))
	dues := make([]time.Time, 0, len(jobs))
	for _, j := range jobs {
		deliveries = append(deliveries, taskqueue.Delivery{Kind: j.Kind, High: j.Priority > 0, Ref: j.Ref(), Due: j.Due})
		ids, generations, priorities, dues = append(ids, j.ID), append(generations, j.Generation), append(priorities, j.Priority), append(dues, j.Due)
	}
	if batch, ok := q.(taskqueue.BatchQueue); ok {
		if err = batch.PushBatch(ctx, deliveries); err != nil {
			return err
		}
	} else {
		for _, d := range deliveries {
			if err = q.Push(ctx, d.Kind, d.High, d.Ref, d.Due); err != nil {
				return err
			}
		}
	}
	// Fence concurrent reschedules and promotions. Unchanged queued work is
	// repaired every 30 seconds after Redis loss or a crash between Pop and Claim.
	_, err = r.Pool.Exec(ctx, `UPDATE node_explorer_work w SET published_at=now(),
	 published_generation=d.generation,published_due_at=d.due_at,published_priority=d.priority
	 FROM unnest($1::bigint[],$2::bigint[],$3::timestamptz[],$4::integer[]) AS d(id,generation,due_at,priority)
	 WHERE w.id=d.id AND w.generation=d.generation AND w.due_at=d.due_at AND w.priority=d.priority`, ids, generations, dues, priorities)
	return err
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (r *Repository) Claim(ctx context.Context, kind, ref string, high bool) (*Job, error) {
	parts := strings.Split(ref, ":")
	if len(parts) != 2 {
		return nil, nil
	}
	id, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil {
		return nil, nil
	}
	generation, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil {
		return nil, nil
	}
	j := &Job{ID: id, Generation: generation, Kind: kind, Token: randomToken()}
	query := r.Pool.QueryRow
	var admission pgx.Tx
	if kind == KindRecovery {
		// Bound complete assessments, including coordinators awaiting probes.
		// A lane-specific transaction lock makes admission safe across replicas.
		admission, e = r.Pool.Begin(ctx)
		if e != nil {
			return nil, e
		}
		defer admission.Rollback(ctx)
		lane := "normal"
		if high {
			lane = "residential"
		}
		if _, e = admission.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext($1))`, "recovery-admission:"+lane); e != nil {
			return nil, e
		}
		query = admission.QueryRow
	}
	var raw []byte
	err := query(ctx, `UPDATE node_explorer_work SET state='running',lease_token=$4,
	lease_until=now()+interval '3 minutes',attempts=attempts+1,
 payload=CASE WHEN kind='recovery' AND (NOT(payload ? 'startedAt') OR payload->>'startedAt' LIKE '0001-%') THEN payload || jsonb_build_object('startedAt',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')) ELSE payload END WHERE id=$1 AND generation=$2 AND kind=$3
	AND (priority>0)=$5 AND due_at<=now() AND (state='queued' OR (state='running' AND lease_until<now()))
 AND (kind<>'recovery' OR (payload ? 'startedAt' AND payload->>'startedAt' NOT LIKE '0001-%') OR
 (SELECT count(*) FROM node_explorer_work active WHERE active.kind='recovery' AND (active.priority>0)=$5
 AND active.state<>'done' AND active.payload ? 'startedAt' AND active.payload->>'startedAt' NOT LIKE '0001-%')<2)
	RETURNING key,payload,priority,attempts,due_at`, id, generation, kind, j.Token, high).Scan(&j.Key, &raw, &j.Priority, &j.Attempts, &j.Due)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &j.Payload); err != nil {
		return nil, err
	}
	if admission != nil {
		if err = admission.Commit(ctx); err != nil {
			return nil, err
		}
	}
	return j, nil
}

var ErrLeaseLost = errors.New("task lease expired")

func (r *Repository) Finish(ctx context.Context, j Job, fn func(pgx.Tx) error) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id int64
	err = tx.QueryRow(ctx, `SELECT id FROM node_explorer_work WHERE id=$1 AND generation=$2
	AND state='running' AND lease_token=$3 AND lease_until>now() FOR UPDATE`, j.ID, j.Generation, j.Token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if fn != nil {
		if err = fn(tx); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE node_explorer_work SET state='done',lease_token=NULL,lease_until=NULL,error='' WHERE id=$1 AND lease_token=$2`, j.ID, j.Token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func recur(ctx context.Context, tx pgx.Tx, j Job, delay time.Duration) error {
	_, err := tx.Exec(ctx, `UPDATE node_explorer_work SET state='queued',generation=generation+1,
	payload=payload-'startedAt',due_at=now()+$2::interval,lease_token=NULL,lease_until=NULL,attempts=0,error='' WHERE id=$1`, j.ID, fmt.Sprintf("%f seconds", delay.Seconds()))
	return err
}

func (r *Repository) Retry(ctx context.Context, j Job, delay time.Duration, message string) error {
	_, err := r.Pool.Exec(ctx, `UPDATE node_explorer_work SET state='queued',due_at=now()+$4::interval,
	lease_token=NULL,lease_until=NULL,error=$5 WHERE id=$1 AND generation=$2 AND lease_token=$3`, j.ID, j.Generation, j.Token, fmt.Sprintf("%f seconds", delay.Seconds()), message)
	return err
}

type Target struct {
	Node                  domain.Node
	Identity              string
	HistoricalResidential bool
	Retired               *store.RetiredRow
}

func (r *Repository) Target(ctx context.Context, id string) (*Target, error) {
	var raw []byte
	var retired, reason *string
	t := &Target{Identity: id}
	err := r.Pool.QueryRow(ctx, `SELECT n.node,n.historical_residential,r.retired_at,r.reason FROM node_explorer_node_registry n
	LEFT JOIN node_explorer_retired_nodes r USING(identity) WHERE n.identity=$1 AND n.available
	AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`, id).Scan(&raw, &t.HistoricalResidential, &retired, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &t.Node); err != nil {
		return nil, err
	}
	if retired != nil {
		t.Retired = &store.RetiredRow{Identity: id, RetiredAt: *retired, Reason: *reason}
	}
	return t, nil
}

func automatic(reason string) bool {
	return reason == store.RetireRestoreReason || strings.HasPrefix(reason, "IPPure:")
}

func matchesRetirement(t *Target, p Payload) bool {
	if t == nil {
		return false
	}
	if p.RetiredAt == "" {
		return t.Retired == nil
	}
	return t.Retired != nil && automatic(t.Retired.Reason) && t.Retired.RetiredAt == p.RetiredAt && t.Retired.Reason == p.Reason
}
