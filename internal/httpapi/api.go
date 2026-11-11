package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/ingest"
	"github.com/pridkett/simple_agent_tracer/internal/query"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

type API struct {
	Store  *storage.Store
	Access *auth.Access
}

var spanFocus = regexp.MustCompile(`^[0-9a-f]{32}\.[0-9a-f]{16}$`)

func New(store *storage.Store, access *auth.Access, ui fs.FS, openapi []byte) http.Handler {
	a := &API{store, access}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
			if !strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "no-store")
			}
			next.ServeHTTP(w, r)
		})
	})
	r.Use(access.Middleware)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if store.Ready(ctx) != nil {
			failure(w, 503, "database unavailable")
			return
		}
		respond(w, 200, map[string]string{"status": "ready"})
	})
	r.Get("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openapi)
	})
	r.Get("/auth/status", access.Status)
	r.Get("/auth/login", access.Login)
	r.Get("/auth/callback", access.Callback)
	r.Post("/auth/logout", access.Logout)
	r.Post("/v1/traces", ingest.New(store, func(r *http.Request) (ingest.Identity, bool, error) {
		return access.Ingest(r, store)
	}).ServeHTTP)
	r.Route("/api/v1/projects", func(r chi.Router) {
		r.Get("/", a.projects)
		r.Post("/", a.createProject)
		r.Route("/{project}", func(r chi.Router) {
			r.Patch("/", a.patchProject)
			r.Get("/ingest-keys", a.listIngestKeys)
			r.Post("/ingest-keys", a.createIngestKey)
			r.Delete("/ingest-keys/{key}", a.revokeIngestKey)
			r.Post("/retention/keep", a.keep)
			r.Get("/metadata/keys", a.metadata(false))
			r.Get("/metadata/values", a.metadata(true))
			for _, kind := range []string{"traces", "sessions"} {
				r.Get("/"+kind, a.list(kind))
				r.Get("/"+kind+"/{id}", a.detail(kind))
				r.Get("/"+kind+"/{id}/export", a.export(kind))
			}
			r.Get("/traces/{id}/spans", a.spans("traces", false))
			r.Get("/traces/{id}/spans/{span}", a.span)
			r.Get("/sessions/{id}/timeline", a.spans("sessions", false))
			r.Get("/sessions/{id}/turns", a.turns)
			r.Get("/sessions/{id}/turns/{turn}", a.spans("sessions", true))
		})
	})
	if ui != nil {
		files := http.FileServer(http.FS(ui))
		r.Get("/assets/*", files.ServeHTTP)
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/v1/") {
				http.NotFound(w, r)
				return
			}
			index, err := fs.ReadFile(ui, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
		})
	}
	return r
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrInvalid):
		failure(w, 400, "invalid request")
	case errors.Is(err, sql.ErrNoRows):
		failure(w, 404, "resource not found")
	default:
		slog.Error("query failed", "errorType", fmt.Sprintf("%T", err))
		failure(w, 500, "database operation failed")
	}
}

func requestBody(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		failure(w, 400, "invalid JSON body")
		return false
	}
	return true
}

func (a *API) scope(w http.ResponseWriter, r *http.Request, mutate bool) (string, bool) {
	project, ok := a.Access.Query(r, mutate)
	if !ok {
		if mutate {
			failure(w, 403, "administrator authorization required")
		} else {
			failure(w, 401, "login or read credential required")
		}
	}
	return project, ok
}

func (a *API) project(w http.ResponseWriter, r *http.Request, mutate bool) (storage.Project, bool) {
	scope, ok := a.scope(w, r, mutate)
	if !ok {
		return storage.Project{}, false
	}
	return a.scopedProject(w, r, scope)
}

func (a *API) scopedProject(w http.ResponseWriter, r *http.Request, scope string) (storage.Project, bool) {
	p, err := a.Store.Project(r.Context(), chi.URLParam(r, "project"))
	if err == nil && scope != "" && scope != p.Slug && scope != p.ID {
		err = sql.ErrNoRows
	}
	if err != nil {
		storeError(w, err)
	}
	return p, err == nil
}

func (a *API) projects(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.scope(w, r, false)
	if !ok {
		return
	}
	if scope != "" {
		p, err := a.Store.Project(r.Context(), scope)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, 200, []storage.Project{p})
		return
	}
	projects, err := a.Store.Projects(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, projects)
}

type projectBody struct {
	Slug      string `json:"slug,omitempty"`
	Retention *int   `json:"retentionDays,omitempty"`
	Capture   *bool  `json:"captureContent,omitempty"`
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.scope(w, r, true); !ok {
		return
	}
	var body projectBody
	if !requestBody(w, r, &body) {
		return
	}
	days, capture := 30, true
	if body.Retention != nil {
		days = *body.Retention
	}
	if body.Capture != nil {
		capture = *body.Capture
	}
	p, err := a.Store.CreateProject(r.Context(), body.Slug, days, capture)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 201, p)
}

