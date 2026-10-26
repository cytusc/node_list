package service

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

func validatePaths(ctx context.Context, tx pgx.Tx, paths map[string]*transport.Outbound) error {
	ids := make([]string, 0, len(paths))
	for id := range paths {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		path := paths[id]
		if path == nil || path.Validate == nil {
			continue
		}
		if err := path.Validate(ctx); err != nil {
			return err
		}
		var raw []byte
		var incarnation string
		err := tx.QueryRow(ctx, `SELECT node,incarnation FROM node_explorer_node_registry WHERE identity=$1 FOR UPDATE`, id).Scan(&raw, &incarnation)
		if err != nil {
			return err
		}
		var entry transport.RegistryEntry
		if err = json.Unmarshal(raw, &entry.Node); err != nil {
			return err
		}
		if incarnation != path.Incarnation || transport.ConfigDigest(entry.Node) != path.Digest {
			return ErrLeaseLost
		}
	}
	return nil
}

// Leave a second for cancellation to reach PostgreSQL before a lease expires.
func pathContext(ctx context.Context, paths map[string]*transport.Outbound) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(2 * time.Minute)
	for _, p := range paths {
		if p != nil && !p.ExpiresAt.IsZero() && p.ExpiresAt.Add(-time.Second).Before(deadline) {
			deadline = p.ExpiresAt.Add(-time.Second)
		}
	}
	return context.WithDeadline(ctx, deadline)
}
