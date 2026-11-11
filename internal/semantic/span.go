package semantic

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	common "go.opentelemetry.io/proto/otlp/common/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	otlp "go.opentelemetry.io/proto/otlp/trace/v1"
)

const ContentLimit = 64 << 10
const attributeLimit = 64 << 10
const metadataLimit = 96 << 10

type Attributes map[string]any

// Double encodes whole-valued OTLP doubles with a fractional part, retaining
// their distinction from OTLP integer attributes in stored and exported JSON.
type Double float64

func (d Double) MarshalJSON() ([]byte, error) {
	return []byte(doubleJSON(float64(d))), nil
}

func doubleJSON(d float64) string {
	value := strconv.FormatFloat(d, 'g', -1, 64)
	if !strings.ContainsAny(value, ".eE") {
		value += ".0"
	}
	return value
}

type Message struct {
	Role string `json:"role"`
	ID   string `json:"id,omitempty"`
	Data any    `json:"data"`
}

type Usage struct {
	Input      *int64            `json:"input"`
	Output     *int64            `json:"output"`
	CacheRead  *int64            `json:"cacheRead"`
	CacheWrite *int64            `json:"cacheWrite"`
	Reasoning  *int64            `json:"reasoning"`
	Total      *int64            `json:"total"`
	Amount     string            `json:"amount,omitempty"`
	Currency   string            `json:"currency,omitempty"`
	Provenance map[string]string `json:"provenance,omitempty"`
}

type Span struct {
	Trace                     string            `json:"traceId"`
	ID                        string            `json:"id"`
	Parent                    string            `json:"parentId,omitempty"`
	Source                    string            `json:"source"`
	Service                   string            `json:"service,omitempty"`
	Name                      string            `json:"name"`
	Kind                      string            `json:"kind"`
	Status                    string            `json:"status"`
	StatusMessage             string            `json:"statusMessage,omitempty"`
	Start                     int64             `json:"start,string"`
	End                       int64             `json:"end,string"`
	Received                  int64             `json:"received,string"`
	Operation                 string            `json:"operation,omitempty"`
	Provider                  string            `json:"provider,omitempty"`
	RequestModel              string            `json:"requestModel,omitempty"`
	ResponseModel             string            `json:"responseModel,omitempty"`
	Tool                      string            `json:"tool,omitempty"`
	ProxyApp                  string            `json:"proxyApplicationId,omitempty"`
	Application               string            `json:"application,omitempty"`
	ProxyRequest              string            `json:"proxyRequestId,omitempty"`
	Conversation              string            `json:"conversation,omitempty"`
	Aliases                   map[string]string `json:"observedSessionIds,omitempty"`
	Turn                      string            `json:"producerTurnId,omitempty"`
	ModelCall                 bool              `json:"modelCall"`
	Attributes                Attributes        `json:"attributes"`
	Resource                  Attributes        `json:"resource"`
	Scope                     Attributes        `json:"scopeAttributes"`
	ScopeName                 string            `json:"scopeName,omitempty"`
	ScopeVersion              string            `json:"scopeVersion,omitempty"`
	ResourceSchema            string            `json:"resourceSchema,omitempty"`
	ScopeSchema               string            `json:"scopeSchema,omitempty"`
	TraceState                string            `json:"traceState,omitempty"`
	Flags                     uint32            `json:"flags,omitempty"`
	DroppedAttributes         uint32            `json:"droppedAttributes,omitempty"`
	DroppedEvents             uint32            `json:"droppedEvents,omitempty"`
	DroppedLinks              uint32            `json:"droppedLinks,omitempty"`
	DroppedResourceAttributes uint32            `json:"droppedResourceAttributes,omitempty"`
	DroppedScopeAttributes    uint32            `json:"droppedScopeAttributes,omitempty"`
	Events                    []Event           `json:"events,omitempty"`
	Links                     []Link            `json:"links,omitempty"`
	Input                     []Message         `json:"input,omitempty"`
	Output                    []Message         `json:"output,omitempty"`
	Messages                  []Message         `json:"messages,omitempty"`
	Instructions              any               `json:"instructions,omitempty"`
	Usage                     Usage             `json:"usage"`
	Partial                   bool              `json:"partial"`
	Truncated                 bool              `json:"truncated"`
	RetainedBytes             int               `json:"-"`
	Diagnostics               []string          `json:"diagnostics,omitempty"`
}

