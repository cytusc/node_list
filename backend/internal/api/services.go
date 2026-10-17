package api

import (
	"context"
	"github.com/cytusc/node_list/backend/internal/service"
	"net/http"
)

// Statuses is the read side of the service board. The repository satisfies it
// directly; callers that serve pollers may wrap it with a short-lived cache.
type Statuses interface {
	Statuses(ctx context.Context) ([]service.Status, error)
}

func WriteServices(w http.ResponseWriter, r *http.Request, repo Statuses) {
	w.Header().Set("Cache-Control", "no-store")
	status, err := repo.Statuses(r.Context())
	if err != nil {
		writeUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": status})
}
