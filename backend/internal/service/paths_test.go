package service

import (
	"context"
	"net/http"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

type testEngine struct{ client *http.Client }

func (e testEngine) Name() string         { return "test" }
func (e testEngine) Supports(string) bool { return true }
func (e testEngine) Reserve(ctx context.Context, nodes []domain.Node) (map[int]*transport.Outbound, func(), error) {
	m := map[int]*transport.Outbound{}
	for i := range nodes {
		m[i] = &transport.Outbound{Client: e.client}
	}
	return m, func() {}, nil
}

// Test-only transaction helper; production workers commit facts through leases.
func (r *Repository) FinishDependencies(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
