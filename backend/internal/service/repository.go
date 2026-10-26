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
	"github.com/cytusc/node_list/backend/internal/ratelimit"
	"github.com/cytusc/node_list/backend/internal/taskqueue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	KindExit       = "exit"
	KindAttributes = "attributes"
	KindRisk       = "risk"
)

type Payload struct {
	Incarnation string `json:"incarnation,omitempty"`
	Identity    string `json:"identity,omitempty"`
	IP          string `json:"ip,omitempty"`
	ExitVersion int64  `json:"exitVersion,omitempty"`
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
	Pool *pgxpool.Pool
	// Limiter paces outbound provider calls across replicas. Nil means this
	// process paces alone, which is correct for a single-replica deployment.
	Limiter         *ratelimit.Limiter
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
	(node_explorer_work.state='queued' AND EXCLUDED.due_at<node_explorer_work.due_at
	 AND NOT (node_explorer_work.error LIKE $6 AND node_explorer_work.due_at>now()))`, kind, key, b, priority, due, configRejectedPattern())
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
 SELECT *,row_number() OVER(PARTITION BY kind,(priority>0) ORDER BY due_at,id) AS lane_position
 FROM node_explorer_work WHERE kind=ANY($1) AND ((state='queued' AND due_at<=now()+interval '10 seconds') OR (state='running' AND lease_until<now()))
 ) pending WHERE lane_position<=1000 AND (published_at IS NULL OR published_at<now()-interval '30 seconds'
 OR published_generation IS DISTINCT FROM generation OR published_due_at IS DISTINCT FROM due_at OR published_priority IS DISTINCT FROM priority)
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
	var raw []byte
	err := r.Pool.QueryRow(ctx, `UPDATE node_explorer_work SET state='running',lease_token=$4,
 lease_until=now()+interval '3 minutes',attempts=attempts+1 WHERE id=$1 AND generation=$2 AND kind=$3
 AND (priority>0)=$5 AND due_at<=now() AND (state='queued' OR (state='running' AND lease_until<now()))
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
	Incarnation           string
	HistoricalResidential bool
}

func (r *Repository) Target(ctx context.Context, id string) (*Target, error) {
	var raw []byte
	t := &Target{Identity: id}
	err := r.Pool.QueryRow(ctx, `SELECT n.node,n.historical_residential,n.incarnation FROM node_explorer_node_registry n WHERE n.identity=$1 AND n.available
	AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`, id).Scan(&raw, &t.HistoricalResidential, &t.Incarnation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &t.Node); err != nil {
		return nil, err
	}
	return t, nil
}
