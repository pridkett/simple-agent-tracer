package semantic

import (
	"strings"
	"testing"

	common "go.opentelemetry.io/proto/otlp/common/v1"
	otlp "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Older simple-llm-proxy releases export each direction as GenAI and Langfuse attributes.
func TestNormalizeKeepsLargeProxyCapture(t *testing.T) {
	messages := `[{"role":"user","parts":[{"type":"text","content":"` + strings.Repeat("x", 1<<20) + `"}]}]`
	str := func(key, value string) *common.KeyValue {
		return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: value}}}
	}
	span := &otlp.Span{
		TraceId: []byte(strings.Repeat("\x01", 16)), SpanId: []byte(strings.Repeat("\x02", 8)), Name: "chat",
		Attributes: []*common.KeyValue{
			str("gen_ai.operation.name", "chat"),
			str("gen_ai.input.messages", messages), str("langfuse.observation.input", messages),
			str("gen_ai.output.messages", messages), str("langfuse.observation.output", messages),
		},
	}
	out, ok := Normalize(span, nil, nil, [2]string{}, "proxy", 1, true)
	if !ok || out.Truncated {
		t.Fatalf("ok=%v truncated=%v", ok, out.Truncated)
	}
	for _, key := range []string{"gen_ai.input.messages", "langfuse.observation.input", "gen_ai.output.messages", "langfuse.observation.output"} {
		if out.Attributes[key] != messages {
			t.Errorf("%s was dropped", key)
		}
	}
	if len(out.Input) != 1 || len(out.Output) != 1 {
		t.Fatalf("input=%d output=%d messages", len(out.Input), len(out.Output))
	}
}

func TestNormalizeFlagsContentOverBudget(t *testing.T) {
	huge := strings.Repeat("x", attributeLimit+1)
	span := &otlp.Span{
		TraceId: []byte(strings.Repeat("\x01", 16)), SpanId: []byte(strings.Repeat("\x02", 8)),
		Attributes: []*common.KeyValue{{Key: "input.value", Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: huge}}}},
	}
	out, ok := Normalize(span, nil, nil, [2]string{}, "proxy", 1, true)
	if !ok || !out.Truncated || out.Attributes["input.value"] != nil {
		t.Fatalf("ok=%v truncated=%v kept=%v", ok, out.Truncated, out.Attributes["input.value"] != nil)
	}
}

func TestNormalizeReadsProxyApplicationAttributes(t *testing.T) {
	str := func(key, value string) *common.KeyValue {
		return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: value}}}
	}
	for name, attrs := range map[string][]*common.KeyValue{
		"semantic conventions": {str("llmproxy.application", "canyon"), str("llmproxy.application_id", "7")},
		"langfuse metadata":    {str("langfuse.trace.metadata.application_name", "canyon"), str("langfuse.trace.metadata.application_id", "7")},
	} {
		span := &otlp.Span{TraceId: []byte(strings.Repeat("\x01", 16)), SpanId: []byte(strings.Repeat("\x02", 8)), Attributes: attrs}
		out, ok := Normalize(span, nil, nil, [2]string{}, "proxy", 1, true)
		if !ok || out.Application != "canyon" || out.ProxyApp != "7" {
			t.Errorf("%s: application=%q id=%q", name, out.Application, out.ProxyApp)
		}
	}
}
