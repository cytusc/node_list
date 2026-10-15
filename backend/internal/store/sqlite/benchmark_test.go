package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store"
)

func BenchmarkStoreRefresh(b *testing.B) {
	ctx := context.Background()
	adapter, err := New(filepath.Join(b.TempDir(), "db.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer adapter.Close()
	for batch := range 100 {
		nodes := make([]domain.Node, 50)
		for i := range nodes {
			nodes[i] = domain.Node{Name: fmt.Sprintf("node-%d-%d", batch, i), Type: "ss", Server: "192.0.2.1", Port: "443", RawConfig: domain.ProxyConfig{"type": "ss", "server": "192.0.2.1", "port": 443, "password": fmt.Sprintf("%d-%d", batch, i)}}
		}
		raw, _ := json.Marshal(domain.Snapshot{Nodes: nodes})
		if err := adapter.Insert(ctx, fmt.Sprint(batch), raw, "", len(nodes)); err != nil {
			b.Fatal(err)
		}
	}
	st := store.New(adapter, nil)
	if err := st.Reload(ctx); err != nil {
		b.Fatal(err)
	}
	b.Run("IdleRevision", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := st.Sync(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("FullReload", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := st.Reload(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
