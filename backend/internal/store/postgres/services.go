package postgres

import (
	"context"
	_ "embed"
	"encoding/json"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

//go:embed services.sql
var serviceSchema string

func createServicesSchema(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, serviceSchema); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, liveSchema)
	return err
}

func queryRuntime(ctx context.Context, tx pgx.Tx) (map[string]domain.NodeState, error) {
	rows, err := tx.Query(ctx, `SELECT n.identity,c.payload,e.payload,e.version,a.payload,r.retired_at,r.reason
	 FROM node_explorer_node_registry n
	 LEFT JOIN node_explorer_connectivity c USING(identity)
	 LEFT JOIN node_explorer_exits e USING(identity)
	 LEFT JOIN node_explorer_ip_attributes a ON a.ip=e.ip
	 LEFT JOIN node_explorer_retired_nodes r USING(identity)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]domain.NodeState{}
	for rows.Next() {
		var id string
		var c, e, a []byte
		var version *int64
		var retired, reason *string
		if err := rows.Scan(&id, &c, &e, &version, &a, &retired, &reason); err != nil {
			return nil, err
		}
		s := domain.NodeState{Lifecycle: &domain.LifecycleState{Status: "active"}}
		if retired != nil {
			s.Lifecycle = &domain.LifecycleState{Status: "retired", RetiredAt: *retired, Reason: *reason}
		}
		if len(c) > 0 {
			s.Connectivity = &domain.ConnectivityState{}
			if err := json.Unmarshal(c, s.Connectivity); err != nil {
				return nil, err
			}
		}
		if len(e) > 0 {
			s.Exit = &domain.ExitState{}
			if err := json.Unmarshal(e, s.Exit); err != nil {
				return nil, err
			}
			s.Exit.Version = *version
		}
		if len(a) > 0 {
			s.Attributes = &domain.IPPureResult{}
			if err := json.Unmarshal(a, s.Attributes); err != nil {
				return nil, err
			}
		}
		out[id] = s
	}
	return out, rows.Err()
}
