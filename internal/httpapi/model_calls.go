package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

func (a *API) modelQuery(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, false)
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

type projectContextKey struct{}

func (a *API) modelMCP() http.HandlerFunc {
	server := mcp.NewServer(&mcp.Implementation{Name: "simple-agent-tracer", Version: "1.0.0"}, nil)
	closedWorld := false
	schema, err := jsonschema.For[query.ModelQuery](nil)
	if err != nil {
		panic(fmt.Sprintf("model query tool schema: %v", err))
	}
	// RawMessage accepts any typed JSON value, including precise integer literals.
	schema.Properties["filters"].Properties["metadataValue"] = &jsonschema.Schema{
		Description: "Typed JSON value to match on metadataKey.",
	}
	schema.Properties["limit"].Minimum = new(float64(1))
	schema.Properties["limit"].Maximum = new(float64(50))
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"sql"}},
		{Required: []string{"from", "to"}},
	}
	server.AddTool(&mcp.Tool{
		Name:        "query_model_calls",
		Description: "Read retained model calls in this project, including costs and timings. Use from/to and filters, or a read-only SQL SELECT FROM model_calls. Select output_messages explicitly for captured model output. Follow nextCursor to read more rows.",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := &mcp.CallToolResult{}
		project, ok := ctx.Value(projectContextKey{}).(string)
		if !ok {
			result.SetError(errors.New("project authorization required"))
			return result, nil
		}
		var input query.ModelQuery
		decoder := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			result.SetError(errors.New("invalid query arguments"))
			return result, nil
		}
		ctx, cancel := context.WithTimeout(ctx, modelQueryTimeout)
		defer cancel()
		rows, err := input.Run(ctx, a.Store, project, 50, 256<<10)
		if err != nil {
			_, message := modelQueryError(ctx, err)
			result.SetError(errors.New(message))
			return result, nil
		}
		raw, err := json.Marshal(rows)
		if err != nil {
			return nil, err
		}
		result.StructuredContent = json.RawMessage(raw)
		result.Content = []mcp.Content{&mcp.TextContent{Text: string(raw)}}
		return result, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 64 << 10,
		// The access layer checks Host in local mode. Authenticated mode can
		// use a public hostname through a reverse proxy on loopback.
		DisableLocalhostProtection: true,
	})
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Access.AllowsMCPRequest(r) {
			failure(w, http.StatusForbidden, "invalid host or origin")
			return
		}
		p, ok := a.project(w, r, false)
		if !ok {
			return
		}
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), projectContextKey{}, p.ID)))
	}
}

func (a *API) modelExport(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r, false)
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