type Event struct {
	Name              string     `json:"name"`
	Time              uint64     `json:"time,string"`
	Attributes        Attributes `json:"attributes"`
	DroppedAttributes uint32     `json:"droppedAttributes,omitempty"`
}

type Link struct {
	Trace             string     `json:"traceId"`
	Span              string     `json:"spanId"`
	TraceState        string     `json:"traceState,omitempty"`
	Flags             uint32     `json:"flags,omitempty"`
	Attributes        Attributes `json:"attributes"`
	DroppedAttributes uint32     `json:"droppedAttributes,omitempty"`
}

func validID(id []byte, n int) bool {
	if len(id) != n {
		return false
	}
	for _, b := range id {
		if b != 0 {
			return true
		}
	}
	return false
}

func bounded(s string) string {
	const limit = 4096
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "")
}

func (a Attributes) str(keys ...string) string {
	for _, key := range keys {
		if value, ok := a[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func contentKey(key string) bool {
	key = strings.ToLower(key)
	for _, prefix := range []string{"langfuse.observation.input", "langfuse.observation.output", "langfuse.trace.input", "langfuse.trace.output", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.tool.definitions", "gen_ai.prompt", "gen_ai.completion", "llm.input_messages", "llm.output_messages", "llm.prompts", "llm.prompt", "llm.completion", "input.value", "output.value", "tool.parameters", "tool.arguments", "tool.result", "message.content", "message.tool_calls", "content", "body"} {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			return true
		}
	}
	return false
}

type capture struct {
	enabled   bool
	truncated bool
	remaining int
	metadata  int
}

func (c *capture) attributes(entries []*common.KeyValue, eventContent bool) Attributes {
	attrs := Attributes{}
	for _, entry := range entries {
		key := entry.GetKey()
		if len(key) > 256 || strings.ContainsRune(key, 0) {
			c.truncated = true
			continue
		}
		body := eventContent || contentKey(key)
		if body && !c.enabled {
			continue
		}
		available := &c.metadata
		if body {
			available = &c.remaining
		}
		budget := min(attributeLimit, *available)
		initial := budget
		budget -= len(key)
		value, ok := decode(entry.GetValue(), &budget, 0, c.enabled)
		if !ok {
			c.truncated = true
			continue
		}
		*available -= initial - budget
		if body {
			value, ok = withoutInlineMedia(value)
			if ok {
				c.truncated = true
			}
		}
		attrs[key] = value
	}
	return attrs
}

// Message envelopes may carry media inside a JSON string as well as typed OTLP attributes.
func withoutInlineMedia(value any) (any, bool) {
	if raw, ok := value.(string); ok && json.Valid([]byte(raw)) {
		var parsed any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		if d.Decode(&parsed) == nil {
			cleaned, changed := removeMedia(parsed)
			if changed {
				encoded, err := json.Marshal(cleaned)
				if err == nil {
					return string(encoded), true
				}
			}
			return value, false
		}
	}
	return removeMedia(value)
}

func removeMedia(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		for _, prefix := range []string{"data:image/", "data:audio/", "data:video/", "data:application/octet-stream"} {
			if strings.HasPrefix(v, prefix) {
				return "[inline media omitted]", true
			}
		}
	case []any:
		changed := false
		for i, item := range v {
			cleaned, removed := removeMedia(item)
			v[i] = cleaned
			changed = changed || removed
		}
		return v, changed
	case Attributes:
		return removeMedia(map[string]any(v))
	case map[string]any:
		kind, _ := v["type"].(string)
		if kind == "base64" {
			return map[string]any{"type": kind, "omitted": true}, true
		}
		changed := false
		for key, item := range v {
			media := kind == "image" || kind == "audio" || kind == "video" || kind == "input_audio" || kind == "inline_data"
			if media && (key == "data" || key == "bytes" || key == "base64" || key == "content") {
				delete(v, key)
				changed = true
				continue
			}
			cleaned, removed := removeMedia(item)
			v[key] = cleaned
			changed = changed || removed
		}
		return v, changed
	}
	return value, false
}

// Charge keys and every node so nested values are bounded even if their strings are empty.
func decode(value *common.AnyValue, budget *int, depth int, includeContent bool) (any, bool) {
	*budget -= 16
	if value == nil || *budget < 0 || depth > 24 {
		return nil, false
	}
	switch v := value.Value.(type) {
	case *common.AnyValue_StringValue:
		*budget -= len(v.StringValue)
		return v.StringValue, *budget >= 0
	case *common.AnyValue_IntValue:
		return v.IntValue, true
	case *common.AnyValue_DoubleValue:
		return Double(v.DoubleValue), !math.IsNaN(v.DoubleValue) && !math.IsInf(v.DoubleValue, 0)
	case *common.AnyValue_BoolValue:
		return v.BoolValue, true
	case *common.AnyValue_ArrayValue:
		values := make([]any, 0)
		for _, item := range v.ArrayValue.GetValues() {
			decoded, ok := decode(item, budget, depth+1, includeContent)
			if !ok {
				return nil, false
			}
			values = append(values, decoded)
		}
		return values, true
	case *common.AnyValue_KvlistValue:
		values := Attributes{}
		for _, entry := range v.KvlistValue.GetValues() {
			if !includeContent && contentKey(entry.GetKey()) {
				continue
			}
			*budget -= len(entry.GetKey())
			decoded, ok := decode(entry.GetValue(), budget, depth+1, includeContent)
			if !ok {
				return nil, false
			}
			values[entry.GetKey()] = decoded
		}
		return values, true
	default:
		return nil, false
	}
}

func Normalize(s *otlp.Span, r *resource.Resource, scope *common.InstrumentationScope, schemas [2]string, source string, received int64, captureContent bool) (Span, bool) {
	if !validID(s.GetTraceId(), 16) || !validID(s.GetSpanId(), 8) ||
		(len(s.GetParentSpanId()) != 0 && !validID(s.GetParentSpanId(), 8)) ||
		s.GetStartTimeUnixNano() > math.MaxInt64 || s.GetEndTimeUnixNano() > math.MaxInt64 {
		return Span{}, false
	}
	c := capture{enabled: captureContent, remaining: ContentLimit, metadata: metadataLimit}
	out := Span{
		Trace: hex.EncodeToString(s.TraceId), ID: hex.EncodeToString(s.SpanId), Parent: hex.EncodeToString(s.ParentSpanId),
		Source: source, Name: bounded(s.Name), Kind: s.Kind.String(), Status: s.Status.GetCode().String(),
		Start: int64(s.StartTimeUnixNano), End: int64(s.EndTimeUnixNano), Received: received,
		Attributes: c.attributes(s.Attributes, false), Resource: c.attributes(r.GetAttributes(), false),
		Scope: c.attributes(scope.GetAttributes(), false), ScopeName: bounded(scope.GetName()), ScopeVersion: bounded(scope.GetVersion()),
		ResourceSchema: bounded(schemas[0]), ScopeSchema: bounded(schemas[1]), TraceState: bounded(s.TraceState), Flags: s.Flags,
		DroppedAttributes: s.DroppedAttributesCount, DroppedEvents: s.DroppedEventsCount, DroppedLinks: s.DroppedLinksCount,
		DroppedResourceAttributes: r.GetDroppedAttributesCount(), DroppedScopeAttributes: scope.GetDroppedAttributesCount(),
	}
	if captureContent {
		out.StatusMessage = bounded(s.Status.GetMessage())
	}
	a := out.Attributes
	out.Service = out.Resource.str("service.name")
	out.Operation = a.str("gen_ai.operation.name", "openinference.span.kind")
	out.Provider = a.str("gen_ai.provider.name", "gen_ai.system", "llm.provider")
	out.RequestModel = a.str("gen_ai.request.model", "llm.model_name")
	out.ResponseModel = a.str("gen_ai.response.model")
	out.Tool = a.str("gen_ai.tool.name", "tool.name")
	out.Application = a.str("langfuse.trace.metadata.application_name", "llmproxy.application")
	out.ProxyApp = a.str("langfuse.trace.metadata.application_id")
	out.ProxyRequest = a.str("llmproxy.request_id")
	out.Aliases = map[string]string{}
	for _, key := range []string{"gen_ai.conversation.id", "session.id", "langfuse.session.id"} {
		if id := a.str(key); id != "" && len(id) <= 1024 {
			out.Aliases[key] = id
			if out.Conversation == "" {
				out.Conversation = id
			}
			if out.Conversation != id && len(out.Diagnostics) == 0 {
				out.Diagnostics = append(out.Diagnostics, "conflicting session identifiers")
			}
		}
	}
	if id := a.str("gen_ai.agent.call.id"); len(id) <= 1024 {
		out.Turn = id
	}
	out.ModelCall = strings.EqualFold(a.str("openinference.span.kind"), "LLM")
	switch out.Operation {
	case "chat", "text_completion", "generate_content", "embeddings":
		out.ModelCall = true
	case "", "completion", "llm", "inference":
		out.ModelCall = out.ModelCall || out.RequestModel != "" || out.ResponseModel != ""
	case "invoke_agent", "execute_tool", "AGENT", "CHAIN", "TOOL":
		out.ModelCall = false
	}
	out.Usage = out.readUsage()
	out.Input = out.messages("gen_ai.input.messages", "llm.input_messages.")
	out.Output = out.messages("gen_ai.output.messages", "llm.output_messages.")
	out.Instructions = out.jsonContent("gen_ai.system_instructions")
	out.Partial = a["llmproxy.capture.output_partial"] == true
	if len(s.Events) > 256 || len(s.Links) > 256 {
		c.truncated = true
	}
	for _, event := range s.Events[:min(256, len(s.Events))] {
		eventBody := (strings.HasPrefix(event.GetName(), "gen_ai.") && strings.HasSuffix(event.GetName(), ".message")) || event.GetName() == "gen_ai.choice"
		attrs := c.attributes(event.GetAttributes(), eventBody)
		// Older GenAI message events carry the body in attributes such as tool_calls.
		out.Events = append(out.Events, Event{bounded(event.GetName()), event.GetTimeUnixNano(), attrs, event.GetDroppedAttributesCount()})
		if eventBody && len(attrs) > 0 {
			role := strings.TrimSuffix(strings.TrimPrefix(event.GetName(), "gen_ai."), ".message")
			if role == "choice" {
				role = "assistant"
			}
			out.Messages = append(out.Messages, Message{Role: role, ID: attrs.str("message.id"), Data: attrs})
		}
	}
	for _, link := range s.Links[:min(256, len(s.Links))] {
		if !validID(link.GetTraceId(), 16) || !validID(link.GetSpanId(), 8) {
			out.Diagnostics = append(out.Diagnostics, "invalid span link identifier")
			continue
		}
		out.Links = append(out.Links, Link{hex.EncodeToString(link.TraceId), hex.EncodeToString(link.SpanId), bounded(link.TraceState), link.Flags, c.attributes(link.Attributes, false), link.DroppedAttributesCount})
	}
	out.RetainedBytes = 1024 + ContentLimit - c.remaining + metadataLimit - c.metadata + len(out.Events)*128 + len(out.Links)*128 + len(out.Name) + len(out.ScopeName) + len(out.ScopeVersion) + len(out.ScopeSchema) + len(out.ResourceSchema) + len(out.StatusMessage) + len(out.TraceState)
	out.Truncated = c.truncated || a["tracer.tool.content_truncated"] == true || a["llmproxy.capture.input_truncated"] == true || a["llmproxy.capture.output_truncated"] == true
	return out, true
}

func (s *Span) jsonContent(key string) any {
	value := s.Attributes[key]
	if str, ok := value.(string); ok {
		var parsed any
		d := json.NewDecoder(strings.NewReader(str))
		d.UseNumber()
		if json.Valid([]byte(str)) && d.Decode(&parsed) == nil {
			return parsed
		}
		s.Diagnostics = append(s.Diagnostics, "invalid JSON in "+key)
		return nil
	}
	return value
}

func (s *Span) messages(key, prefix string) []Message {
	if value := s.jsonContent(key); value != nil {
		if list, ok := value.([]any); ok {
			messages := make([]Message, 0, len(list))
			for _, data := range list {
				attrs, ok := data.(map[string]any)
				if typed, isTyped := data.(Attributes); isTyped {
					attrs, ok = typed, true
				}
				if ok {
					messages = append(messages, Message{Attributes(attrs).str("role"), Attributes(attrs).str("id", "message.id"), data})
				}
			}
			if len(messages) != len(list) {
				s.Diagnostics = append(s.Diagnostics, "invalid message in "+key)
			}
			return messages
		}
		s.Diagnostics = append(s.Diagnostics, "expected message array in "+key)
	}
	indexed := map[int]Attributes{}
	for k, value := range s.Attributes {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		number, field, ok := strings.Cut(strings.TrimPrefix(k, prefix), ".")
		index, err := strconv.Atoi(number)
		if !ok || err != nil || index < 0 {
			continue
		}
		if indexed[index] == nil {
			indexed[index] = Attributes{}
		}
		indexed[index][strings.TrimPrefix(field, "message.")] = value
	}
	indices := make([]int, 0, len(indexed))
	for index := range indexed {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var messages []Message
	for _, index := range indices {
		a := indexed[index]
		messages = append(messages, Message{a.str("role"), a.str("id"), a})
	}
	return messages
}

func (s *Span) readUsage() Usage {
	u := Usage{Provenance: map[string]string{}}
	for _, field := range []struct {
		name   string
		target **int64
		keys   []string
	}{
		{"input", &u.Input, []string{"gen_ai.usage.input_tokens", "llm.token_count.prompt"}},
		{"output", &u.Output, []string{"gen_ai.usage.output_tokens", "llm.token_count.completion"}},
		{"cacheRead", &u.CacheRead, []string{"gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_read_input_tokens", "llm.token_count.prompt_details.cache_read"}},
		{"cacheWrite", &u.CacheWrite, []string{"gen_ai.usage.cache_creation.input_tokens", "gen_ai.usage.cache_creation_input_tokens", "llm.token_count.prompt_details.cache_write"}},
		{"reasoning", &u.Reasoning, []string{"gen_ai.usage.reasoning.output_tokens", "gen_ai.usage.reasoning_tokens", "llm.token_count.completion_details.reasoning"}},
		{"total", &u.Total, []string{"gen_ai.usage.total_tokens", "llm.token_count.total"}},
	} {
		for _, key := range field.keys {
			if value, ok := s.Attributes[key]; ok {
				n, valid := value.(int64)
				if valid && n >= 0 {
					*field.target = &n
					u.Provenance[field.name] = key
					break
				}
				s.Diagnostics = append(s.Diagnostics, "invalid token count in "+key)
			}
		}
	}
	key, currency := "llmproxy.cost.total_usd", "USD"
	if _, found := s.Attributes[key]; !found {
		key = "gen_ai.usage.cost.amount"
		currency = s.Attributes.str("gen_ai.usage.cost.currency")
	}
	validCurrency := len(currency) == 3 && strings.Trim(currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == ""
	switch value := s.Attributes[key].(type) {
	case Double:
		u.Amount = strconv.FormatFloat(float64(value), 'f', -1, 64)
	case int64:
		u.Amount = strconv.FormatInt(value, 10)
	case string:
		if len(value) <= 128 && decimal(value) {
			u.Amount = value
		}
	}
	if u.Amount != "" && validCurrency {
		u.Currency = currency
		u.Provenance["amount"] = key
	} else if _, exists := s.Attributes[key]; exists {
		u.Amount = ""
		s.Diagnostics = append(s.Diagnostics, fmt.Sprintf("invalid reported amount or currency for %s", key))
	}
	return u
}

func decimal(s string) bool {
	s = strings.TrimPrefix(s, "-")
	n := 0
	for _, char := range s {
		if char == '.' && n == 0 {
			n++
			continue
		}
		if char < '0' || char > '9' {
			return false
		}
	}
	return len(s) > n
}

func IsContentKey(key string) bool { return contentKey(key) }

func IsBackground(operation string) bool {
	switch operation {
	case "title", "compaction", "summary", "background":
		return true
	default:
		return false
	}
}
