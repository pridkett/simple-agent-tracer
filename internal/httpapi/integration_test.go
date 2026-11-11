package httpapi_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/httpapi"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	trace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const root = "/api/v1/projects/default"
const proxyToken = "proxy-test-token-1234567890"
const agentToken = "agent-test-token-1234567890"
const readerToken = "reader-test-token-1234567890"
const otherAgentToken = "other-agent-test-token-1234567890"
const otherProjectToken = "other-project-test-token-1234567890"

type fixture struct {
	t       *testing.T
	store   *storage.Store
	handler interface {
		ServeHTTP(w http.ResponseWriter, r *http.Request)
	}
}

func newFixture(t *testing.T, dsn string) *fixture {
	store, err := storage.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	credentials := fmt.Sprintf(`[{"token":%q,"access":"ingest","project":"default","source":"proxy"},{"token":%q,"access":"ingest","project":"default","source":"agent"},{"token":%q,"access":"read","project":"default"},{"token":%q,"access":"ingest","project":"default","source":"other-agent"},{"token":%q,"access":"ingest","project":"other","source":"other-proxy"}]`, proxyToken, agentToken, readerToken, otherAgentToken, otherProjectToken)
	access, _, err := auth.New(t.Context(), true, credentials, auth.OIDCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t, store, httpapi.New(store, access, nil, nil)}
}

func postgresFixture(t *testing.T, base string) string {
	t.Helper()
	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := fmt.Sprintf("tracer_test_%d", time.Now().UnixNano())
	if _, err = db.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err = db.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	values := u.Query()
	values.Set("search_path", schema)
	u.RawQuery = values.Encode()
	return u.String()
}

func (f *fixture) call(method, path, token, body string, status int) *httptest.ResponseRecorder {
	f.t.Helper()
	request := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, request)
	if w.Code != status {
		f.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}

func attr(key, value string) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: value}}}
}

func number(key string, value int64) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: value}}}
}

func double(key string, value float64) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_DoubleValue{DoubleValue: value}}}
}

func makeSpan(traceByte, spanByte, parentByte byte, name string, at time.Time, attrs ...*common.KeyValue) *trace.Span {
	s := &trace.Span{TraceId: bytes.Repeat([]byte{traceByte}, 16), SpanId: bytes.Repeat([]byte{spanByte}, 8),
		Name: name, StartTimeUnixNano: uint64(at.UnixNano()), EndTimeUnixNano: uint64(at.Add(time.Second).UnixNano()), Attributes: attrs}
	if parentByte != 0 {
		s.ParentSpanId = bytes.Repeat([]byte{parentByte}, 8)
	}
	return s
}

func (f *fixture) push(token string, compressed bool, spans ...*trace.Span) (int, error) {
	envelope := &collector.ExportTraceServiceRequest{ResourceSpans: []*trace.ResourceSpans{{
		Resource:   &resource.Resource{Attributes: []*common.KeyValue{attr("service.name", "fixture"), number("resource.int", 9007199254740993)}},
		ScopeSpans: []*trace.ScopeSpans{{Scope: &common.InstrumentationScope{Name: "test", Version: "1"}, Spans: spans}},
	}}}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		return 0, err
	}
	if compressed {
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		if _, err = gz.Write(raw); err != nil {
			return 0, err
		}
		if err = gz.Close(); err != nil {
			return 0, err
		}
		raw = data.Bytes()
	}
	req := httptest.NewRequest("POST", "http://localhost/v1/traces", bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", "application/x-protobuf")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if compressed {
		req.Header.Set("Content-Encoding", "gzip")
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	if w.Code != 200 {
		return 0, fmt.Errorf("OTLP HTTP %d: %x", w.Code, w.Body.Bytes())
	}
	var result collector.ExportTraceServiceResponse
	if err = proto.Unmarshal(w.Body.Bytes(), &result); err != nil {
		return 0, err
	}
	return int(result.PartialSuccess.GetRejectedSpans()), nil
}

