package query

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pridkett/simple_agent_tracer/internal/semantic"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

const (
	maxQueryCandidates = 100000
	maxExportRows      = 10000
	maxResultBytes     = 4 << 20
)

var ErrQueryBudget = errors.New("query exceeds its scan or group budget; narrow the filters or time window")

type ModelQuery struct {
	SQL     string       `json:"sql,omitempty" jsonschema:"Read-only SELECT over model_calls. Supply either sql or from/to and structured options."`
	From    string       `json:"from,omitempty" jsonschema:"Inclusive RFC3339 model call start time."`
	To      string       `json:"to,omitempty" jsonschema:"Exclusive RFC3339 model call start time, at most 90 days after from."`
	Filters ModelFilters `json:"filters,omitempty"`
	Select  []string     `json:"select,omitempty" jsonschema:"Columns to return. Select output_messages explicitly to include captured model responses."`
	GroupBy []string     `json:"groupBy,omitempty" jsonschema:"Aggregate by served_model, requested_model, application, provider, status, currency, or day."`
	Limit   *int         `json:"limit,omitempty"`
	Cursor  string       `json:"cursor,omitempty" jsonschema:"Opaque nextCursor from the same query."`
}

type ModelFilters struct {
	Source         string          `json:"source,omitempty"`
	Service        string          `json:"service,omitempty"`
	Session        string          `json:"session,omitempty"`
	Trace          string          `json:"trace,omitempty"`
	Model          string          `json:"model,omitempty" jsonschema:"Exact match on requested or served model."`
	RequestedModel string          `json:"requestedModel,omitempty"`
	ServedModel    string          `json:"servedModel,omitempty"`
	Application    string          `json:"application,omitempty"`
	Provider       string          `json:"provider,omitempty"`
	Operation      string          `json:"operation,omitempty"`
	Status         string          `json:"status,omitempty"`
	Currency       string          `json:"currency,omitempty"`
	MinDurationMs  *float64        `json:"minDurationMs,omitempty"`
	HasOutput      *bool           `json:"hasOutput,omitempty"`
	MetadataKey    string          `json:"metadataKey,omitempty"`
	MetadataValue  json.RawMessage `json:"metadataValue,omitempty" jsonschema:"Typed JSON value to match on metadataKey."`
}

type modelRow map[string]any

