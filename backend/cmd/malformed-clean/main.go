// Command malformed-clean removes only independently malformed, rejected nodes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cytusc/node_list/backend/internal/service"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	apply := flag.Bool("apply", false, "delete confirmed malformed configurations; default is preview")
	backup := flag.String("backup-reference", "", "reference to the completed database backup (required for apply)")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "database connection configuration failed")
		os.Exit(1)
	}
	defer pool.Close()
	report, err := service.CleanMalformedNodes(ctx, pool, *apply, *backup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup aborted (%T); transaction rolled back\n", err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
}
