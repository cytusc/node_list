// Package testutil contains isolated integration fixtures used by backend tests.
package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres creates a dedicated schema only when a test database is explicitly
// supplied. Standard deployment PG* variables never enable integration tests.
func Postgres(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv("NODE_EXPLORER_TEST_PG")
	if dsn == "" {
		t.Skip("set NODE_EXPLORER_TEST_PG to an isolated PostgreSQL database")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("go_regression_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup schema: %v", err)
		}
		admin.Close()
	})
	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	return admin.Config().ConnConfig.Copy()
}
