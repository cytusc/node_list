package api

import (
	"github.com/cytusc/node_list/backend/internal/service"
	"net/http"
)

func WriteServices(w http.ResponseWriter, r *http.Request, repo *service.Repository) {
	w.Header().Set("Cache-Control", "no-store")
	status, err := repo.Statuses(r.Context())
	if err != nil {
		writeUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": status})
}
