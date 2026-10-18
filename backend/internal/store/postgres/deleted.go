package postgres

import (
	"context"
	"encoding/json"
	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
	"time"
)

// A watermark rejects stale source observations without permanently blacklisting
// a connection that a future source batch independently rediscovers.
func filterDeletedSnapshot(ctx context.Context, tx pgx.Tx, raw []byte, count int) ([]byte, int, error) {
	var payload map[string]json.RawMessage
	if e := json.Unmarshal(raw, &payload); e != nil {
		return nil, 0, e
	}
	var nodes []json.RawMessage
	if e := json.Unmarshal(payload["nodes"], &nodes); e != nil {
		return nil, 0, e
	}
	ids := make([]string, len(nodes))
	for i, b := range nodes {
		var n domain.Node
		if e := json.Unmarshal(b, &n); e != nil {
			return nil, 0, e
		}
		ids[i] = domain.AggregateIdentity(n)
	}
	rows, e := tx.Query(ctx, `SELECT identity,deleted_at FROM node_explorer_deleted_nodes WHERE identity=ANY($1::text[])`, ids)
	if e != nil {
		return nil, 0, e
	}
	deleted := map[string]time.Time{}
	for rows.Next() {
		var id string
		var stamp time.Time
		if e = rows.Scan(&id, &stamp); e != nil {
			rows.Close()
			return nil, 0, e
		}
		deleted[id] = stamp
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return nil, 0, e
	}
	if len(deleted) == 0 {
		return raw, count, nil
	}
	var checked string
	_ = json.Unmarshal(payload["checkedAt"], &checked)
	stamp, _ := time.Parse(time.RFC3339Nano, checked)
	kept := make([]json.RawMessage, 0, len(nodes))
	for i, node := range nodes {
		if watermark, ok := deleted[ids[i]]; !ok || stamp.Truncate(time.Microsecond).After(watermark) {
			kept = append(kept, node)
		}
	}
	payload["nodes"], _ = json.Marshal(kept)
	raw, e = json.Marshal(payload)
	return raw, len(kept), e
}
