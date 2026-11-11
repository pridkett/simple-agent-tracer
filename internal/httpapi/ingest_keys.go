package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

func (a *API) listIngestKeys(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.scope(w, r, false)
	if !ok {
		return
	}
	if scope != "" {
		failure(w, 403, "administrator authorization required")
		return
	}
	p, ok := a.scopedProject(w, r, scope)
	if !ok {
		return
	}
	keys, err := a.Store.IngestKeys(r.Context(), p.ID)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, keys)
}

func (a *API) createIngestKey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	if !requestBody(w, r, &body) {
		return
	}
	key, token, err := a.Store.CreateIngestKey(r.Context(), p.ID, body.Source)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 201, struct {
		storage.IngestKey
		Token string `json:"token"`
	}{key, token})
}

func (a *API) revokeIngestKey(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, true)
	if !ok {
		return
	}
	if err := a.Store.RevokeIngestKey(r.Context(), p.ID, chi.URLParam(r, "key")); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
