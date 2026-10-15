package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cytusc/node_list/backend/internal/store"
	pg "github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/testutil"
)

func TestPostgresAggregateStripsCredentials(t *testing.T) {
	cfg := testutil.Postgres(t)
	ctx := context.Background()
	adapter, err := pg.NewAdapter(ctx, pg.Config{Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database, User: cfg.User, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	batch := batchOne()
	batch.CheckedAt = time.Now().UTC().Format(time.RFC3339Nano)
	batch.Nodes[1].RawConfig["password"] = "projection-secret"
	raw, _ := json.Marshal(batch)
	if err := adapter.Insert(ctx, "one", raw, batch.CheckedAt, len(batch.Nodes)); err != nil {
		t.Fatal(err)
	}
	st := store.New(adapter, nil)
	if err := st.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	projector, err := pg.NewProjectorForAdapter(ctx, adapter, st, pg.ProjectorConfig{RetentionDays: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := projector.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	handler := New(ServerConfig{Store: st, Projector: projector, HasPostgres: true}).Handler()
	for _, path := range []string{"/api/results?view=aggregate&page=1&limit=50", "/api/results?page=1&limit=50", "/api/results?view=aggregate", "/api/results?snapshot=1"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != 200 {
			t.Fatalf("%s returned %d: %s", path, response.Code, response.Body)
		}
		body := response.Body.String()
		for _, secret := range []string{"rawLink", "rawConfig", "projection-secret", "ss://"} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaked credentials", path)
			}
		}
	}
	// Public filtering must leave internal credentials available for future probes.
	snap, _ := st.Get(nil)
	if snap.Nodes[0].RawLink == "" || snap.Nodes[1].RawConfig["password"] != "projection-secret" {
		t.Fatal("public response modified stored credentials")
	}
}
