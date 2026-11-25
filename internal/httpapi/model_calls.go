package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/query"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

const modelQueryTimeout = 15 * time.Second

func modelQueryError(ctx context.Context, err error) (int, string) {
	switch {
	case errors.Is(err, storage.ErrInvalid):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, query.ErrQueryBudget):
		return http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "query timed out; narrow the filters or time window"
	default:
		slog.Error("model call query failed", "errorType", fmt.Sprintf("%T", err))
		return http.StatusInternalServerError, "query failed"
	}
}

func (a *API) queryProject(w http.ResponseWriter, r *http.Request) (storage.Project, bool) {
	if scope, ok := a.Access.Query(r, false); ok {
		return a.scopedProject(w, r, scope)
	}
	project := chi.URLParam(r, "project")
	if _, ok := a.queryOAuthSubject(w, r, project); !ok {
		return storage.Project{}, false
	}
	return a.oauthProject(w, r, project)
}

func (a *API) queryOAuthSubject(w http.ResponseWriter, r *http.Request, project string) (string, bool) {
	metadata, metadataURL, enabled := a.Access.QueryResource(project)
	if !enabled {
		failure(w, http.StatusUnauthorized, "login or read credential required")
		return "", false
	}
	subject, status := a.Access.CheckQueryOAuth(r, metadata.Resource)
	if status == http.StatusOK {
		return subject, true
	}
	challenge := fmt.Sprintf("Bearer resource_metadata=%q, scope=%q", metadataURL, auth.QueryReadScope)
	message := "OAuth access token or project read credential required"
	if status == http.StatusForbidden {
		challenge += `, error="insufficient_scope"`
		message = "OAuth access token lacks " + auth.QueryReadScope
	} else if r.Header.Get("Authorization") != "" {
		challenge += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	failure(w, status, message)
	return "", false
}

func (a *API) oauthProject(w http.ResponseWriter, r *http.Request, slug string) (storage.Project, bool) {
	p, ok := a.scopedProject(w, r, slug)
	if ok && p.Slug != slug {
		failure(w, http.StatusNotFound, "resource not found")
		return storage.Project{}, false
	}
	return p, ok
}

func (a *API) queryResourceMetadata(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "project")
	metadata, _, enabled := a.Access.QueryResource(slug)
	if !enabled {
		http.NotFound(w, r)
		return
	}
	if slug != "" {
		if _, ok := a.oauthProject(w, r, slug); !ok {
			return
		}
	}
	respond(w, http.StatusOK, metadata)
}

func (a *API) modelQuery(w http.ResponseWriter, r *http.Request) {
	p, ok := a.queryProject(w, r)
	if !ok {
		return
	}
	var input query.ModelQuery
	if !requestBody(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelQueryTimeout)
	defer cancel()
	result, err := input.Run(ctx, a.Store, p.ID, 200, 0)
	if err != nil {
		status, message := modelQueryError(ctx, err)
		failure(w, status, message)
		return
	}
	respond(w, http.StatusOK, result)
}

func (a *API) modelExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.queryProject(w, r)
	if !ok {
		return
	}
	var input query.ModelQuery
	if !requestBody(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelQueryTimeout)
	defer cancel()
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(modelQueryTimeout + 5*time.Second)); err == nil {
		defer controller.SetWriteDeadline(time.Time{})
	}
	type record struct {
		Type       string `json:"type"`
		Row        any    `json:"row,omitempty"`
		NextCursor string `json:"nextCursor,omitempty"`
		Error      string `json:"error,omitempty"`
	}
	started := false
	writeFailed := false
	encoder := json.NewEncoder(w)
	write := func(item record) error {
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			started = true
		}
		err := encoder.Encode(item)
		writeFailed = err != nil
		return err
	}
	next, err := input.Export(ctx, a.Store, p.ID, func(row any) error {
		return write(record{Type: "row", Row: row})
	})
	if writeFailed || r.Context().Err() != nil {
		return
	}
	if err != nil {
		status, message := modelQueryError(ctx, err)
		if !started {
			failure(w, status, message)
			return
		}
		_ = write(record{Type: "error", Error: message})
		return
	}
	_ = write(record{Type: "complete", NextCursor: next})
}
