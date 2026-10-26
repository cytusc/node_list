package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Archive before batch deduplication, not just during registry publication:
// two deliveries of one observation can carry different original URI spellings.
// Both are provenance, and neither may disappear behind ON CONFLICT DO NOTHING.
func archiveNodeLinks(ctx context.Context, tx pgx.Tx, payload []byte) error {
	var snap domain.Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, n := range snap.Nodes {
		key := domain.ConnectionKey(n)
		if key == "" {
			key = "identity:" + domain.AggregateIdentity(n)
		}
		for origin, link := range map[string]string{"original": n.RawLink, "generated": n.GeneratedLink} {
			if link == "" {
				continue
			}
			id := fmt.Sprintf("%x", sha256.Sum256([]byte(key+"\x00"+origin+"\x00"+link)))
			batch.Queue(`INSERT INTO node_explorer_node_links(id,connection_key,link,origin) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO NOTHING`, id, key, link, origin)
		}
	}
	if batch.Len() == 0 {
		return nil
	}
	return tx.SendBatch(ctx, batch).Close()
}
