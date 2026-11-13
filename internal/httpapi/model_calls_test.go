package httpapi_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/httpapi"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	trace "go.opentelemetry.io/proto/otlp/trace/v1"
)

type modelResult struct {
	Columns    []string
	Rows       []map[string]any
	NextCursor string
	Truncated  bool
}

func TestModelQueryDatabaseParity(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "tracer.db")
			if backend == "postgres" {
				base := os.Getenv("TRACER_TEST_POSTGRES")
				if base == "" {
					t.Skip("set TRACER_TEST_POSTGRES to exercise PostgreSQL")
				}
				dsn = postgresFixture(t, base)
			}
			f := newFixture(t, dsn)
			exerciseModelQuery(t, f)
		})
	}
}

func exerciseModelQuery(t *testing.T, f *fixture) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const window = `"from":"2026-01-01T00:00:00Z","to":"2026-01-02T00:00:00Z"`
	const sqlWindow = " WHERE started_at >= '2026-01-01T00:00:00Z' AND started_at < '2026-01-02T00:00:00Z'"
	const path = root + "/query"

	native := "client-session"
	digest := fmt.Sprintf("app:42:%x", sha256.Sum256([]byte(native)))
	session := attr("gen_ai.conversation.id", digest)
	app := attr("langfuse.trace.metadata.application_id", "42")
	client := makeSpan(1, 1, 0, "model client", now, attr("gen_ai.conversation.id", native),
		attr("gen_ai.operation.name", "chat"), number("gen_ai.usage.output_tokens", 4))
	client.Kind = trace.Span_SPAN_KIND_CLIENT
	server := makeSpan(1, 2, 1, "proxy", now, session, app)
	server.Kind = trace.Span_SPAN_KIND_SERVER
	proxy := makeSpan(1, 3, 2, "model", now, session, app, attr("gen_ai.operation.name", "chat"),
		attr("gen_ai.request.model", "alias"), attr("gen_ai.response.model", "alpha"), attr("gen_ai.provider.name", "provider"),
		attr("llmproxy.request_id", "request-1"), attr("llmproxy.cost.total_usd", "0.1001"),
		number("gen_ai.usage.input_tokens", 9007199254740993), double("gen_ai.response.time_to_first_chunk", 0.25),
		attr("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"private input"}]}]`),
		attr("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"model output\u0000"},{"type":"tool_call","id":"call-1"}]}]`),
		number("rank", 1))
	f.ingest(agentToken, 0, client)
	f.ingest(proxyToken, 0, server, proxy)

	legacy := makeSpan(2, 3, 0, "legacy", now, attr("gen_ai.operation.name", "chat"),
		attr("gen_ai.response.model", "beta"), attr("gen_ai.usage.cost.amount", "0.7"),
		attr("gen_ai.usage.cost.currency", "EUR"))
	legacy.EndTimeUnixNano = 0
	legacy.Events = []*trace.Span_Event{
		{Name: "gen_ai.user.message", Attributes: []*common.KeyValue{attr("message.content", "private event")}},
		{Name: "gen_ai.choice", Attributes: []*common.KeyValue{attr("message.content", "legacy answer")}},
	}
	unknown := makeSpan(3, 3, 0, "unknown", now.Add(time.Second), attr("gen_ai.operation.name", "chat"),
		attr("gen_ai.response.model", "beta"))
	outside := makeSpan(4, 3, 0, "outside", now.Add(24*time.Hour), attr("gen_ai.operation.name", "chat"))
	f.ingest(agentToken, 0, legacy, unknown, outside)

	// Export batches are an implementation detail and must not affect coverage.
	batchStart := now.Add(48 * time.Hour)
	batch := make([]*trace.Span, 0, 101)
	for i := 1; i <= 101; i++ {
		batch = append(batch, makeSpan(5, byte(i), 0, "batch", batchStart,
			attr("gen_ai.operation.name", "chat")))
	}
	f.ingest(proxyToken, 0, batch...)
	batchExport := f.call("POST", path+"/export", readerToken,
		`{"from":"2026-01-03T00:00:00Z","to":"2026-01-04T00:00:00Z","select":["span_id"]}`, 200).Body.String()
	if strings.Count(batchExport, `"type":"row"`) != 101 || !strings.HasSuffix(batchExport, "{\"type\":\"complete\"}\n") {
		t.Fatalf("unexpected multi-batch export: %s", batchExport)
	}

	if _, err := f.store.CreateProject(t.Context(), "other", 30, false); err != nil {
		t.Fatal(err)
	}
	f.ingest(otherProjectToken, 0, proxy)
	noCapture := f.call("POST", "/api/v1/projects/other/query", "", "{"+window+`,"select":["has_output","output_messages"]}`, 200)
	if !strings.Contains(noCapture.Body.String(), `"has_output":false,"output_messages":[]`) {
		t.Fatal(noCapture.Body.String())
	}
	f.call("POST", "/api/v1/projects/other/query", readerToken, "{"+window+"}", 404)
	f.call("POST", path, proxyToken, "{"+window+"}", 401)

	post := func(body string, status int) *httptest.ResponseRecorder {
		return f.call("POST", path, readerToken, body, status)
	}
	defaults := post("{"+window+"}", 200).Body.String()
	if strings.Contains(defaults, "model output") || strings.Contains(defaults, "output_messages") || strings.Contains(defaults, "private") {
		t.Fatal(defaults)
	}
	body := "{" + window + `,"limit":1,"select":["trace_id","output_messages"]`
	var results []map[string]any
	cursor := ""
	for range 3 {
		page := readJSON[modelResult](t, post(body+`,"cursor":`+jsonString(cursor)+"}", 200))
		if len(page.Rows) != 1 {
			t.Fatalf("expected one row: %+v", page)
		}
		results = append(results, page.Rows[0])
		cursor = page.NextCursor
	}
	if cursor != "" || results[0]["trace_id"] != strings.Repeat("03", 16) ||
		results[1]["trace_id"] != strings.Repeat("02", 16) || results[2]["trace_id"] != strings.Repeat("01", 16) {
		t.Fatalf("wrong page order: %+v", results)
	}
	if raw, _ := json.Marshal(results); strings.Contains(string(raw), "private") || !strings.Contains(string(raw), "legacy answer") ||
		!strings.Contains(string(raw), "model output") || !strings.Contains(string(raw), "tool_call") {
		t.Fatalf("wrong captured output: %s", raw)
	}
	first := readJSON[modelResult](t, post(body+"}", 200))
	post("{"+window+`,"select":["trace_id"],"cursor":`+jsonString(first.NextCursor)+"}", 400)
	post("{"+window+`,"limit":0}`, 400)

	filtered := post("{"+window+`,"filters":{"model":"alias","provider":"provider","session":"client-session","minDurationMs":1000,"hasOutput":true,"metadataKey":"rank","metadataValue":1}}`, 200).Body.String()
	for _, want := range []string{`"proxy_request_id":"request-1"`, `"input":"9007199254740993"`, `"output":"4"`,
		`"time_to_first_chunk_ms":250`, `"cost_amount":"0.1001"`, `"cost_provenance":"llmproxy.cost.total_usd"`} {
		if !strings.Contains(filtered, want) {
			t.Fatalf("missing %s: %s", want, filtered)
		}
	}
	typed := readJSON[modelResult](t, post("{"+window+`,"filters":{"metadataKey":"rank","metadataValue":1.0}}`, 200))
	if len(typed.Rows) != 0 {
		t.Fatalf("typed metadata filter matched: %+v", typed)
	}

	total := post("{"+window+`,"select":["calls","reported_cost","known_cost_calls","p95_duration_ms","known_duration_calls"]}`, 200).Body.String()
	for _, want := range []string{`"calls":3`, `"known_cost_calls":2`, `"known_duration_calls":2`,
		`"p95_duration_ms":1000`, `"EUR":{"value":"0.7","knownCalls":1}`, `"USD":{"value":"0.1001","knownCalls":1}`} {
		if !strings.Contains(total, want) {
			t.Fatalf("missing %s: %s", want, total)
		}
	}

	sql := "SELECT served_model, currency, COUNT(*) AS calls, SUM(cost_amount) AS cost, P95(duration_ms) AS p95_ms FROM model_calls" +
		sqlWindow + " GROUP BY served_model, currency ORDER BY cost DESC LIMIT 2"
	grouped := readJSON[modelResult](t, post(`{"sql":`+jsonString(sql)+"}", 200))
	if !grouped.Truncated || len(grouped.Rows) != 2 || grouped.Rows[0]["cost"] != "0.7" ||
		grouped.Rows[0]["p95_ms"] != nil || grouped.Rows[1]["cost"] != "0.1001" {
		t.Fatalf("unexpected grouped SQL result: %+v", grouped)
	}
	for _, statement := range []string{
		"DELETE FROM model_calls",
		"SELECT * FROM spans" + sqlWindow,
		"SELECT * FROM model_calls JOIN spans ON 1=1" + sqlWindow,
		"SELECT * FROM model_calls" + sqlWindow + " OR 1=1",
		"SELECT arbitrary_function(cost_amount) FROM model_calls" + sqlWindow,
		"SELECT SUM(cost_amount) FROM model_calls" + sqlWindow,
		"SELECT * FROM model_calls" + sqlWindow + "; SELECT * FROM spans",
	} {
		post(`{"sql":`+jsonString(statement)+"}", 400)
	}

	export := func(body string) ([]string, string) {
		t.Helper()
		response := f.call("POST", path+"/export", readerToken, body, 200)
		if response.Header().Get("Content-Type") != "application/x-ndjson" {
			t.Fatalf("unexpected export type: %v", response.Header())
		}
		var ids []string
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			var record struct {
				Type, NextCursor string
				Row              map[string]string
			}
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			switch record.Type {
			case "row":
				ids = append(ids, record.Row["trace_id"])
			case "complete":
				if scanner.Scan() || scanner.Err() != nil {
					t.Fatal("unexpected trailing record")
				}
				return ids, record.NextCursor
			default:
				t.Fatal(scanner.Text())
			}
		}
		t.Fatal("export ended without completion")
		return nil, ""
	}
	exportBody := "{" + window + `,"select":["trace_id"],"limit":2`
	ids, next := export(exportBody + "}")
	if len(ids) != 2 || next == "" {
		t.Fatalf("unexpected first export page: %v, %q", ids, next)
	}
	ids, next = export(exportBody + `,"cursor":` + jsonString(next) + "}")
	if len(ids) != 1 || ids[0] != strings.Repeat("01", 16) || next != "" {
		t.Fatalf("unexpected second export page: %v, %q", ids, next)
	}
	f.call("POST", path+"/export", readerToken, "{"+window+`,"groupBy":["day"]}`, 400)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

type authenticatedMCPTransport struct{}

func (authenticatedMCPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+readerToken)
	return http.DefaultTransport.RoundTrip(r)
}

