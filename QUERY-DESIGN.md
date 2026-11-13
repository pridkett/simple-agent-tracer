# Bulk model-call queries

The tracer already lets users find sessions and traces, inspect individual spans, and export a single session or trace. Bulk queries return the matching model calls directly across a project. Typical workflows include exporting model responses for evaluation, measuring usage and costs by model, and identifying slow calls.

## Data model

The logical table is `model_calls`. Each row represents a model span that the tracer counts as a call. When ingestion correlates a client span with a proxy inference span, the proxy span represents the call. The existing token fallback can fill compatible missing fields from a uniquely linked client span. Background operations and failed calls are included when they have a model span. The project is resolved from the endpoint and its credentials.

| Columns | Meaning |
| --- | --- |
| `trace_id`, `span_id`, `session_id`, `turn_id`, `proxy_request_id` | Source identifiers. Session IDs use the canonical alias when available. The proxy request ID can correlate with proxy accounting. |
| `started_at`, `day` | UTC RFC3339 start time, with receipt time as a fallback. The day is the corresponding UTC calendar date. |
| `duration_ms`, `time_to_first_chunk_ms` | Duration from valid span timestamps and first-chunk latency from the recorded `gen_ai.response.time_to_first_chunk` attribute. Both are nullable. |
| `requested_model`, `served_model`, `provider`, `operation`, `application`, `source`, `service`, `status`, `background` | Recorded call dimensions. |
| `usage` | Input, output, cache read, cache write, reasoning, and total token fields. Known values are decimal integer strings. Unknown values are null. Includes attribute provenance. |
| `cost_amount`, `currency`, `cost_provenance` | Recorded cost as a decimal string, three-letter currency, and the source attribute. A missing cost is null. |
| `has_output`, `partial`, `truncated` | Availability of retained model messages and the canonical span’s capture flags. |
| `output_messages` | Explicitly selected array of normalized captured output in producer order. Structured message parts can include text and tool calls. Uses retained assistant events when an output snapshot is absent. An empty array means the tracer has no output to return for the call. |

The default projection includes the call columns except `day` and `output_messages`. Capture settings and retention determine which data is available. Capture policy applies at ingest time. Results reuse the stored payload and inherit its redaction and truncation. Reading message bodies requires the same project read access as span detail.

## REST and MCP

Use `POST /api/v1/projects/{project}/query` with an existing project read bearer token, an administrator session, or local access. The response contains `columns` and `rows`. Continue a row query by sending its `nextCursor` with the same query. The project and query shape are bound to the cursor. Pages sort by descending start time, then trace and span ID. Pages read retained data as of each request, so concurrent ingest, correlation, and retention can change the results.

For agents, configure a Streamable HTTP MCP server at `/api/v1/projects/{project}/mcp` with the same bearer token. The read-only tool `query_model_calls` takes the same JSON arguments and returns the same result shape. Each MCP request is authorized for the project. For browser-based sign-in from a remote MCP client, see [OAuth setup](QUERY-OAUTH.md).

### Structured request

The `from` and `to` fields are RFC3339 timestamps. The start is inclusive and the end is exclusive. Filters apply to the same canonical call. Supply `source`, `service`, `session`, `trace`, `operation`, `model`, `requestedModel`, `servedModel`, `application`, `provider`, `status`, `currency`, `minDurationMs`, `hasOutput`, or `metadataKey` with an optional typed `metadataValue`. Model names match exactly. `model` matches either the requested or served name.

```json
{
  "from": "2026-11-01T00:00:00Z",
  "to": "2026-11-02T00:00:00Z",
  "filters": {"model": "example-model", "minDurationMs": 2000, "hasOutput": true},
  "select": ["trace_id", "span_id", "duration_ms", "cost_amount", "currency", "output_messages"],
  "limit": 25
}
```

Set `groupBy` to one or more of `served_model`, `requested_model`, `application`, `provider`, `status`, `currency`, or `day`. The default aggregate projection includes the grouping columns and the following metrics. Selecting aggregate columns without `groupBy` returns totals for the matching calls.

| Metric | Meaning |
| --- | --- |
| `calls` | Number of matching calls. |
| `tokens` | Known token sums by token category, each with its known-call count. |
| `reported_cost`, `known_cost_calls` | Exact decimal sums by currency with per-currency coverage, and the overall count of calls with known cost. |
| `p95_duration_ms`, `known_duration_calls` | Nearest-rank p95 over measured durations and the measurement count. |
| `p95_time_to_first_chunk_ms`, `known_first_chunk_calls` | The same statistics for recorded first-chunk latency. |

### SQL request

Send a `sql` string in place of the structured query options. The server parses one statement into the same query plan. The supported grammar covers `SELECT`, aliases, `FROM model_calls`, AND-connected predicates, `GROUP BY`, `ORDER BY`, and `LIMIT`. Predicates support the structured string filters other than typed metadata, the required `started_at >=` and `started_at <` bounds, `duration_ms >=`, and `has_output = true|false`. Row sorting is fixed at `started_at DESC` with stable tie-breakers. Aggregates may sort by group columns or selected scalar metrics and aliases. Joins, subqueries, arbitrary functions, writes, and offset pagination are outside this dialect.

```sql
SELECT served_model, COUNT(*) AS calls,
       SUM(cost_amount) AS cost, P95(duration_ms) AS p95_ms
FROM model_calls
WHERE started_at >= '2026-11-01T00:00:00Z'
  AND started_at < '2026-11-08T00:00:00Z'
  AND currency = 'USD'
GROUP BY served_model
ORDER BY cost DESC
LIMIT 20
```

`COUNT(*)`, `COUNT(cost_amount)`, `COUNT(duration_ms)`, `COUNT(time_to_first_chunk_ms)`, and `P95` of either latency field are supported. `SUM(cost_amount)` requires a currency filter or a currency grouping. Use `reported_cost` for sums across currencies in one row. SQL limit and the JSON limit field are mutually exclusive. Follow the returned JSON cursor for more row results. Aggregate results are bounded by the limit and signal omitted groups with `truncated: true`.

### NDJSON export

Send the same row query to `POST /api/v1/projects/{project}/query/export`. The default and maximum are 10,000 rows per request. The response is `application/x-ndjson` with a `{"type":"row","row":{...}}` record per result and a final `{"type":"complete"}` record. Follow `nextCursor` on the completion record to continue. A terminal `error` record or a missing `complete` record means the export did not finish.

## Storage and bounds

The query uses the existing span and metadata tables on SQLite and PostgreSQL. A partial index on project and model call start order applies to existing and new spans. Database predicates use bound parameters, and SQL text supplied by a caller never reaches the database. The query service applies payload-dependent filters and calculates decimal totals and percentiles in Go for backend parity. No separate copy of captured messages is maintained.

The query accepts a time window of up to 90 days, scans up to 100,000 candidates per request, limits aggregation to 1,000 distinct groups, and has a 15-second query deadline. REST defaults to 50 rows and caps pages at 200. MCP caps pages at 50. Row pages may end earlier based on their encoded size. Export reads bounded batches and releases the database connection between batches before writing to the client.

A later iteration can add individual output-message rows and content search. First-chunk coverage depends on producer instrumentation. Costs cover the model calls and amounts retained by the tracer. Proxy accounting records can be joined externally by request ID when reconciliation is needed.
