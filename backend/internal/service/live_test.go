package service

import (
	"context"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
)

func TestLiveQueriesObserveFactsWithoutProjectionRefresh(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	reader := postgres.NewLiveReader(f.repo.Pool)
	q := &postgres.Query{Page: 1, Limit: 1, Sort: "name", Direction: "asc"}
	page, err := reader.QueryPage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Nodes) != 1 {
		t.Fatalf("initial page: %+v", page)
	}
	var before, after int64
	if err = f.repo.Pool.QueryRow(ctx, `SELECT source_revision FROM node_explorer_results_state`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.evidence(t, "")
	if err = f.repo.Pool.QueryRow(ctx, `SELECT source_revision FROM node_explorer_results_state`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("runtime observations dirtied source revision: %d -> %d", before, after)
	}
	page, err = reader.QueryPage(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if page.Nodes[0].Exit == nil || page.Nodes[0].Exit.IP != "8.8.4.4" {
		t.Fatal("new exit was not immediately visible")
	}
	// Runtime changes do not require an archive fold or invalidate the source registry.
	if err = f.repo.SyncRegistry(ctx, f.st); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE node_explorer_node_registry SET node=node || '{"country":"JP","name":"节点 10","baseName":"节点 10","network":"tcp","tls":true,"speed":2048,"mediaCheck":true,"media":[{"platform":"netflix","tag":"yes"}]}'::jsonb WHERE identity=$1`, f.id)
	f.exec(t, `INSERT INTO node_explorer_ip_attributes(ip,checked_at,payload) VALUES('8.8.4.4',now(),'{"ip":"8.8.4.4","status":"success","country":"US","score":12,"source":"ippure","humanPercent":90,"attributes":["住宅","住宅"],"classification":{"state":"residential"}}') ON CONFLICT(ip) DO UPDATE SET payload=EXCLUDED.payload`)
	for _, query := range []*postgres.Query{
		{Page: 99, Limit: 1, Sort: "preferred"},
		{Page: 1, Limit: 50, Search: "美国", Sort: "country", Direction: "asc"},
		{Page: 1, Limit: 50, Countries: []string{"US"}, Types: []string{"http"}, Networks: []string{"tcp"}, Features: []string{"tls"}, IPSources: []string{"ippure"}, IPAttributes: []string{"住宅"}, Media: []string{"netflix"}, MinSpeed: 1, Sort: "trafficRatio", Direction: "desc"},
	} {
		page, err = reader.QueryPage(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 1 || len(page.Nodes) != 1 || page.Page != 1 {
			t.Fatalf("query %+v: %+v", query, page)
		}
		node := page.Nodes[0]
		if node.Country != "US" || node.ClaimedCountry != "JP" || node.RawLink != "" || len(node.RawConfig) > 0 {
			t.Fatalf("bad node: %+v", node)
		}
		if len(page.Facets.IPAttributes) != 1 || page.Facets.IPAttributes[0].Count != 1 {
			t.Fatalf("attribute counts: %+v", page.Facets.IPAttributes)
		}
	}
	for _, sort := range domain.ResultSortKeys {
		q.Sort = sort
		if _, err = reader.QueryPage(ctx, q); err != nil {
			t.Fatalf("sort %s: %v", sort, err)
		}
	}
	f.exec(t, `INSERT INTO node_explorer_retired_nodes(identity,retired_at,reason) VALUES($1,'now','manual')`, f.id)
	page, err = reader.QueryPage(ctx, q)
	if err != nil || page.Total != 0 {
		t.Fatalf("retirement invisible: %+v %v", page, err)
	}
	f.exec(t, `DELETE FROM node_explorer_retired_nodes WHERE identity=$1`, f.id)
	page, err = reader.QueryPage(ctx, q)
	if err != nil || page.Total != 1 {
		t.Fatalf("restore invisible: %+v %v", page, err)
	}
	// A corrupt archive is deliberately unreadable as a snapshot. Live queries
	// use the registry and must not load any archive payload to return a page.
	f.exec(t, `UPDATE node_explorer_snapshots SET payload='invalid snapshot JSON'`)
	page, err = reader.QueryPage(ctx, q)
	if err != nil || page.Total != 1 {
		t.Fatalf("query read archive: %+v %v", page, err)
	}
}

func TestLivePagingUsesCurrentCountsAndNaturalSort(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	reader := postgres.NewLiveReader(f.repo.Pool)
	f.exec(t, `UPDATE node_explorer_node_registry SET node=node || '{"name":"节点 10","baseName":"节点 10","speed":1024,"speedTest":true}'::jsonb`)
	f.exec(t, `INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index)
 SELECT 'second',node || '{"name":"节点 2","baseName":"节点 2","speed":2048}'::jsonb,snapshot_id,1 FROM node_explorer_node_registry WHERE identity=$1`, f.id)
	f.exec(t, `INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index)
 SELECT 'third',node || '{"name":"节点 3","baseName":"节点 3","speed":3072}'::jsonb,snapshot_id,2 FROM node_explorer_node_registry WHERE identity=$1`, f.id)
	for _, c := range []struct {
		q    *postgres.Query
		name string
		page int
	}{
		{&postgres.Query{Page: 1, Limit: 1, Sort: "name", Direction: "asc"}, "节点 2", 1},
		{&postgres.Query{Page: 99, Limit: 1, Sort: "name", Direction: "asc"}, "节点 10", 3},
		{&postgres.Query{Page: 1, Limit: 1, Sort: "preferred"}, "节点 3", 1},
		{&postgres.Query{Page: 1, Limit: 1, Sort: "speed", Direction: "asc"}, "节点 10", 1},
	} {
		page, err := reader.QueryPage(ctx, c.q)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 3 || page.TotalNodes != 3 || len(page.Nodes) != 1 || page.Nodes[0].Name != c.name || page.Page != c.page {
			t.Fatalf("query %+v: %+v", c.q, page)
		}
	}
	f.exec(t, `UPDATE node_explorer_node_registry SET node=node || '{"speedTest":false,"media":null}'::jsonb`)
	page, err := reader.QueryPage(ctx, &postgres.Query{Page: 1, Limit: 1, Sort: "speed", Direction: "desc", MinSpeed: 1000})
	if err != nil || len(page.Nodes) != 1 || page.Nodes[0].Name != "节点 2" || page.Total != 3 {
		t.Fatalf("disabled speed fallback: %+v %v", page, err)
	}
	page, err = reader.QueryPage(ctx, nil)
	if err != nil || len(page.Nodes) != 3 || page.Total != 3 {
		t.Fatalf("all current nodes: %+v %v", page, err)
	}
}
