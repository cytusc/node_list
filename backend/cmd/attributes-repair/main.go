// Command attributes-repair re-queues addresses whose residential verdict was
// lost before failures stopped overwriting successful evidence.
//
// A refused refresh used to replace the stored answer with a "request-failed"
// row, so an address that had been classified as residential fell back to
// unknown and the exit list, which publishes only residential nodes, dropped
// it. Prevention alone leaves those rows unknown forever: the record no longer
// remembers what it once knew. This command finds the nodes that still carry
// the historical flag but no longer hold a residential verdict and no
// confident datacenter answer either, and puts them back in the attribute
// queue so the fixed voter can classify them again.
//
// It is read-only by default and enqueues only with -apply.
//
//	PGHOST=... go run ./cmd/attributes-repair
//	PGHOST=... go run ./cmd/attributes-repair -apply -limit 200
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/jackc/pgx/v5"
)

// defaultLimit keeps one run inside a queue the lane can actually retire. The
// normal lane is shared with fresh work, so a repair sweep must not flood it.
const defaultLimit = 200

// confidentDatacenter marks an answer that positively identified a hosting
// network. Such a node was corrected, not damaged, and must stay out of the
// repair set: re-asking would only spend quota to reach the same verdict.
const confidentDatacenter = `(ev->>'outcome' IN ('datacenter','provider-conflict') OR coalesce(ev->>'hosting','') = 'true')`

// lostResidential selects one row per address. The GROUP BY is load bearing:
// several identities can share an exit IP, and a duplicate key in the SELECT
// makes ON CONFLICT DO UPDATE fail with "cannot affect row a second time".
const lostResidential = `
 SELECT a.ip AS ip, min(e.identity) AS identity
 FROM node_explorer_ip_attributes a
 JOIN node_explorer_exits e ON e.ip = a.ip
 JOIN node_explorer_node_registry n ON n.identity = e.identity
 WHERE n.available
   AND n.historical_residential
   AND coalesce(a.payload->'classification'->>'state','') <> 'residential'
   AND NOT EXISTS (
     SELECT 1
     FROM jsonb_array_elements(coalesce(a.payload->'classification'->'evidence','[]'::jsonb)) ev
     WHERE coalesce(ev->>'queryStatus','') IN ('', 'success')
       AND ` + confidentDatacenter + `)
 GROUP BY a.ip
 ORDER BY a.ip
 LIMIT CASE WHEN $1 > 0 THEN $1 ELSE NULL END`

// enqueueLost inserts the repair jobs. A task that is queued or running is left
// alone; only a finished one is reopened, so repeated runs stay cheap.
const enqueueLost = `
 INSERT INTO node_explorer_work(kind,key,payload,priority,due_at)
 SELECT 'attributes', s.ip, jsonb_build_object('ip',s.ip,'identity',s.identity), $2, now()
 FROM (` + lostResidential + `) s
 ON CONFLICT(kind,key) DO UPDATE
 SET state='queued', priority=EXCLUDED.priority, due_at=now(),
     generation=node_explorer_work.generation+1, attempts=0, error=''
 WHERE node_explorer_work.state='done'`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func validate(limit, priority int) error {
	if limit < 0 {
		return fmt.Errorf("limit must be 0 (unlimited) or positive")
	}
	if priority < 0 {
		return fmt.Errorf("priority must not be negative")
	}
	return nil
}

func run() error {
	apply := flag.Bool("apply", false, "enqueue matching addresses; default is a read-only count")
	limit := flag.Int("limit", defaultLimit, "maximum addresses per run; 0 means unlimited")
	priority := flag.Int("priority", 0, "queue priority for the repair jobs; 0 keeps them on the shared lane")
	flag.Parse()
	if err := validate(*limit, *priority); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// An empty connection string makes pgx read PGHOST, PGPORT, PGDATABASE,
	// PGUSER and PGPASSWORD, matching the other operational commands.
	db, err := pgx.Connect(ctx, "")
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer db.Close(context.Background())

	var candidates int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM (`+lostResidential+`) s`, *limit).Scan(&candidates); err != nil {
		return fmt.Errorf("count candidates: %w", err)
	}
	fmt.Printf("lost residential verdicts: %d\n", candidates)
	if !*apply {
		fmt.Println("dry run: pass -apply to enqueue them")
		return nil
	}
	tag, err := db.Exec(ctx, enqueueLost, *limit, *priority)
	if err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	fmt.Printf("enqueued: %d\n", tag.RowsAffected())
	return nil
}
