package ingestion

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct{ Pool *pgxpool.Pool }

func (r *Repository) Healthy(ctx context.Context) error {
	var ok bool
	err := r.Pool.QueryRow(ctx, `SELECT updated_at>now()-interval '90 seconds' AND coalesce(payload->>'error','')='' AND coalesce(payload->>'sourceWarning','')='' AND payload->>'running'='true' FROM node_explorer_service_status WHERE service='source-ingest'`).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("source heartbeat stale or degraded")
	}
	return nil
}