func (f *fixture) ingest(token string, expectedRejected int, spans ...*trace.Span) {
	f.t.Helper()
	rejected, err := f.push(token, true, spans...)
	if err != nil || rejected != expectedRejected {
		f.t.Fatalf("ingest rejected %d, want %d: %v", rejected, expectedRejected, err)
	}
}

type entityJSON struct {
	ID                 string
	ExternalID         string
	ModelCalls         int
	Spans              int
	Kept               bool
	ProtectedBySession bool
	ReportedCost       map[string]struct {
		Value      string
		KnownCalls int
	}
}

func readJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDatabaseParity(t *testing.T) {
	var sqliteResult string
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
			snapshot := exercise(t, f)
			// Reopening applies no new migrations and retains project settings.
			reopened, err := storage.Open(t.Context(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			p, err := reopened.Project(t.Context(), "default")
			if err != nil || p.Retention != 30 || p.Capture {
				t.Fatalf("unexpected project after reopen: %+v %v", p, err)
			}
			if backend == "sqlite" {
				sqliteResult = snapshot
			} else if snapshot != sqliteResult {
				t.Fatalf("API results differ:\n%s\n%s", sqliteResult, snapshot)
			}
		})
	}
}

func exercise(t *testing.T, f *fixture) string {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	native := "agent-native-id"
	digest := fmt.Sprintf("app:42:%x", sha256.Sum256([]byte(native)))
	conversation := attr("gen_ai.conversation.id", digest)
	app := attr("langfuse.trace.metadata.application_id", "42")
	proxyServer := makeSpan(1, 4, 3, "POST /v1/chat/completions", now.Add(2*time.Second), conversation, app)
	proxyServer.Kind = trace.Span_SPAN_KIND_SERVER
	proxyCall := makeSpan(1, 5, 4, "chat model", now.Add(3*time.Second), conversation, app,
		attr("gen_ai.operation.name", "chat"), attr("gen_ai.request.model", "model"),
		attr("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"<script>example</script>"}]}]`),
		attr("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"answer"}]}]`),
		attr("llmproxy.request_id", "proxy-1"), number("gen_ai.usage.input_tokens", 0), double("llmproxy.cost.total_usd", 0.1), number("rank", 1))
	invalid := makeSpan(0, 8, 0, "invalid", now)
	f.ingest(proxyToken, 1, proxyCall, invalid)
	earlier := readJSON[struct{ Items []entityJSON }](t, f.call("GET", root+"/sessions", "", "", 200))
	if len(earlier.Items) != 1 || earlier.Items[0].ExternalID != digest {
		t.Fatalf("unexpected proxy session: %+v", earlier)
	}
	oldID := earlier.Items[0].ID
	f.ingest(proxyToken, 0, proxyCall, proxyServer)
	f.ingest(proxyToken, 0, proxyCall)
	changed := proto.CloneOf(proxyCall)
	changed.Name = "different"
	f.ingest(proxyToken, 0, changed)
	traceID := strings.Repeat("01", 16)
	spanPath := root + "/traces/" + traceID + "/spans/" + strings.Repeat("05", 8)
	detail := f.call("GET", spanPath, "", "", 200).Body.String()
	if !strings.Contains(detail, `"conflictingExports":1`) || !strings.Contains(detail, "proxy-1") ||
		!strings.Contains(detail, `"resource.int":9007199254740993`) {
		t.Fatalf("span provenance or replay incorrect: %s", detail)
	}
	turn := makeSpan(1, 1, 0, "invoke agent", now, attr("gen_ai.conversation.id", native),
		attr("gen_ai.operation.name", "invoke_agent"), attr("gen_ai.agent.call.id", "turn1"), number("gen_ai.usage.input_tokens", 1000))
	client := makeSpan(1, 3, 1, "proxy request", now.Add(time.Second), attr("gen_ai.conversation.id", native),
		attr("gen_ai.operation.name", "chat"), number("gen_ai.usage.input_tokens", 100))
	client.Kind = trace.Span_SPAN_KIND_CLIENT
	tool := makeSpan(1, 2, 1, "search", now, attr("gen_ai.operation.name", "execute_tool"), attr("gen_ai.tool.name", "search"), double("rank", 1))
	tool.Status = &trace.Status{Code: trace.Status_STATUS_CODE_ERROR}
	var wg sync.WaitGroup
	for _, span := range []*trace.Span{turn, client, tool} {
		wg.Go(func() {
			rejected, err := f.push(agentToken, false, span)
			if err != nil || rejected != 0 {
				t.Errorf("parallel ingest: %d %v", rejected, err)
			}
		})
	}
	wg.Wait()
	canonical := readJSON[entityJSON](t, f.call("GET", root+"/sessions/"+oldID, readerToken, "", 200))
	if canonical.ExternalID != native || canonical.ModelCalls != 1 || canonical.Spans != 5 || canonical.ReportedCost["USD"].Value != "0.1" {
		t.Fatalf("linked session accounting failed: %+v", canonical)
	}
	if canonical.ID == oldID {
		t.Fatal("original ID did not resolve to canonical session")
	}
	// One descendant must satisfy both predicates. Integer and double exact matches differ.
	filtered := f.call("GET", root+"/sessions?tool=search&status=STATUS_CODE_ERROR&metadataKey=rank&metadataValue=1.0", "", "", 200).Body.String()
	if !strings.Contains(filtered, canonical.ID) {
		t.Fatalf("missing matching child: %s", filtered)
	}
	for _, query := range []string{"tool=search&model=model", "tool=search&metadataKey=rank&metadataValue=1"} {
		if raw := f.call("GET", root+"/sessions?"+query, "", "", 200).Body.String(); !strings.Contains(raw, `"items":[]`) {
			t.Fatal(raw)
		}
	}
	turnPage := readJSON[struct{ Items []struct{ ID string } }](t, f.call("GET", root+"/sessions/"+oldID+"/turns", "", "", 200))
	if len(turnPage.Items) != 1 {
		t.Fatalf("expected one turn: %+v", turnPage)
	}
	timeline := f.call("GET", root+"/sessions/"+oldID+"/turns/"+turnPage.Items[0].ID, "", "", 200).Body.String()
	if !strings.Contains(timeline, "proxy-1") || !strings.Contains(timeline, `"transcript":[{"role":"user"`) {
		t.Fatal(timeline)
	}
	page := readJSON[struct {
		Items      []struct{ ID string }
		NextCursor string
	}](t, f.call("GET", root+"/traces/"+traceID+"/spans?limit=1", "", "", 200))
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("bad pagination: %+v", page)
	}
	next := readJSON[struct{ Items []struct{ ID string } }](t, f.call("GET", root+"/traces/"+traceID+"/spans?limit=1&cursor="+page.NextCursor, "", "", 200))
	if len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatalf("repeated page: %+v", next)
	}
	f.call("POST", root+"/retention/keep", "", fmt.Sprintf(`{"traceIds":[%q,"absent"],"keep":true}`, traceID), 400)
	state := readJSON[entityJSON](t, f.call("GET", root+"/traces/"+traceID, "", "", 200))
	if state.Kept {
		t.Fatal("failed bulk Keep updated a trace")
	}
	f.call("POST", root+"/retention/keep", "", fmt.Sprintf(`{"sessionIds":[%q],"keep":true}`, oldID), 204)
	f.call("POST", root+"/retention/keep", readerToken, fmt.Sprintf(`{"sessionIds":[%q],"keep":false}`, oldID), 403)
	// Unrelated session and content policy changes.
	f.call("POST", "/api/v1/projects", "", `{"slug":"other","retentionDays":90}`, 201)
	f.call("GET", "/api/v1/projects/other/traces", readerToken, "", 404)
	f.call("PATCH", root, "", `{"captureContent":false}`, 200)
	hidden := makeSpan(2, 1, 0, "redacted", now, attr("gen_ai.input.messages", `[{"role":"user","content":"private fixture"}]`), attr("langfuse.observation.input", "private fixture"), attr("gen_ai.tool.call.result", "private fixture"))
	hidden.Events = []*trace.Span_Event{{Name: "gen_ai.user.message", Attributes: []*common.KeyValue{attr("tool_calls", "private fixture")}}}
	f.ingest(proxyToken, 0, hidden)
	hiddenID := strings.Repeat("02", 16)
	if raw := f.call("GET", root+"/traces/"+hiddenID+"/export", "", "", 200).Body.String(); strings.Contains(raw, "private fixture") || !json.Valid([]byte(raw)) {
		t.Fatal(raw)
	}
	kept := readJSON[entityJSON](t, f.call("GET", root+"/traces/"+traceID, "", "", 200))
	if !kept.ProtectedBySession {
		t.Fatal("old session link did not protect canonical trace")
	}
	export := f.call("GET", root+"/sessions/"+oldID+"/export", "", "", 200).Body.String()
	if !json.Valid([]byte(export)) || !strings.Contains(export, "proxy-1") {
		t.Fatal(export)
	}
	future := makeSpan(3, 1, 0, "future proxy trace", now, conversation, app)
	f.ingest(proxyToken, 0, future)
	futureID := strings.Repeat("03", 16)
	state = readJSON[entityJSON](t, f.call("GET", root+"/traces/"+futureID, "", "", 200))
	if !state.ProtectedBySession {
		t.Fatal("kept session did not protect a future proxy-only trace")
	}
	deleted, err := f.store.Sweep(t.Context(), time.Now().Add(24*time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("expected one expired trace: %d %v", deleted, err)
	}
	f.call("GET", root+"/traces/"+traceID, "", "", 200)
	f.call("GET", root+"/traces/"+hiddenID, "", "", 404)
	f.call("POST", root+"/retention/keep", "", fmt.Sprintf(`{"sessionIds":[%q],"keep":false}`, oldID), 204)
	deleted, err = f.store.Sweep(t.Context(), time.Now().Add(24*time.Hour))
	if err != nil || deleted != 2 {
		t.Fatalf("expected both unkept traces to expire: %d %v", deleted, err)
	}
	f.call("GET", root+"/sessions/"+oldID, "", "", 404)
	return filtered
}

func TestKeepAndSweep(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "tracer.db"))
	project, err := f.store.Project(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := byte(1); i <= 8; i++ {
		span := makeSpan(i, 1, 0, "expires", now.AddDate(0, 0, -60))
		f.ingest(proxyToken, 0, span)
		traceID := fmt.Sprintf("%x", span.TraceId)
		sweepTime := now.Add(time.Hour)
		var keepErr, sweepErr error
		var deleted int64
		var wg sync.WaitGroup
		wg.Go(func() { keepErr = f.store.Keep(t.Context(), project.ID, []string{traceID}, nil, true) })
		wg.Go(func() { deleted, sweepErr = f.store.Sweep(t.Context(), sweepTime) })
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		if keepErr == nil {
			kept := readJSON[entityJSON](t, f.call("GET", root+"/traces/"+traceID, "", "", 200))
			if !kept.Kept || deleted != 0 {
				t.Fatalf("kept trace was swept: %+v, deleted %d", kept, deleted)
			}
		} else {
			if !errors.Is(keepErr, storage.ErrInvalid) || deleted != 1 {
				t.Fatalf("inconsistent sweep outcome: %d, %v", deleted, keepErr)
			}
			f.call("GET", root+"/traces/"+traceID, "", "", 404)
		}
	}
}

