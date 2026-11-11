// Command benchmark populates an empty tracer and measures representative queries.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	trace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const project = "/api/v1/projects/default"

type client struct {
	base         string
	http         *http.Client
	read, ingest string
}

func (c client) request(path, media, token string, body []byte) ([]byte, error) {
	method := "GET"
	if body != nil {
		method = "POST"
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s returned %d", path, resp.StatusCode)
	}
	return data, nil
}

func attr(key, value string) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: value}}}
}

func number(key string, value int64) *common.KeyValue {
	return &common.KeyValue{Key: key, Value: &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: value}}}
}

func id(n uint64, size int) []byte {
	id := make([]byte, size)
	binary.BigEndian.PutUint64(id[size-8:], n)
	return id
}

func batch(start, count int, epoch int64) []*trace.Span {
	spans := make([]*trace.Span, 0, count)
	prompt := strings.Repeat("Compare the captured activity with the prior conversation. ", 9)
	answer := strings.Repeat("The run includes a model request and an observed tool result. ", 7)
	for i := start; i < start+count; i++ {
		tid := i/5 + 1
		step := i % 5
		now := uint64(epoch + int64(i)*int64(time.Millisecond))
		span := &trace.Span{
			TraceId: id(uint64(tid), 16), SpanId: id(uint64(step+1), 8), Name: "invoke_agent",
			StartTimeUnixNano: now, EndTimeUnixNano: now + 200000,
			Attributes: []*common.KeyValue{attr("gen_ai.conversation.id", fmt.Sprint("bench-", tid/5)), attr("fixture.region", "east")},
		}
		if step == 0 {
			span.Attributes = append(span.Attributes, attr("gen_ai.operation.name", "invoke_agent"), attr("gen_ai.agent.call.id", fmt.Sprint(tid)))
		} else {
			span.ParentSpanId = id(1, 8)
			if step%2 == 1 {
				span.Name = "chat benchmark-model"
				span.Attributes = append(span.Attributes,
					attr("gen_ai.operation.name", "chat"), attr("gen_ai.request.model", "benchmark-model"),
					attr("gen_ai.input.messages", fmt.Sprintf(`[{"role":"user","parts":[{"type":"text","content":%q}]}]`, prompt)),
					attr("gen_ai.output.messages", fmt.Sprintf(`[{"role":"assistant","parts":[{"type":"text","content":%q}]}]`, answer)),
					number("gen_ai.usage.input_tokens", 128), number("gen_ai.usage.output_tokens", 98), attr("llmproxy.cost.total_usd", "0.000051"))
			} else {
				span.Name = "search"
				span.Attributes = append(span.Attributes, attr("gen_ai.operation.name", "execute_tool"), attr("gen_ai.tool.name", "search"))
				if tid%5 == 0 {
					span.Status = &trace.Status{Code: trace.Status_STATUS_CODE_ERROR}
				}
			}
		}
		spans = append(spans, span)
	}
	return spans
}

func run() error {
	url := flag.String("url", "http://127.0.0.1:4318", "empty tracer instance")
	count := flag.Int("spans", 100000, "number of spans to insert")
	samples := flag.Int("samples", 30, "requests per measured route")
	flag.Parse()
	if *count < 10 || *samples < 1 {
		return errors.New("use at least 10 spans and one sample")
	}
	c := client{strings.TrimRight(*url, "/"), &http.Client{Timeout: 30 * time.Second}, os.Getenv("TRACER_BENCH_READ_TOKEN"), os.Getenv("TRACER_BENCH_INGEST_TOKEN")}
	epoch := time.Now().Add(-time.Hour).UnixNano()
	start := time.Now()
	for index := 0; index < *count; index += 500 {
		envelope := &collector.ExportTraceServiceRequest{ResourceSpans: []*trace.ResourceSpans{{
			Resource:   &resource.Resource{Attributes: []*common.KeyValue{attr("service.name", "benchmark")}},
			ScopeSpans: []*trace.ScopeSpans{{Spans: batch(index, min(500, *count-index), epoch)}},
		}}}
		raw, err := proto.Marshal(envelope)
		if err != nil {
			return err
		}
		response, err := c.request("/v1/traces", "application/x-protobuf", c.ingest, raw)
		if err != nil {
			return err
		}
		var result collector.ExportTraceServiceResponse
		if err = proto.Unmarshal(response, &result); err != nil {
			return err
		}
		if result.PartialSuccess.GetRejectedSpans() != 0 {
			return errors.New("fixture spans rejected")
		}
	}
	fmt.Printf("Ingested %d spans in %s\n", *count, time.Since(start).Round(time.Millisecond))
	data, err := c.request(project+"/sessions?limit=1", "", c.read, nil)
	if err != nil {
		return err
	}
	var list struct{ Items []struct{ ID string } }
	if err = json.Unmarshal(data, &list); err != nil {
		return err
	}
	if len(list.Items) == 0 {
		return errors.New("no sessions after ingest")
	}
	session := project + "/sessions/" + list.Items[0].ID
	for _, route := range []struct{ name, path string }{
		{"Sessions", project + "/sessions"}, {"Filtered sessions", project + "/sessions?tool=search&status=STATUS_CODE_ERROR"},
		{"Traces", project + "/traces"}, {"Session timeline", session + "/timeline"},
		{"Span detail", project + "/traces/00000000000000000000000000000001/spans/0000000000000002"},
	} {
		var times []time.Duration
		if _, err = c.request(route.path, "", c.read, nil); err != nil {
			return err
		}
		for range *samples {
			start := time.Now()
			if _, err = c.request(route.path, "", c.read, nil); err != nil {
				return err
			}
			times = append(times, time.Since(start))
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		fmt.Printf("%-18s p50 %9s  p95 %9s (%d samples)\n", route.name, times[(len(times)-1)/2].Round(time.Microsecond), times[(95*len(times)+99)/100-1].Round(time.Microsecond), len(times))
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