func (a *API) patchProject(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, true)
	if !ok {
		return
	}
	var body projectBody
	if !requestBody(w, r, &body) {
		return
	}
	if body.Slug != "" || (body.Retention == nil && body.Capture == nil) {
		storeError(w, storage.ErrInvalid)
		return
	}
	p, err := a.Store.UpdateProject(r.Context(), p, body.Retention, body.Capture)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, p)
}

type entity struct {
	storage.Entry
	LastActivity       string `json:"lastActivity,omitempty"`
	LastAccepted       string `json:"lastAccepted,omitempty"`
	SweepEligibleAfter string `json:"sweepEligibleAfter,omitempty"`
	query.Summary
}

func timestamp(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

func (a *API) summarize(ctx context.Context, project storage.Project, kind string, e storage.Entry) (entity, error) {
	accumulator := query.NewAccumulator()
	err := a.Store.Observations(ctx, project.ID, kind, e.ID, accumulator.Add)
	if e.Source == "" {
		e.Source = accumulator.SummarySource
	}
	result := entity{Entry: e, LastActivity: timestamp(e.Time), LastAccepted: timestamp(e.Received), Summary: accumulator.Summary}
	if kind == "traces" && e.Received > 0 {
		// AddDate supports the full configured retention range without duration overflow.
		retained := time.Unix(0, e.Time).UTC().AddDate(0, 0, project.Retention)
		grace := time.Unix(0, e.Received).Add(storage.Grace)
		if grace.After(retained) {
			retained = grace
		}
		result.SweepEligibleAfter = retained.UTC().Format(time.RFC3339Nano)
	}
	return result, err
}

func (a *API) list(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.project(w, r, false)
		if !ok {
			return
		}
		filters, err := query.Filters(r.URL.Query())
		if err != nil {
			storeError(w, err)
			return
		}
		limit, after, err := query.Pagination(r.URL.Query())
		if err != nil {
			storeError(w, err)
			return
		}
		entries, err := a.Store.Entries(r.Context(), p.ID, kind, filters, after, limit+1)
		if err != nil {
			storeError(w, err)
			return
		}
		page := query.Page[entity]{Items: []entity{}}
		if len(entries) > limit {
			last := entries[limit-1]
			page.Next = query.Cursor(storage.Position{Time: last.Time, ID: last.ID})
			entries = entries[:limit]
		}
		for _, e := range entries {
			row, err := a.summarize(r.Context(), p, kind, e)
			if err != nil {
				storeError(w, err)
				return
			}
			page.Items = append(page.Items, row)
		}
		respond(w, 200, page)
	}
}

func (a *API) entry(w http.ResponseWriter, r *http.Request, kind string) (storage.Project, storage.Entry, bool) {
	p, ok := a.project(w, r, false)
	if !ok {
		return p, storage.Entry{}, false
	}
	e, err := a.Store.Entry(r.Context(), p.ID, kind, chi.URLParam(r, "id"))
	if err != nil {
		storeError(w, err)
	}
	return p, e, err == nil
}

func (a *API) detail(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, e, ok := a.entry(w, r, kind)
		if !ok {
			return
		}
		result, err := a.summarize(r.Context(), p, kind, e)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, 200, result)
	}
}

func (a *API) span(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, false)
	if !ok {
		return
	}
	rows, err := a.Store.SpanRows(r.Context(), p.ID, "traces", chi.URLParam(r, "id"), "", chi.URLParam(r, "span"), nil, 1)
	if err == nil && len(rows) == 0 {
		err = sql.ErrNoRows
	}
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, 200, rows[0])
}