type ModelResult struct {
	Columns    []string   `json:"columns"`
	Rows       []modelRow `json:"rows"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
}

var defaultModelColumns = []string{
	"trace_id", "span_id", "session_id", "turn_id", "started_at", "duration_ms", "time_to_first_chunk_ms",
	"requested_model", "served_model", "provider", "operation", "application", "source", "service", "status",
	"background", "proxy_request_id", "usage", "cost_amount", "currency", "cost_provenance", "has_output", "partial", "truncated",
}

var groupColumns = []string{"served_model", "requested_model", "application", "provider", "status", "currency", "day"}
var aggregateColumns = []string{"calls", "tokens", "reported_cost", "known_cost_calls", "p95_duration_ms", "known_duration_calls", "p95_time_to_first_chunk_ms", "known_first_chunk_calls"}

type modelPlan struct {
	request     ModelQuery
	filter      storage.Filter
	columns     []string
	aliases     []string
	order       []groupOrder
	aggregate   bool
	limit       int
	after       *storage.Position
	fingerprint string
}

func invalidQuery(message string) error {
	return fmt.Errorf("%w: %s", storage.ErrInvalid, message)
}

func (q ModelQuery) prepare(project string, maxPage int) (modelPlan, error) {
	p := modelPlan{request: q}
	if q.SQL != "" {
		if q.From != "" || q.To != "" || len(q.Select) != 0 || len(q.GroupBy) != 0 || !emptyFilters(q.Filters) {
			return p, invalidQuery("sql cannot be combined with structured query options")
		}
		var err error
		q, p.aliases, p.order, err = parseModelSQL(q)
		if err != nil {
			return p, err
		}
	}
	p.request = q
	f := q.Filters
	values := url.Values{
		"from": {q.From}, "to": {q.To}, "source": {f.Source}, "service": {f.Service}, "session": {f.Session},
		"trace": {f.Trace}, "model": {f.Model}, "operation": {f.Operation}, "status": {f.Status}, "metadataKey": {f.MetadataKey},
	}
	if len(f.MetadataValue) != 0 {
		values.Set("metadataValue", string(f.MetadataValue))
	}
	var err error
	p.filter, err = Filters(values)
	if err != nil || p.filter.From == nil || p.filter.To == nil || *p.filter.To-*p.filter.From > int64(90*24*time.Hour) {
		return p, invalidQuery("supply valid from and to timestamps within 90 days and valid filters")
	}
	for _, value := range []string{f.RequestedModel, f.ServedModel, f.Application, f.Provider, f.Currency} {
		if len(value) > 1024 || strings.ContainsRune(value, 0) {
			return p, invalidQuery("invalid filter value")
		}
	}
	if f.Currency != "" && (len(f.Currency) != 3 || strings.Trim(f.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "") {
		return p, invalidQuery("currency must be a three-letter uppercase code")
	}
	if f.MinDurationMs != nil && (*f.MinDurationMs < 0 || math.IsNaN(*f.MinDurationMs) || math.IsInf(*f.MinDurationMs, 0)) {
		return p, invalidQuery("minDurationMs must be finite and nonnegative")
	}
	p.limit = min(50, maxPage)
	if maxPage == maxExportRows {
		p.limit = maxPage
	}
	if q.Limit != nil {
		if *q.Limit < 1 || *q.Limit > maxPage {
			return p, invalidQuery(fmt.Sprintf("limit must be between 1 and %d", maxPage))
		}
		p.limit = *q.Limit
	}
	groups := map[string]bool{}
	for _, column := range q.GroupBy {
		if groups[column] || !slices.Contains(groupColumns, column) {
			return p, invalidQuery("invalid or repeated groupBy column")
		}
		groups[column] = true
	}
	p.columns = q.Select
	p.aggregate = len(groups) != 0
	for _, column := range p.columns {
		p.aggregate = p.aggregate || slices.Contains(aggregateColumns, column) || column == "cost_total"
	}
	if len(p.columns) == 0 {
		p.columns = defaultModelColumns
		if p.aggregate {
			p.columns = append(slices.Clone(q.GroupBy), aggregateColumns...)
		}
	}
	if len(p.aliases) == 0 {
		p.aliases = p.columns
	}
	if len(p.columns) > 32 {
		return p, invalidQuery("too many selected columns")
	}
	seen := map[string]bool{}
	for i, column := range p.columns {
		allowed := slices.Contains(defaultModelColumns, column) || column == "day" || column == "output_messages"
		if p.aggregate {
			allowed = groups[column] || slices.Contains(aggregateColumns, column) || column == "cost_total" && (groups["currency"] || f.Currency != "")
		}
		alias := p.aliases[i]
		if !allowed || seen[alias] || len(alias) > 64 || strings.ContainsRune(alias, 0) {
			return p, invalidQuery("invalid, incompatible, or repeated selected column: " + column)
		}
		seen[alias] = true
	}
	if p.aggregate && q.Cursor != "" {
		return p, invalidQuery("aggregate queries do not accept a cursor")
	}
	q.Cursor, q.Limit = "", nil
	raw, err := json.Marshal(struct {
		ModelQuery
		Aliases []string
	}{q, p.aliases})
	if err != nil {
		return p, invalidQuery("invalid filters")
	}
	sum := sha256.Sum256(append([]byte(project+"\x00"), raw...))
	p.fingerprint = hex.EncodeToString(sum[:16])
	if p.request.Cursor != "" {
		p.after, err = p.decodeCursor(p.request.Cursor)
	}
	return p, err
}

func emptyFilters(f ModelFilters) bool {
	if len(f.MetadataValue) != 0 {
		return false
	}
	return f.Source == "" && f.Service == "" && f.Session == "" && f.Trace == "" && f.Model == "" &&
		f.RequestedModel == "" && f.ServedModel == "" && f.Application == "" && f.Provider == "" &&
		f.Operation == "" && f.Status == "" && f.Currency == "" && f.MinDurationMs == nil &&
		f.HasOutput == nil && f.MetadataKey == ""
}

type modelCursor struct {
	Query string `json:"query"`
	storage.Position
}

func (p modelPlan) cursor(pos storage.Position) string {
	raw, _ := json.Marshal(modelCursor{p.fingerprint, pos})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (p modelPlan) decodeCursor(encoded string) (*storage.Position, error) {
	if len(encoded) <= 512 {
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		var cursor modelCursor
		if err == nil && json.Unmarshal(raw, &cursor) == nil && cursor.Query == p.fingerprint &&
			len(cursor.Trace) == 32 && len(cursor.ID) == 16 && cursor.Time >= *p.filter.From && cursor.Time < *p.filter.To {
			_, traceErr := hex.DecodeString(cursor.Trace)
			_, spanErr := hex.DecodeString(cursor.ID)
			if traceErr == nil && spanErr == nil {
				return &cursor.Position, nil
			}
		}
	}
	return nil, invalidQuery("cursor does not match this query")
}

type capturedCall struct {
	Start         int64           `json:"start,string"`
	End           int64           `json:"end,string"`
	Source        string          `json:"source"`
	Service       string          `json:"service"`
	Status        string          `json:"status"`
	Operation     string          `json:"operation"`
	Provider      string          `json:"provider"`
	RequestModel  string          `json:"requestModel"`
	ResponseModel string          `json:"responseModel"`
	Application   string          `json:"application"`
	ProxyRequest  string          `json:"proxyRequestId"`
	Usage         semantic.Usage  `json:"usage"`
	Output        json.RawMessage `json:"output"`
	Messages      json.RawMessage `json:"messages"`
	Attributes    struct {
		FirstChunk json.RawMessage `json:"gen_ai.response.time_to_first_chunk"`
	} `json:"attributes"`
	Partial   bool `json:"partial"`
	Truncated bool `json:"truncated"`
}

func (c capturedCall) output() (messages []semantic.Message, err error) {
	messages = []semantic.Message{}
	if len(c.Output) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(c.Output))
		decoder.UseNumber()
		if err = decoder.Decode(&messages); err != nil || len(messages) > 0 {
			return messages, err
		}
	}
	if len(c.Messages) != 0 {
		var events []semantic.Message
		decoder := json.NewDecoder(bytes.NewReader(c.Messages))
		decoder.UseNumber()
		if err = decoder.Decode(&events); err != nil {
			return nil, err
		}
		for _, message := range events {
			if message.Role == "assistant" {
				messages = append(messages, message)
			}
		}
	}
	if messages == nil {
		messages = []semantic.Message{}
	}
	return messages, nil
}

func (c capturedCall) duration() *float64 {
	if c.Start == 0 || c.End == 0 || c.End < c.Start {
		return nil
	}
	duration := float64(c.End-c.Start) / 1e6
	return &duration
}

func (c capturedCall) firstChunk() *float64 {
	if len(c.Attributes.FirstChunk) == 0 || c.Attributes.FirstChunk[0] == '"' || bytes.Equal(c.Attributes.FirstChunk, []byte("null")) {
		return nil
	}
	seconds, err := strconv.ParseFloat(string(c.Attributes.FirstChunk), 64)
	if err != nil || seconds < 0 || math.IsInf(seconds*1000, 0) {
		return nil
	}
	ms := seconds * 1000
	return &ms
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (c capturedCall) dimensions(row storage.ModelCall) modelRow {
	return modelRow{
		"served_model": nullable(c.ResponseModel), "requested_model": nullable(c.RequestModel),
		"application": nullable(c.Application), "provider": nullable(c.Provider), "status": c.Status,
		"currency": nullable(c.Usage.Currency), "day": time.Unix(0, row.Time).UTC().Format(time.DateOnly),
	}
}

func (c capturedCall) values(row storage.ModelCall, output []semantic.Message) modelRow {
	fields := c.dimensions(row)
	fields["trace_id"], fields["span_id"], fields["session_id"], fields["turn_id"] = row.Trace, row.ID, nullable(row.Session), nullable(row.Turn)
	fields["started_at"] = time.Unix(0, row.Time).UTC().Format(time.RFC3339Nano)
	fields["duration_ms"], fields["time_to_first_chunk_ms"] = c.duration(), c.firstChunk()
	fields["operation"], fields["source"], fields["service"] = nullable(c.Operation), c.Source, nullable(c.Service)
	fields["proxy_request_id"], fields["background"] = nullable(c.ProxyRequest), row.Background
	usage := modelRow{}
	for _, field := range tokenFields(&c.Usage) {
		if *field.value == nil {
			usage[field.name] = nil
		} else {
			usage[field.name] = strconv.FormatInt(**field.value, 10)
		}
	}
	usage["provenance"] = c.Usage.Provenance
	fields["usage"] = usage
	fields["cost_amount"], fields["cost_provenance"] = nullable(c.Usage.Amount), nullable(c.Usage.Provenance["amount"])
	fields["output_messages"], fields["has_output"] = output, len(output) > 0
	fields["partial"], fields["truncated"] = c.Partial, c.Truncated
	return fields
}

func (p modelPlan) project(fields modelRow) modelRow {
	row := make(modelRow, len(p.columns))
	for i, column := range p.columns {
		row[p.aliases[i]] = fields[column]
	}
	return row
}

func (p modelPlan) scan(ctx context.Context, store *storage.Store, project string, scanned *int, visit func(storage.ModelCall, capturedCall, []semantic.Message) (bool, error)) error {
	f := p.request.Filters
	includeOutput := f.HasOutput != nil || slices.Contains(p.columns, "has_output") || slices.Contains(p.columns, "output_messages")
	return store.ModelCalls(ctx, project, p.filter, f.Application, f.RequestedModel, f.ServedModel, p.after, maxQueryCandidates-*scanned+1,
		func(row storage.ModelCall) (bool, error) {
			(*scanned)++
			if *scanned > maxQueryCandidates {
				return false, ErrQueryBudget
			}
			var call capturedCall
			decoder := json.NewDecoder(strings.NewReader(row.Payload))
			decoder.UseNumber()
			if err := decoder.Decode(&call); err != nil {
				return false, err
			}
			if f.Provider != "" && f.Provider != call.Provider || f.Currency != "" && f.Currency != call.Usage.Currency {
				return true, nil
			}
			if f.MinDurationMs != nil {
				d := call.duration()
				if d == nil || *d < *f.MinDurationMs {
					return true, nil
				}
			}
			var output []semantic.Message
			if includeOutput {
				var err error
				output, err = call.output()
				if err != nil {
					return false, err
				}
			}
			if f.HasOutput != nil && *f.HasOutput != (len(output) > 0) {
				return true, nil
			}
			if err := fillUsage(&call.Usage, json.RawMessage(row.Fallback)); err != nil {
				return false, err
			}
			return visit(row, call, output)
		})
}

func (q ModelQuery) Run(ctx context.Context, store *storage.Store, project string, maxPage, maxBytes int) (ModelResult, error) {
	p, err := q.prepare(project, maxPage)
	if err != nil {
		return ModelResult{}, err
	}
	result := ModelResult{Columns: p.aliases, Rows: []modelRow{}}
	if p.aggregate {
		return p.aggregateRows(ctx, store, project, result)
	}
	if maxBytes == 0 {
		maxBytes = maxResultBytes
	}
	scanned := 0
	result.Rows, _, result.NextCursor, err = p.readPage(ctx, store, project, p.limit, maxBytes, &scanned)
	return result, err
}

func (p modelPlan) readPage(ctx context.Context, store *storage.Store, project string, limit, maxBytes int, scanned *int) ([]modelRow, storage.Position, string, error) {
	rows := []modelRow{}
	var last storage.Position
	var next string
	size := 0
	err := p.scan(ctx, store, project, scanned, func(row storage.ModelCall, call capturedCall, output []semantic.Message) (bool, error) {
		if len(rows) == limit {
			next = p.cursor(last)
			return false, nil
		}
		item := p.project(call.values(row, output))
		raw, err := json.Marshal(item)
		if err != nil {
			return false, err
		}
		if len(rows) != 0 && size+len(raw)+1 > maxBytes {
			next = p.cursor(last)
			return false, nil
		}
		size += len(raw) + 1
		last = row.Position
		rows = append(rows, item)
		return true, nil
	})
	return rows, last, next, err
}

// Export releases the database connection between batches before writing to a client.
func (q ModelQuery) Export(ctx context.Context, store *storage.Store, project string, emit func(any) error) (string, error) {
	p, err := q.prepare(project, maxExportRows)
	if err != nil {
		return "", err
	}
	if p.aggregate {
		return "", invalidQuery("export requires a row query")
	}
	remaining, scanned := p.limit, 0
	for {
		rows, last, next, err := p.readPage(ctx, store, project, min(remaining, 100), maxResultBytes, &scanned)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			if err := emit(row); err != nil {
				return "", err
			}
		}
		remaining -= len(rows)
		if remaining == 0 || next == "" {
			return next, nil
		}
		p.after = &last
	}
}

type modelGroup struct {
	fields               modelRow
	costAndTokens        *Accumulator
	duration, firstChunk []float64
	calls, knownCost     int
}

func (p modelPlan) aggregateRows(ctx context.Context, store *storage.Store, project string, result ModelResult) (ModelResult, error) {
	groups := map[string]*modelGroup{}
	if len(p.request.GroupBy) == 0 {
		groups["[]"] = &modelGroup{fields: modelRow{}, costAndTokens: NewAccumulator()}
	}
	scanned := 0
	err := p.scan(ctx, store, project, &scanned, func(row storage.ModelCall, call capturedCall, _ []semantic.Message) (bool, error) {
		values := []any{}
		dimensions := call.dimensions(row)
		for _, column := range p.request.GroupBy {
			values = append(values, dimensions[column])
		}
		raw, _ := json.Marshal(values)
		key := string(raw)
		group := groups[key]
		if group == nil {
			if len(groups) >= 1000 {
				return false, ErrQueryBudget
			}
			group = &modelGroup{fields: dimensions, costAndTokens: NewAccumulator()}
			groups[key] = group
		}
		group.calls++
		if call.Usage.Amount != "" {
			group.knownCost++
		}
		if d := call.duration(); d != nil {
			group.duration = append(group.duration, *d)
		}
		if d := call.firstChunk(); d != nil {
			group.firstChunk = append(group.firstChunk, *d)
		}
		return true, group.costAndTokens.addUsage(call.Usage)
	})
	if err != nil {
		return ModelResult{}, err
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		g := groups[key]
		fields := g.fields
		fields["calls"], fields["tokens"], fields["reported_cost"] = g.calls, g.costAndTokens.Tokens, g.costAndTokens.Costs
		fields["known_cost_calls"] = g.knownCost
		currency, _ := fields["currency"].(string)
		if currency == "" {
			currency = p.request.Filters.Currency
		}
		fields["cost_total"] = nullable(g.costAndTokens.Costs[currency].Value)
		fields["p95_duration_ms"], fields["known_duration_calls"] = percentile95(g.duration), len(g.duration)
		fields["p95_time_to_first_chunk_ms"], fields["known_first_chunk_calls"] = percentile95(g.firstChunk), len(g.firstChunk)
	}
	slices.SortStableFunc(keys, func(a, b string) int {
		for _, order := range p.order {
			left, right := groups[a].fields[order.column], groups[b].fields[order.column]
			if left == nil || right == nil {
				if left == right {
					continue
				}
				if left == nil {
					return 1
				}
				return -1
			}
			n := compareGroupValues(left, right, order.column)
			if n != 0 {
				if order.desc {
					return -n
				}
				return n
			}
		}
		return 0
	})
	if len(keys) > p.limit {
		result.Truncated = true
		keys = keys[:p.limit]
	}
	for _, key := range keys {
		result.Rows = append(result.Rows, p.project(groups[key].fields))
	}
	return result, nil
}

func percentile95(values []float64) any {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	return values[(95*len(values)+99)/100-1]
}

func compareGroupValues(left, right any, column string) int {
	switch a := left.(type) {
	case int:
		return cmp.Compare(a, right.(int))
	case float64:
		return cmp.Compare(a, right.(float64))
	case string:
		b := right.(string)
		if column == "cost_total" {
			x, _ := new(big.Rat).SetString(a)
			y, _ := new(big.Rat).SetString(b)
			return x.Cmp(y)
		}
		return cmp.Compare(a, b)
	default:
		return 0
	}
}
