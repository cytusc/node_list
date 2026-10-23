package service

import (
	"context"
	"time"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

// RejectConfirmedFaults prevents repeated imports of the same automatically
// retired connection, including equivalent share-link/Clash variants. The
// original registry/source row remains available to scheduled recovery. A
// restored connection or changed configuration is eligible on the next import.
// Neither a lone exit failure nor a policy/manual retirement is evidence here.
func (r *Repository) RejectConfirmedFaults(ctx context.Context, nodes []domain.Node) (map[int]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys := make([]string, 0, len(nodes))
	positions := make(map[string][]int, len(nodes))
	for index, node := range nodes {
		key := domain.ConnectionKey(node)
		if key == "" {
			continue
		}
		if _, exists := positions[key]; !exists {
			keys = append(keys, key)
		}
		positions[key] = append(positions[key], index)
	}
	denied := make(map[int]string)
	// Bound SQL array/response size without one query per candidate. No
	// network probes or shared-kernel reloads run in the ingestion critical path.
	for start := 0; start < len(keys); start += 1000 {
		rows, err := r.Pool.Query(ctx, `SELECT DISTINCT n.connection_key
 FROM node_explorer_node_registry n JOIN node_explorer_retired_nodes t USING(identity)
 WHERE n.connection_key=ANY($1::text[]) AND n.connection_key<>'' AND n.available AND t.reason=$2
 AND EXISTS(SELECT 1 FROM node_explorer_snapshots s WHERE s.id=n.snapshot_id)`, keys[start:min(start+1000, len(keys))], store.RetireRestoreReason)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			for _, index := range positions[key] {
				denied[index] = "confirmed-retired-connection"
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return denied, nil
}