func (a *API) spans(kind string, turnDetail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, e, ok := a.entry(w, r, kind)
		if !ok {
			return
		}
		limit, after, err := query.Pagination(r.URL.Query())
		if err != nil {
			storeError(w, err)
			return
		}
		turn := chi.URLParam(r, "turn")
		focus := r.URL.Query().Get("focus")
		var rows []storage.SpanRow
		if focus != "" {
			if after != nil || !spanFocus.MatchString(focus) {
				storeError(w, storage.ErrInvalid)
				return
			}
			traceID, spanID, _ := strings.Cut(focus, ".")
			rows, err = a.Store.FocusedSpanRows(r.Context(), p.ID, kind, e.ID, turn, traceID, spanID, limit)
		} else {
			rows, err = a.Store.SpanRows(r.Context(), p.ID, kind, e.ID, turn, "", after, limit+1)
		}
		if err != nil {
			storeError(w, err)
			return
		}
		if turnDetail && after == nil && len(rows) == 0 {
			storeError(w, sql.ErrNoRows)
			return
		}
		page := struct {
			query.Page[storage.SpanRow]
			Parents    []storage.SpanRow          `json:"parents,omitempty"`
			Transcript *[]query.TranscriptMessage `json:"transcript,omitempty"`
		}{Page: query.Page[storage.SpanRow]{Items: rows}}
		if len(rows) > limit {
			page.Items = rows[:limit]
			page.Next = query.Cursor(rows[limit-1].Position)
		}
		if r.URL.Query().Get("includeParents") == "true" {
			page.Parents, err = a.Store.SpanParents(r.Context(), p.ID, page.Items)
			if err != nil {
				storeError(w, err)
				return
			}
		}
		if kind == "traces" {
			respond(w, 200, page)
			return
		}
		transcript := query.NewTranscript()
		if (after != nil || focus != "") && len(page.Items) > 0 {
			priorPosition := &page.Items[0].Position
			prior, err := a.Store.PrecedingModel(r.Context(), p.ID, e.ID, turn, priorPosition)
			if err != nil {
				storeError(w, err)
				return
			}
			for _, row := range prior {
				if err := transcript.Add(row); err != nil {
					storeError(w, err)
					return
				}
			}
			transcript.Messages = transcript.Messages[:0]
		}
		for _, row := range page.Items {
			if err := transcript.Add(row); err != nil {
				storeError(w, err)
				return
			}
		}
		page.Transcript = &transcript.Messages
		respond(w, 200, page)
	}
}

type turn struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Start  string `json:"start"`
}

func (a *API) turns(w http.ResponseWriter, r *http.Request) {
	p, e, ok := a.entry(w, r, "sessions")
	if !ok {
		return
	}
	limit, after, err := query.Pagination(r.URL.Query())
	if err != nil {
		storeError(w, err)
		return
	}
	rows, err := a.Store.Turns(r.Context(), p.ID, e.ID, after, limit+1)
	if err != nil {
		storeError(w, err)
		return
	}
	page := query.Page[turn]{Items: []turn{}}
	if len(rows) > limit {
		last := rows[limit-1]
		page.Next = query.Cursor(storage.Position{Time: last.Time, ID: last.ID})
		rows = rows[:limit]
	}
	for _, row := range rows {
		page.Items = append(page.Items, turn{row.ID, row.Number, timestamp(row.Time)})
	}
	respond(w, 200, page)
}

func (a *API) metadata(values bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.project(w, r, false)
		if !ok {
			return
		}
		key := ""
		if values {
			key = r.URL.Query().Get("key")
			if key == "" || len(key) > 256 || strings.ContainsRune(key, 0) {
				storeError(w, storage.ErrInvalid)
				return
			}
		}
		limit, _, err := query.Pagination(r.URL.Query())
		if err != nil {
			storeError(w, err)
			return
		}
		result, err := a.Store.Metadata(r.Context(), p.ID, key, limit)
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, 200, result)
	}
}

func (a *API) keep(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Traces   []string `json:"traceIds"`
		Sessions []string `json:"sessionIds"`
		Keep     *bool    `json:"keep"`
	}
	if !requestBody(w, r, &body) {
		return
	}
	if body.Keep == nil {
		storeError(w, storage.ErrInvalid)
		return
	}
	if err := a.Store.Keep(r.Context(), p.ID, body.Traces, body.Sessions, *body.Keep); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) export(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, e, ok := a.entry(w, r, kind)
		if !ok {
			return
		}
		result, err := a.summarize(r.Context(), p, kind, e)
		if err != nil {
			storeError(w, err)
			return
		}
		header, err := json.Marshal(result)
		if err != nil {
			storeError(w, err)
			return
		}
		rows, err := a.Store.SpanRows(r.Context(), p.ID, kind, e.ID, "", "", nil, 16)
		if err != nil {
			storeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="tracer-`+e.ID+`.json"`)
		w.Header().Set("Trailer", "X-Export-Status")
		if _, err = fmt.Fprintf(w, `{"version":1,"project":%q,"kind":%q,"summary":%s,"spans":[`, p.Slug, kind, header); err != nil {
			return
		}
		count := 0
		for len(rows) > 0 {
			for _, row := range rows {
				raw, err := json.Marshal(row)
				if err != nil {
					return
				}
				if count > 0 {
					if _, err = io.WriteString(w, ","); err != nil {
						return
					}
				}
				if _, err = w.Write(raw); err != nil {
					return
				}
				count++
			}
			last := rows[len(rows)-1].Position
			rows, err = a.Store.SpanRows(r.Context(), p.ID, kind, e.ID, "", "", &last, 16)
			if err != nil {
				w.Header().Set("X-Export-Status", "failed")
				return
			}
		}
		if _, err = io.WriteString(w, "]}\n"); err == nil {
			w.Header().Set("X-Export-Status", "complete")
		}
	}
}