func TestOpenInferenceAndCapture(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "tracer.db"))
	now := time.Now()
	openInference := makeSpan(1, 1, 0, "openinference fixture", now,
		attr("session.id", "observed"),
		attr("langfuse.session.id", "alternative"),
		attr("openinference.span.kind", "LLM"),
		attr("llm.model_name", "model"),
		number("llm.token_count.total", 7),
		attr("llm.input_messages.2.message.role", "user"), attr("llm.input_messages.2.message.content", "indexed prompt"),
		attr("llm.output_messages.0.message.role", "assistant"), attr("llm.output_messages.0.message.content", "indexed reply"))
	media := makeSpan(2, 1, 0, "inline media", now,
		attr("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"image","data":"inline-secret"},{"type":"text","content":"text survives"}]}]`))
	f.ingest("", 0, openInference, media)
	result := f.call("GET", root+"/traces?session=alternative&model=model", "", "", 200).Body.String()
	if !strings.Contains(result, `"total":{"value":"7","knownCalls":1}`) {
		t.Fatal(result)
	}
	tracePath := root + "/traces/" + strings.Repeat("01", 16) + "/spans/" + strings.Repeat("01", 8)
	span := f.call("GET", tracePath, "", "", 200).Body.String()
	if !strings.Contains(span, "indexed prompt") || !strings.Contains(span, "indexed reply") || !strings.Contains(span, "conflicting session identifiers") {
		t.Fatal(span)
	}
	tracePath = root + "/traces/" + strings.Repeat("02", 16) + "/spans/" + strings.Repeat("01", 8)
	span = f.call("GET", tracePath, "", "", 200).Body.String()
	if strings.Contains(span, "inline-secret") || !strings.Contains(span, "text survives") || !strings.Contains(span, `"truncated":true`) {
		t.Fatal(span)
	}
}

func TestSessionLinkConflict(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "tracer.db"))
	now := time.Now()
	const native = "reused-native-id"
	digest := fmt.Sprintf("app:42:%x", sha256.Sum256([]byte(native)))
	sessionID := attr("gen_ai.conversation.id", digest)
	app := attr("langfuse.trace.metadata.application_id", "42")
	for _, item := range []struct {
		trace byte
		token string
	}{{1, agentToken}, {2, otherAgentToken}} {
		server := makeSpan(item.trace, 3, 2, "proxy server", now, sessionID, app)
		server.Kind = trace.Span_SPAN_KIND_SERVER
		f.ingest(proxyToken, 0, server)
		client := makeSpan(item.trace, 2, 1, "agent request", now, attr("gen_ai.conversation.id", native))
		client.Kind = trace.Span_SPAN_KIND_CLIENT
		turn := makeSpan(item.trace, 1, 0, "invoke_agent", now, attr("gen_ai.conversation.id", native), attr("gen_ai.operation.name", "invoke_agent"), attr("gen_ai.agent.call.id", "run"))
		f.ingest(item.token, 0, turn, client)
	}
	sessions := readJSON[struct{ Items []entityJSON }](t, f.call("GET", root+"/sessions", "", "", 200))
	if len(sessions.Items) != 3 {
		t.Fatalf("ambiguous sources were merged: %+v", sessions)
	}
	var proxyID string
	for _, session := range sessions.Items {
		if session.ExternalID == digest {
			proxyID = session.ID
		}
	}
	if proxyID == "" {
		t.Fatal("proxy session lost in conflict")
	}
	response := f.call("GET", root+"/sessions/"+proxyID, "", "", 200).Body.String()
	if !strings.Contains(response, `"sessionConflict":true`) {
		t.Fatal(response)
	}
	response = f.call("GET", root+"/sessions/"+proxyID+"/turns", "", "", 200).Body.String()
	if !strings.Contains(response, `"items":[]`) {
		t.Fatal(response)
	}
}

func TestProjectRetention(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "tracer.db"))
	f.call("POST", "/api/v1/projects", "", `{"slug":"other","retentionDays":90}`, 201)
	now := time.Now()
	span := makeSpan(1, 1, 0, "sixty days old", now.AddDate(0, 0, -60))
	f.ingest(proxyToken, 0, span)
	f.ingest(otherProjectToken, 0, span)
	n, err := f.store.Sweep(t.Context(), now.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("expected expiration only in default project: %d %v", n, err)
	}
	tracePath := "/traces/" + strings.Repeat("01", 16)
	f.call("GET", root+tracePath, "", "", 404)
	f.call("GET", "/api/v1/projects/other"+tracePath, "", "", 200)
}