func TestModelQueryMCP(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "tracer.db"))
	f.ingest(proxyToken, 0, makeSpan(1, 1, 0, "model", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		attr("gen_ai.operation.name", "chat"), number("rank", 9007199254740993)))
	if _, err := f.store.CreateProject(t.Context(), "other", 30, true); err != nil {
		t.Fatal(err)
	}
	f.call("POST", root+"/mcp", proxyToken, "{}", 401)
	f.call("POST", "/api/v1/projects/other/mcp", readerToken, "{}", 404)
	badOrigin := httptest.NewRequest("POST", "http://localhost"+root+"/mcp", strings.NewReader("{}"))
	badOrigin.Header.Set("Origin", "https://example.com")
	badOrigin.Header.Set("Authorization", "Bearer "+readerToken)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, badOrigin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unexpected origin status: %d", w.Code)
	}
	// A reverse proxy on loopback can forward the deployment's public Host.
	access, _, err := auth.New(t.Context(), false, fmt.Sprintf(
		`[{"token":%q,"access":"read","project":"default"}]`, readerToken), auth.OIDCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(f.store, access, nil, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "tracer.example"
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "tracer-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL + root + "/mcp", DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: authenticatedMCPTransport{}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "query_model_calls" || !tools.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("unexpected tool discovery: %+v, %v", tools, err)
	}
	input := map[string]any{
		"from": "2026-01-01T00:00:00Z", "to": "2026-01-02T00:00:00Z", "select": []string{"trace_id"},
		"filters": map[string]any{"metadataKey": "rank", "metadataValue": int64(9007199254740993)},
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "query_model_calls", Arguments: input})
	if err != nil || result.IsError {
		raw, _ := json.Marshal(result)
		t.Fatalf("unexpected MCP result: %s, %v", raw, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil || !strings.Contains(string(raw), strings.Repeat("01", 16)) {
		t.Fatalf("unexpected structured result: %s, %v", raw, err)
	}
	input["limit"] = 51
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "query_model_calls", Arguments: input})
	if err != nil || !result.IsError {
		t.Fatalf("unexpected MCP result for invalid limit: %+v, %v", result, err)
	}
}

func TestModelQueryMigratesExistingRows(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "tracer.db")
	f := newFixture(t, dsn)
	f.ingest(proxyToken, 0, makeSpan(1, 1, 0, "existing", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		attr("gen_ai.operation.name", "chat")))
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), "DROP INDEX model_call_order; DELETE FROM schema_migrations WHERE version=3"); err != nil {
		t.Fatal(err)
	}
	f = newFixture(t, dsn)
	result := readJSON[modelResult](t, f.call("POST", root+"/query", readerToken,
		`{"from":"2026-01-01T00:00:00Z","to":"2026-01-02T00:00:00Z","select":["span_id"]}`, 200))
	if len(result.Rows) != 1 || result.Rows[0]["span_id"] != strings.Repeat("01", 8) {
		t.Fatalf("old span unavailable after migration: %+v", result)
	}
}
