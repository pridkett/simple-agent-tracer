package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/query"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

type mcpAccessKey struct{}

// Each HTTP request gets either a fixed project from its route or the caller's
// shared-endpoint grants. An empty grant list permits no projects.
type mcpAccess struct {
	fixedProjectID string
	grants         []string
}

func (access mcpAccess) allows(p storage.Project) bool {
	return slices.Contains(access.grants, "*") || slices.Contains(access.grants, p.Slug) || slices.Contains(access.grants, p.ID)
}

func (access mcpAccess) resolveProject(ctx context.Context, store *storage.Store, slug string) (string, error) {
	if access.fixedProjectID != "" {
		return access.fixedProjectID, nil
	}
	if slug == "" {
		return "", errors.New("project is required; choose a slug from list_projects")
	}
	p, err := store.Project(ctx, slug)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("unable to load project")
	}
	if err != nil || p.Slug != slug || !access.allows(p) {
		return "", errors.New("project unavailable")
	}
	return p.ID, nil
}

func (a *API) mcpProjectAccess(w http.ResponseWriter, r *http.Request, shared bool) (mcpAccess, bool) {
	if !shared {
		p, ok := a.queryProject(w, r)
		return mcpAccess{fixedProjectID: p.ID}, ok
	}
	if scope, ok := a.Access.Query(r, false); ok {
		if scope == "" {
			scope = "*" // Local or signed-in browser administrator.
		}
		return mcpAccess{grants: []string{scope}}, true
	}
	subject, ok := a.queryOAuthSubject(w, r, "")
	if !ok {
		return mcpAccess{}, false
	}
	return mcpAccess{grants: a.Access.SharedQueryProjects(subject)}, true
}

type projectList struct {
	Projects []string `json:"projects" jsonschema:"Project slugs available to query."`
}

func (a *API) listMCPProjects(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, projectList, error) {
	access, _ := ctx.Value(mcpAccessKey{}).(mcpAccess)
	projects, err := a.Store.Projects(ctx)
	if err != nil {
		return nil, projectList{}, errors.New("unable to list projects")
	}
	out := projectList{Projects: []string{}}
	for _, p := range projects {
		if access.allows(p) {
			out.Projects = append(out.Projects, p.Slug)
		}
	}
	return nil, out, nil
}

func (a *API) modelMCP(shared bool) http.HandlerFunc {
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
	tool := &mcp.Tool{
		Name:        "query_model_calls",
		Description: "Read retained model calls in this project, including costs and timings. Use from/to and filters, or a read-only SQL SELECT FROM model_calls. Select output_messages explicitly for captured model output. Follow nextCursor to read more rows.",
		InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
	}
	if a.Access.QueryOAuthEnabled() {
		tool.Meta = mcp.Meta{"securitySchemes": []map[string]any{
			{"type": "oauth2", "scopes": []string{auth.QueryReadScope}},
		}}
	}
	if shared {
		schema.Properties["project"] = &jsonschema.Schema{Type: "string", Description: "Project slug returned by list_projects."}
		schema.Required = append(schema.Required, "project")
		mcp.AddTool(server, &mcp.Tool{
			Name: "list_projects", Description: "List the projects you can query on this connection.",
			Meta: tool.Meta, Annotations: tool.Annotations,
		}, a.listMCPProjects)
	}
	server.AddTool(tool, a.queryMCP)
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
		access, ok := a.mcpProjectAccess(w, r, shared)
		if !ok {
			return
		}
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpAccessKey{}, access)))
	}
}

func (a *API) queryMCP(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result := &mcp.CallToolResult{}
	access, ok := ctx.Value(mcpAccessKey{}).(mcpAccess)
	if !ok {
		result.SetError(errors.New("project authorization required"))
		return result, nil
	}
	var input struct {
		query.ModelQuery
		Project string `json:"project"`
	}
	var args any = &input.ModelQuery
	if access.fixedProjectID == "" {
		args = &input
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		result.SetError(errors.New("invalid query arguments"))
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, modelQueryTimeout)
	defer cancel()
	projectID, err := access.resolveProject(ctx, a.Store, input.Project)
	if err != nil {
		result.SetError(err)
		return result, nil
	}
	rows, err := input.Run(ctx, a.Store, projectID, 50, 256<<10)
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
}
