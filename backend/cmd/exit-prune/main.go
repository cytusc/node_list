// Command exit-prune deletes stale probe results that never recorded an exit
// IP. A failed probe keeps an empty payload ip, so a backlog of old failures
// makes the frontend look almost entirely broken even though current probes
// succeed. Those rows hold no usable observation: the next probe recreates the
// row. The command is read-only by default and deletes only with -apply.
//
//	PGHOST=... go run ./cmd/exit-prune -older-than 36h
//	PGHOST=... go run ./cmd/exit-prune -older-than 36h -apply -limit 5000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5"
)

// defaultOlderThan keeps a full day of failed probes for diagnosis while
// dropping the long tail that inflates the failure ratio.
const defaultOlderThan = 24 * time.Hour

// staleEmptyExit selects expired rows that never stored an exit IP. The
// payload and the canonical column are both checked so a row that carries an
// address in either place is never pruned.
const staleEmptyExit = `e.checked_at < now() - $1::interval
 AND coalesce(e.payload->>'ip','')='' AND coalesce(e.ip,'')=''`

// pruneStaleEmptyExits deletes at most $2 matching rows; a non-positive limit
// means unlimited. LIMIT 0 in PostgreSQL already means unlimited, but the CASE
// keeps the intent explicit for the -limit=0 default.
const pruneStaleEmptyExits = `DELETE FROM node_explorer_exits WHERE identity IN (
 SELECT e.identity FROM node_explorer_exits e WHERE ` + staleEmptyExit + `
 ORDER BY e.checked_at, e.identity
 LIMIT CASE WHEN $2 > 0 THEN $2 ELSE NULL END)`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// validate rejects arguments that would either delete everything or run an
// unintentionally broad window.
func validate(olderThan time.Duration, limit int) error {
	if olderThan <= 0 {
		return fmt.Errorf("older-than must be a positive duration")
	}
	if limit < 0 {
		return fmt.Errorf("limit must be 0 (unlimited) or positive")
	}
	return nil
}

func run() error {
	olderThan := flag.Duration("older-than", defaultOlderThan, "prune results checked before now minus this duration (e.g. 36h, 90m)")
	apply := flag.Bool("apply", false, "delete matching rows; default is a read-only count")
	limit := flag.Int("limit", 0, "maximum rows to delete in one run; 0 means unlimited")
	flag.Parse()
	if err := validate(*olderThan, *limit); err != nil {
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

	total, err := tableRows(ctx, db)
	if err != nil {
		return err
	}
	matched, err := matchedRows(ctx, db, *olderThan)
	if err != nil {
		return err
	}
	available, err := availableRows(ctx, db, *olderThan)
	if err != nil {
		return err
	}
	planned := matched
	if *limit > 0 && int64(*limit) < planned {
		planned = int64(*limit)
	}
	fmt.Printf("olderThan=%s limit=%d apply=%t total=%d matched=%d planned=%d registeredAvailable=%d\n", *olderThan, *limit, *apply, total, matched, planned, available)
	if available > 0 {
		fmt.Printf("note: %d matching rows belong to registered nodes marked available; these are probe results only and the next probe recreates them\n", available)
	}
	if !*apply {
		fmt.Printf("DRY RUN: no rows deleted; rerun with -apply\n")
		fmt.Printf("remaining=%d\n", total)
		return nil
	}
	deleted, err := prune(ctx, db, *olderThan, *limit)
	if err != nil {
		return err
	}
	remaining, err := tableRows(ctx, db)
	if err != nil {
		return err
	}
	fmt.Printf("deleted=%d remaining=%d\n", deleted, remaining)
	return nil
}

func tableRows(ctx context.Context, db *pgx.Conn) (int64, error) {
	var total int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM node_explorer_exits`).Scan(&total); err != nil {
		return 0, fmt.Errorf("count exit rows: %w", err)
	}
	return total, nil
}

func matchedRows(ctx context.Context, db *pgx.Conn, olderThan time.Duration) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM node_explorer_exits e WHERE `+staleEmptyExit, olderThan).Scan(&n); err != nil {
		return 0, fmt.Errorf("count stale empty exit rows: %w", err)
	}
	return n, nil
}

// availableRows only reports the scale of affected registered nodes; it never
// blocks pruning, because a missing exit row is rebuilt by the next probe.
func availableRows(ctx context.Context, db *pgx.Conn, olderThan time.Duration) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM node_explorer_exits e JOIN node_explorer_node_registry n USING(identity) WHERE `+staleEmptyExit+` AND n.available`, olderThan).Scan(&n); err != nil {
		return 0, fmt.Errorf("count available registered nodes: %w", err)
	}
	return n, nil
}

// prune deletes in a single transaction so a partial failure leaves the table
// unchanged.
func prune(ctx context.Context, db *pgx.Conn, olderThan time.Duration, limit int) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	tag, err := tx.Exec(ctx, pruneStaleEmptyExits, olderThan, limit)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
