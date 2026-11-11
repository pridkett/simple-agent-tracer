package query

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pridkett/simple_agent_tracer/internal/semantic"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

type Page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"nextCursor,omitempty"`
}

func Pagination(values url.Values) (int, *storage.Position, error) {
	limit := 50
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return 0, nil, storage.ErrInvalid
		}
		limit = n
	}
	cursor := values.Get("cursor")
	if cursor == "" {
		return limit, nil, nil
	}
	if len(cursor) > 512 {
		return 0, nil, storage.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	var p storage.Position
	if err != nil || json.Unmarshal(raw, &p) != nil || len(p.ID) == 0 || len(p.ID) > 32 || len(p.Trace) > 32 {
		return 0, nil, storage.ErrInvalid
	}
	return limit, &p, nil
}

func Cursor(pos storage.Position) string {
	raw, _ := json.Marshal(pos)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func Filters(values url.Values) (storage.Filter, error) {
	var f storage.Filter
	for _, field := range []struct {
		key    string
		target *string
	}{
		{"source", &f.Source}, {"service", &f.Service}, {"session", &f.Session}, {"operation", &f.Operation}, {"model", &f.Model},
		{"status", &f.Status}, {"tool", &f.Tool}, {"trace", &f.Trace}, {"metadataKey", &f.MetadataKey},
	} {
		*field.target = values.Get(field.key)
		if len(*field.target) > 1024 || strings.ContainsRune(*field.target, 0) {
			return f, storage.ErrInvalid
		}
	}
	for _, field := range []struct {
		key    string
		target **int64
	}{{"from", &f.From}, {"to", &f.To}} {
		if raw := values.Get(field.key); raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil || t.Year() < 1970 || t.Year() > 2261 {
				return f, storage.ErrInvalid
			}
			n := t.UnixNano()
			*field.target = &n
		}
	}
	if f.From != nil && f.To != nil && *f.From >= *f.To {
		return f, storage.ErrInvalid
	}
	if values.Has("metadataValue") {
		raw := values.Get("metadataValue")
		if f.MetadataKey == "" || len(raw) > 4096 || !json.Valid([]byte(raw)) {
			return f, storage.ErrInvalid
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var parsed any
		if decoder.Decode(&parsed) != nil || parsed == nil {
			return f, storage.ErrInvalid
		}
		parsed, err := canonicalMetadata(parsed)
		if err != nil {
			return f, err
		}
		data, err := json.Marshal(parsed)
		if err != nil {
			return f, storage.ErrInvalid
		}
		f.MetadataType = storage.MetadataType(data)
		f.MetadataDigest = storage.MetadataDigest(data)
	}
	return f, nil
}

func canonicalMetadata(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			n, err := v.Float64()
			if err != nil || math.IsInf(n, 0) {
				return nil, storage.ErrInvalid
			}
			return semantic.Double(n), nil
		}
		n, err := v.Int64()
		if err != nil {
			return nil, storage.ErrInvalid
		}
		return n, nil
	case []any:
		for i, item := range v {
			canonical, err := canonicalMetadata(item)
			if err != nil {
				return nil, err
			}
			v[i] = canonical
		}
	case map[string]any:
		for key, item := range v {
			canonical, err := canonicalMetadata(item)
			if err != nil {
				return nil, err
			}
			v[key] = canonical
		}
	}
	return value, nil
}

type Total struct {
	Value string `json:"value"`
	Known int    `json:"knownCalls"`
}

type Summary struct {
	Calls         int              `json:"modelCalls"`
	Spans         int              `json:"spans"`
	Status        string           `json:"status"`
	Application   string           `json:"application,omitempty"`
	SummarySource string           `json:"-"`
	Tokens        map[string]Total `json:"tokens"`
	Costs         map[string]Total `json:"reportedCost"`
}

type Accumulator struct {
	Summary
	tokens map[string]*big.Int
	costs  map[string]*big.Rat
	places map[string]int
}

func NewAccumulator() *Accumulator {
	return &Accumulator{
		Summary: Summary{Status: "STATUS_CODE_UNSET", Tokens: map[string]Total{}, Costs: map[string]Total{}},
		tokens:  map[string]*big.Int{}, costs: map[string]*big.Rat{}, places: map[string]int{},
	}
}

func (a *Accumulator) Add(o storage.Observation) error {
	a.Spans++
	if o.Status == "STATUS_CODE_ERROR" || (o.Status == "STATUS_CODE_OK" && a.Status == "STATUS_CODE_UNSET") {
		a.Status = o.Status
	}
	if a.SummarySource == "" || o.Source < a.SummarySource {
		a.SummarySource = o.Source
	}
	if o.Application != "" && (a.Application == "" || o.Application < a.Application) {
		a.Application = o.Application
	}
	if !o.Model || o.Linked {
		return nil
	}
	a.Calls++
	var u semantic.Usage
	if err := json.Unmarshal(o.Usage, &u); err != nil {
		return err
	}
	var fallback semantic.Usage
	if len(o.Fallback) > 0 {
		if err := json.Unmarshal(o.Fallback, &fallback); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name             string
		value, alternate *int64
	}{
		{"input", u.Input, fallback.Input}, {"output", u.Output, fallback.Output}, {"cacheRead", u.CacheRead, fallback.CacheRead}, {"cacheWrite", u.CacheWrite, fallback.CacheWrite}, {"reasoning", u.Reasoning, fallback.Reasoning}, {"total", u.Total, fallback.Total},
	} {
		if field.value == nil && u.Provenance[field.name] == "" && strings.HasPrefix(fallback.Provenance[field.name], "gen_ai.usage.") {
			field.value = field.alternate
		}
		if field.value == nil {
			continue
		}
		n := a.tokens[field.name]
		if n == nil {
			n = new(big.Int)
			a.tokens[field.name] = n
		}
		n.Add(n, big.NewInt(*field.value))
		a.Tokens[field.name] = Total{n.String(), a.Tokens[field.name].Known + 1}
	}
	if u.Amount != "" {
		amount, ok := new(big.Rat).SetString(u.Amount)
		if !ok {
			return errors.New("invalid stored monetary amount")
		}
		cost := a.costs[u.Currency]
		if cost == nil {
			cost = new(big.Rat)
			a.costs[u.Currency] = cost
		}
		_, decimals, _ := strings.Cut(u.Amount, ".")
		a.places[u.Currency] = max(a.places[u.Currency], len(decimals))
		cost.Add(cost, amount)
		a.Costs[u.Currency] = Total{cost.FloatString(a.places[u.Currency]), a.Costs[u.Currency].Known + 1}
	}
	return nil
}

type TranscriptMessage struct {
	semantic.Message
	Trace string `json:"traceId"`
	Span  string `json:"spanId"`
}

type Transcript struct {
	Messages []TranscriptMessage `json:"messages"`
	history  []historyMessage
	seen     map[string]bool
}

func NewTranscript() *Transcript {
	return &Transcript{Messages: []TranscriptMessage{}, seen: map[string]bool{}}
}

type historyMessage struct {
	role, id, content string
}

func history(messages []semantic.Message) []historyMessage {
	out := make([]historyMessage, 0, len(messages))
	for _, m := range messages {
		data := m.Data
		if object, ok := data.(map[string]any); ok {
			comparable := make(map[string]any, len(object))
			for key, value := range object {
				switch key {
				case "finish_reason", "stop_reason", "id", "message.id":
					continue
				}
				if key == "parts" {
					if parts, ok := value.([]any); ok {
						retained := make([]any, 0, len(parts))
						for _, part := range parts {
							if obj, ok := part.(map[string]any); ok && obj["type"] == "text" && (obj["content"] == nil || obj["content"] == "") {
								continue
							}
							retained = append(retained, part)
						}
						value = retained
					}
				}
				comparable[key] = value
			}
			data = comparable
		}
		raw, _ := json.Marshal(data)
		out = append(out, historyMessage{m.Role, m.ID, string(raw)})
	}
	return out
}

// The session window is execution ordered. Only remove context if the new
// snapshot contains a matching prefix or overlap with the previous history.
func (t *Transcript) Add(row storage.SpanRow) error {
	if row.Background {
		return nil
	}
	var span semantic.Span
	d := json.NewDecoder(bytes.NewReader(row.Data))
	d.UseNumber()
	if err := d.Decode(&span); err != nil {
		return err
	}
	add := func(messages []semantic.Message) {
		for _, m := range messages {
			key := span.Source + "\x00" + m.ID
			if m.ID != "" && t.seen[key] {
				continue
			}
			t.Messages = append(t.Messages, TranscriptMessage{m, span.Trace, span.ID})
			if m.ID != "" {
				t.seen[key] = true
			}
		}
	}
	if len(span.Messages) > 0 {
		add(conversationMessages(span.Messages))
		return nil
	}
	if !span.ModelCall || row.Linked {
		return nil
	}
	messages, output := conversationMessages(span.Input), conversationMessages(span.Output)
	if span.Partial || span.Truncated || len(messages) == 0 {
		add(messages)
		add(output)
		t.history = nil
		return nil
	}
	input := history(messages)
	overlap := 0
	for size := min(len(t.history), len(messages)); size > 0; size-- {
		match, reliable := true, false
		for i := 0; i < size; i++ {
			before, after := t.history[len(t.history)-size+i], input[i]
			if before.id != "" && after.id != "" && before.id == after.id {
				reliable = true
				continue
			}
			if (before.id != "" && after.id != "") || before.role != after.role || before.content != after.content {
				match = false
				break
			}
			reliable = reliable || (before.role != "" && before.role != "user" && before.role != "system")
		}
		if match && reliable {
			overlap = size
			break
		}
	}
	add(messages[overlap:])
	add(output)
	t.history = append(input, history(output)...)
	return nil
}

// Instructions remain attached to the model snapshot, where they can be
// inspected alongside the exact request.
func conversationMessages(messages []semantic.Message) []semantic.Message {
	out := make([]semantic.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role != "system" && message.Role != "developer" {
			out = append(out, message)
		}
	}
	return out
}
