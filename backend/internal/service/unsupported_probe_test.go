package service

import (
	"context"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/transport"
	"github.com/jackc/pgx/v5"
)

func TestUnsupportedShareLinkPersistsProbeResultAndDoesNotImmediatelyRequeue(t *testing.T) {
	for _, protocol := range []string{"http", "ss"} {
		for _, kind := range []string{KindLive, KindExit} {
			t.Run(protocol+"/"+kind, func(t *testing.T) {
				f := setup(t)
				ctx := context.Background()
				f.exec(t, `UPDATE node_explorer_node_registry SET node=(node-'rawConfig') || jsonb_build_object('rawLink','ss://invalid','type',$2::text) WHERE identity=$1`, f.id, protocol)
				router := transport.NewRouter(transport.NewNativeEngine()).WithFallback(transport.NewMihomoEngine([]transport.MihomoConfig{{Controller: "http://127.0.0.1:1", ProxyURL: "http://127.0.0.1:1"}}, time.Second))
				f.runner.Config.HighRouter, f.runner.Config.NormalRouter = router, router
				f.runner.Config.Role = "connectivity"
				if kind == KindExit {
					f.runner.Config.Role = "ip"
				}
				if err := f.repo.FinishDependencies(ctx, func(tx pgx.Tx) error {
					return once(ctx, tx, kind, f.id, Payload{Identity: f.id}, 1)
				}); err != nil {
					t.Fatal(err)
				}
				job := f.claim(t, kind)
				if err := f.runner.handle(ctx, job, true); err != nil {
					t.Fatalf("unsupported share link was retried: %v", err)
				}
				var result string
				table := "node_explorer_connectivity"
				if kind == KindExit {
					table = "node_explorer_exits"
				}
				if err := f.repo.Pool.QueryRow(ctx, "SELECT payload->>'error' FROM "+table+" WHERE identity=$1", f.id).Scan(&result); err != nil || result != "连接配置不支持" {
					t.Fatalf("permanent failure not recorded: %q %v", result, err)
				}
				if err := f.runner.schedule(ctx); err != nil {
					t.Fatal(err)
				}
				var future bool
				if err := f.repo.Pool.QueryRow(ctx, `SELECT due_at>now()+interval '20 minutes' FROM node_explorer_work WHERE kind=$1 AND key=$2`, kind, f.id).Scan(&future); err != nil || !future {
					t.Fatalf("unsupported node immediately requeued: %v %v", future, err)
				}
			})
		}
	}
}
