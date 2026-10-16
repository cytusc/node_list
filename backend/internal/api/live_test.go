package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cytusc/node_list/backend/internal/store"
	"github.com/cytusc/node_list/backend/internal/store/postgres"
	"github.com/cytusc/node_list/backend/internal/testutil"
)

func TestCurrentAPIHasNoArchiveReadOrCredentialExposure(t *testing.T) {
	cfg := testutil.Postgres(t)
	ctx := context.Background()
	a, err := postgres.NewAdapter(ctx, postgres.Config{Host: cfg.Host, Port: int(cfg.Port), User: cfg.User, Database: cfg.Database, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Insert(ctx, "invalid", []byte("invalid snapshot JSON"), "", 1); err != nil {
		t.Fatal(err)
	}
	_, err = a.Pool().Exec(ctx, `INSERT INTO node_explorer_node_registry(identity,node,snapshot_id,node_index) VALUES('one','{"name":"live","rawLink":"ss://private","rawConfig":{"password":"private"}}',1,0)`)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(ServerConfig{Store: store.New(a, nil), Live: postgres.NewLiveReader(a.Pool())}).Handler()
	for _, path := range []string{"/api/results?page=1&limit=50", "/api/results", "/api/results?view=aggregate&page=1&limit=50"} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest("GET", path, nil))
		if res.Code != 200 {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body)
		}
		for _, secret := range []string{"rawLink", "rawConfig", "private"} {
			if strings.Contains(res.Body.String(), secret) {
				t.Fatalf("%s leaked %s", path, secret)
			}
		}
		if !strings.Contains(res.Body.String(), `"name":"live"`) {
			t.Fatalf("missing current node: %s", res.Body)
		}
	}
	for path, want := range map[string]int{"/api/history": 404, "/api/results?snapshot=1": 400, "/api/results?page=-1": 400} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest("GET", path, nil))
		if res.Code != want {
			t.Fatalf("%s: %d", path, res.Code)
		}
	}
}
