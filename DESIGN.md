# Simple Agent Tracer Design Specification

Build a self-hosted OpenTelemetry trace viewer for debugging agent conversations. The main workflow is to open a session, find the relevant turn or model call, and inspect the messages, tools, timings, errors, token usage, and reported cost behind it. A single static Go binary serves the UI, the query API, and the trace receiver. SQLite provides a standalone installation, and PostgreSQL supports installations that already run it.

## Release scope

The first release supports existing `simple_llm_proxy` exports and richer Pi and OpenCode runs routed through that proxy. Proxy-only traffic works through configuration. The Pi extension and OpenCode plugin add the user-visible turn boundaries and tool activity. Both storage engines, local and OIDC administrator access, metadata filtering, project retention, and trace and session preservation ship in the first release.

Pydantic AI is the next integration and uses native OTLP. Direct-provider capture for Pi and OpenCode is follow-up work. The roadmap also leaves room for an MCP server, OIDC group-to-project access, additional OTLP transports, and a model price catalog. Evaluation frameworks, prompt libraries, and experiment tooling are outside this release.

## Runtime and code layout

- Use Go, `chi`, the generated OTLP protobuf types, `modernc.org/sqlite`, and `pgx`. Keep ingest, semantic mapping, query and retention logic, storage adapters, and HTTP handlers in separate packages. Keep database selection at startup and use explicit versioned migrations for each backend.
- Use Vue 3, Vue Router, Vite, and Tailwind. Follow the proxy's gray and indigo light and dark themes, compact tables, filter controls, and detail panels. Reference its `frontend/src/style.css` and `frontend/package.json` in the sibling checkout. A span waterfall can use CSS and the existing layout primitives.
- Build frontend assets in a separate stage and embed them with `go:embed`. Build the server with `CGO_ENABLED=0` and run it as a numeric non-root user in a `scratch` image with a CA bundle. Document the writable SQLite data directory and its ownership requirements. The PostgreSQL deployment uses a database DSN. The default runtime needs no additional service beyond its configured database.
- Keep exporter dependencies on the agent side. The server receives OTLP over HTTP and does not require an OpenTelemetry Collector distribution. A separately operated Collector can forward to it.

## Entities and identity

| Entity | Identity and behavior |
|---|---|
| Project | Server-assigned ID and unique slug. Owns retention, capture policy, and credentials. A default project makes local setup simple. A request's credential or configured local default selects the project. Exported attributes cannot change it. |
| Source | A configured producer identity within a project, such as a particular proxy installation or agent exporter. Separate credentials can write to the same project. Use source identity for namespacing and diagnostics. Preserve resource and instrumentation scope metadata. |
| Session | A conversation or workflow across calls and turns. An external ID is qualified by project and producer namespace. Proxy session IDs are already scoped to an application within the proxy installation. A durable internal ID identifies the session in UI and API links. |
| Turn | One user submission and its resulting agent activity, or one top-level unattended invocation. Qualify its producer turn ID with the source and session. An explicit turn can refer to one or more traces or a subtree of a trace. A session can contain calls with no known turn. |
| Trace | The project and OTLP trace ID. A trace contains a tree or forest of spans and may arrive over several exports. Trace IDs are shared across sources when propagation succeeds. |
| Span | The project, trace ID, and OTLP span ID. Preserve parent ID, links, events, status, timestamps, typed attributes, resource, scope, and ingest provenance. A span can be inspectable before its parent arrives. |
| Message | An ordered role and content record with source span and stable producer message ID when available. Preserve text and structured tool calls and results, including call IDs. Keep model input and output snapshots accessible from the span. |
| Model usage | A record attached to a model operation. Holds nullable input, output, cache, reasoning, and reported total token counts, reported monetary amount and currency, and the provenance of each value. |

### Correlation and transcript rules

For generic OTLP spans, resolve `gen_ai.conversation.id`, OpenInference `session.id`, then `langfuse.session.id` within their source namespace. Equal aliases on a span can describe the same identity. For conflicting IDs preserve the observed values and use the selected mapping for that producer. Keep ambiguous memberships visible in trace detail. Generic telemetry without a usable session identifier remains discoverable in Traces.

For Pi and OpenCode, define a small instrumentation contract. Use a stable native session ID as `gen_ai.conversation.id`, a root `invoke_agent` span per turn, and the native run or user-message ID as `gen_ai.agent.call.id` when available. Mint a unique ID once for a Pi run if the harness has no durable run ID. Give tool and model-request spans the turn as an ancestor. Keep agent-loop iterations inside the user-visible turn. Where OpenCode identifies a distinct title, compaction, or background operation, keep its operation visible and attach it to a turn only when the producer supplies that relationship.

Trust shared trace IDs and parent IDs to correlate the exported agent request span with the proxy HTTP and model spans. Both sources must write to the same tracer project. The client sends the original native session ID to the proxy, which exports its application-scoped digest. When a matched agent and proxy request supplies a single unambiguous pair of session IDs, record their relationship within that project and proxy application. Prefer the agent's turn membership for the linked call and keep both external IDs searchable. Keep a stable lookup for the earlier proxy session link if the richer session is learned later. Surface conflicts such as several agent sessions claiming the same proxy session and require unambiguous evidence before merging. A shared timestamp or matching message text alone is insufficient evidence.

For proxy-only sessions, display calls in span start order, using server receive time when the producer timestamp is absent and the trace and span IDs to break ties. Use the model span's input and output snapshots for the best-effort transcript. Prefer actual agent message events or stable message IDs when available. Compare overlapping ordered histories within a session to suppress repeated context, keeping distinct user messages that happen to have identical text. If snapshots are truncated, missing, or inconsistent, expose the individual calls with their captured content. Assign turn numbers only where the producer supplied a turn boundary. Arrival order can differ from execution order during streaming or export retries. Add an optional proxy request sequence only if release tests expose a practical need. Assign it at request acceptance and scope it to the producer instance and boot if process-local.

### Token and cost accounting

- Prefer one usage record per actual model call. Ignore usage on orchestration spans when they report a rollup of their children. A provider retry represented as an event does not create an additional usage record. Distinct model calls with usage remain individually countable.
- When a propagated request span and proxy inference span describe the same call, count one operation. Prefer the proxy's reported values. A linked agent field can fill a missing value only when its meaning and units match. In proxied mode the adapters should leave billing fields off their proxy request spans. Unlinked observations with reported usage remain inspectable and retain their provenance.
- Preserve unknowns as null and explicit zero as zero. Show totals with coverage when some counted calls lack usage or price. Cache and reasoning fields that are subsets of a reported input or output total remain breakdowns. When a producer reports only a total, preserve it and leave the component fields unknown.
- Carry through reported amounts, including `llmproxy.cost.total_usd` in USD. Store monetary values with decimal semantics and aggregate separately by currency. The initial release uses reported cost and omits estimates when none is reported.

## Ingest and persistence

Accept `POST /v1/traces` with `application/x-protobuf`, including gzip from standard exporters. Use standard OTLP success and partial-success responses. Bound compressed and decoded request size, spans per request, attribute values, and concurrent ingestion. Start with an 8 MiB request limit, 16 MiB decoded limit, and 2,000 spans per batch. Document defaults and make limits configurable if actual producer fixtures need tuning. Return a permanent client error for an invalid envelope or oversized request, and a retryable response for a temporary storage failure. A partial success may reject invalid spans while committing valid spans. Acknowledge accepted spans after the database transaction commits. Follow the OTLP status and response body rules. [OTLP specification](https://opentelemetry.io/docs/specs/otlp/)

Validate trace and span IDs, decode resource and scope records, and retain timestamps, events, links, and attribute types. Parse GenAI message attributes whether an exporter sends structured values or a JSON string. Support OpenInference indexed message fields and common session, span kind, model, and token fields. Handle the proxy's `langfuse.trace.metadata.*` attribution and `llmproxy.capture.*` flags in its mapping. Keep mapping code fixture-driven and expose unmapped spans through the general trace view. Mapping errors in optional fields leave the span available with its observed attributes and a diagnostic. [OpenTelemetry GenAI conventions](https://github.com/open-telemetry/semantic-conventions-genai) · [OpenInference conventions](https://github.com/Arize-ai/openinference/blob/main/spec/semantic_conventions.md)

| Field | Initial mappings |
|---|---|
| Operation and provider | `gen_ai.operation.name`, `gen_ai.provider.name`, `openinference.span.kind`. Keep OTLP span kind independently. |
| Model | `gen_ai.request.model`, `gen_ai.response.model`, `llm.model_name`. Keep requested and served names separately when both are available. |
| Usage | `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, the reported GenAI cache and reasoning breakdowns, and OpenInference `llm.token_count.prompt`, `llm.token_count.completion`, and `llm.token_count.total`. |
| Conversation data | `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`, and OpenInference `llm.input_messages.*` and `llm.output_messages.*`. Preserve ordering and role. |
| Attribution | `service.name` and other resource attributes, proxy team and app metadata, `user.id`, and the original producer metadata. Preserve the original types for API filters. |

Persist typed source attributes subject to the project capture policy. Default to storing supplied text, instructions, and tool content with a 64 KiB per-span normalized content cap. Flag partial or truncated capture, preserve original proxy flags, and render captured content as data. Keep binary content out of the initial data path. With project capture disabled, strip recognized message, prompt, and tool bodies before persisting them, including their source-attribute copies. Apply request and attribute limits to arbitrary metadata too. Never log exporter credentials or request bodies.

### Storage contract

- Use tables for projects, sources, sessions and aliases, traces, spans, and the normalized data needed for query and retention. Store typed attributes and nested span detail in a form that round-trips on both backends. Keep common filters and time ordering in indexed columns. Index metadata keys and values used for exact-match filtering without scanning every trace's content.
- Upsert spans on the project, trace, and span key. Re-exporting the same span is a no-op for counts and usage. For conflicting exports of the same completed span, use a deterministic rule such as retaining the first accepted version and recording a diagnostic. Resolve parents and membership as later batches arrive. Allow parents and children to commit independently.
- Maintain any persisted summaries in the same transaction as the affected spans or derive them from canonical rows. Handle concurrent exports and retention sweeps consistently. Use bounded SQLite transactions, WAL where supported, and a finite busy timeout. Bound the PostgreSQL connection pool.
- Use fixture-driven integration tests for both backends, including migrations, partial trees, replay, parallel writes, pagination, matching child-span filters, and retention. Verify equivalent API results from the same stored fixture.

## UI and query API

**Sessions** opens a time-ordered list with project, last activity, source or application, status, token usage, reported cost, and Keep state. Session detail shows the conversation alongside inspectable turns and proxy-only calls. **Traces** provides a separate entry point for background work, ungrouped traffic, and exact trace lookups. Trace detail shows the span tree and timeline with model, tool, and error indicators. Span detail presents messages and tool payloads, status, latency, usage, attributes, events, links, and raw data subject to the project's capture policy. Use stable URLs and preserve filters in the URL. Show partial and truncated content clearly.

| Route | Behavior |
|---|---|
| `GET /health`, `GET /ready`, `GET /openapi.json` | Liveness, readiness, and machine-readable contract. |
| `GET /api/v1/projects`, `POST /api/v1/projects`, `PATCH /api/v1/projects/{project}` | List and configure projects, including retention and the capture policy for new data. |
| `GET /api/v1/projects/{project}/sessions`, `GET .../sessions/{session}` | Filtered list and summary, with stable links to turn and trace detail. |
| `GET .../sessions/{session}/turns`, `GET .../sessions/{session}/turns/{turn}` | Ordered turn list and detail with transcript and trace references. The session detail or a paginated session timeline endpoint includes calls without turn membership. |
| `GET .../traces`, `GET .../traces/{trace}`, `GET .../traces/{trace}/spans`, `GET .../traces/{trace}/spans/{span}` | Filtered trace list, trace summary, paginated span tree data, and full span detail. |
| `GET .../metadata/keys`, `GET .../metadata/values` | Bounded discovery for filter controls, using the same project and access rules as detail queries. |
| `POST .../retention/keep` | Idempotent Keep or unkeep by explicit trace and session IDs. Validate the batch's project and all IDs before applying it atomically. |
| `GET .../traces/{trace}/export`, `GET .../sessions/{session}/export` | Download stored trace or session data in a documented JSON format, using bounded or streamed reads. |

The paths abbreviated with `...` use the `/api/v1/projects/{project}` prefix. Document request and response schemas, authorization, timestamp encoding, errors, pagination, and units in OpenAPI. Use UTC timestamps and opaque cursor pagination with deterministic ordering. Default list size to 50 and cap it at 200. Make sorting and filters consistent across UI and API. Start with time range, project, source or service, session ID, operation, model, status, and typed metadata exact match or presence. A trace or session matches a child-span filter when one descendant span satisfies the supplied child predicates. Keep rollup values scoped to the returned entity so a filtering match does not silently change its totals. Example query use case: find sessions containing a failed `search` tool call.

## Access and retention

Local mode exposes the UI and query API to the loopback listener without login. When running in a container, document how to bind the host-published port to loopback. For nonlocal UI access use the proxy's OIDC and session approach with `go-oidc` and SCS, using the same administrator group policy. The configured group defaults to `admin` and members can see all projects. Store no human passwords. Leave project group mappings for a later version and keep identity separate from project membership. Treat exported user identifiers as attribution. Use an ingest credential bound to a project and source, and a separate project-scoped read-only API credential for integrations. Local test ingestion can use the default project on a loopback-only receiver. For OIDC-backed mutations, verify origin or use a CSRF token and use secure session cookies with production TLS.

Start with one HTTP listener for all routes and a configurable bind address. Default to `127.0.0.1:4318`. An externally reachable listener requires authenticated ingest and queries. Provision read credentials through deployment configuration. Administrators can also issue and revoke project and source scoped ingest keys from the Projects screen. Return the secret only at creation and store only its hash and a short identifying prefix. Deployment-configured ingest credentials remain supported.

Store a positive retention period in days per project, defaulting to 30. The sweep uses the latest span end time in a trace, with ingest time as fallback, and a short grace period after the most recent accepted new span. Replayed spans do not refresh the grace period. Delete whole expired traces in bounded batches and remove their dependent content and obsolete session membership. Protect a trace when it has its own Keep flag or belongs to a kept session. Keeping a session protects current and future traces while that flag remains set. Clearing a session's flag preserves any individually kept traces. Protect empty kept sessions and remove empty unkept sessions. Test the race between Keep and the sweep. Make the sweep and pin state queryable enough to explain why an old trace remains.

## Proxy integration setup

The proxy already supports the required OTLP export, session attribution, message capture, and trusted incoming trace context. Use its existing configuration and dedicated application keys. The tracer defaults to retaining content it receives. The proxy controls which applications send content through its capture allowlist. [Proxy session attribution PR](https://github.com/pridkett/simple-llm-proxy/pull/137) · [Proxy message capture PR](https://github.com/pridkett/simple-llm-proxy/pull/138) · [Proxy trace context PR](https://github.com/pridkett/simple-llm-proxy/pull/139)

```yaml
otel_settings:
  enabled: true
  service_name: simple-llm-proxy
  sampling_ratio: 1.0
  exporter:
    protocol: http
    endpoint: http://127.0.0.1:4318
    insecure: true
    headers:
      Authorization: os.environ/TRACER_OTLP_AUTH_HEADER
  message_capture:
    application_ids: [42]
    max_bytes: 32768
  trusted_traceparent_cidrs: ["127.0.0.1/32", "::1/128"]
```

This example assumes the tracer and proxy run on the same host and the agent connects to the proxy over loopback. Substitute the real application ID, OTLP endpoint, and immediate agent or relay peer CIDRs. Set `TRACER_OTLP_AUTH_HEADER` to the full `Bearer` header value. The agent and proxy use separate ingestion credentials for the same project. When using a trusted relay, have it sanitize and inject the trace headers it forwards. Restart the proxy after changing trusted peers. For the test use recording agent spans and ensure the sampled flag is set in the injected context. [Proxy OpenTelemetry docs](https://github.com/pridkett/simple-llm-proxy#opentelemetry-traces)

The adapter creates a real outbound request span and sends its context in `traceparent`, plus the native session identifier in `X-Session-ID`, only to the configured proxy origin. On Pi use the run lifecycle and provider header hook. On OpenCode use the native session and user message identifiers and the supported request header hook for the chosen version. Verify both hook surfaces against the actual versions and provider adapters used in the release tests. Leave tool and request behavior unchanged by tracing failures. Document installation and the versions and provider configurations exercised. If an acceptance test exposes a real proxy-side gap, reproduce it with a fixture and propose the smallest change against the existing telemetry design. The sequence counter is conditional on a demonstrated ordering gap. [Pi extension API](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md) · [OpenCode plugin API](https://opencode.ai/docs/build/plugins/server/) · [OpenCode gateway integration example](https://langfuse.com/integrations/developer-tools/opencode-gateway)

## Implementation milestones and release checks

1. **Proxy vertical slice.** Build the OTLP receiver, SQLite migrations, normalization, and session and trace API. Run a two-message conversation through a real proxy using a dedicated application key. Verify message snapshots and transcript ordering, application metadata filters, reported usage and cost, and proxy request IDs. Replay a batch and deliver child spans before the parent. Confirm the same operations remain inspectable.
2. **Explorer and database parity.** Add the Vue explorer, PostgreSQL storage, typed metadata filters, exports, and OpenAPI. Exercise both backends with the same fixture. Verify large paginated sessions remain navigable, filters can match a failed child tool call, and captured text renders safely.
3. **Pi and OpenCode.** Run a two-submission conversation through each harness and the proxy. Include a turn with multiple model calls and a tool result, plus a streamed response. Verify one visible turn per submission, request ancestry, transcript content, correct cost and token aggregation, no duplicate accounting when both layers export, and no extra turn from a tool loop. Re-run with the adapter disabled and verify proxy-only sessions work with best-effort ordering. Test trusted, untrusted, and missing propagation separately. Test independent applications reusing the same session ID.
4. **Operations and footprint.** Verify project-scoped credentials, local mode, OIDC administrator sign-in, 30- and 90-day policies, bulk Keep, and export of a kept session. Build the non-root `scratch` image, check startup and TLS with its CA bundle, and document a reproducible benchmark. Report compressed image size, startup-to-ready, Go process RSS, container memory, and representative list and detail latency. Measure a fresh SQLite deployment and a warm idle process after browsing a dataset with 100,000 spans and representative captured content. Use 50 MiB idle process RSS as a provisional target. Report the database service separately for PostgreSQL. Record the hardware, fixture, duration, and observed results.
5. **Next release.** Add a native Pydantic AI fixture and setup instructions using its conversation and run identifiers. Then add direct-provider Pi and OpenCode capture with reported usage and cost from their own instrumentation. Keep the same query API and user-visible turn model. [Pydantic AI message history and identifiers](https://pydantic.dev/docs/ai/core-concepts/message-history/#correlating-runs-with-run_id-and-conversation_id)

## Product references

- Langfuse and Arize AX use session IDs to group traces and show chat turns. Braintrust demonstrates turns as spans within a larger trace. Support both shapes by representing turns separately from traces. [Langfuse tracing practices](https://langfuse.com/docs/observability/best-practices) · [Arize AX sessions](https://arize.com/docs/ax/instrument/set-up-sessions) · [Braintrust multi-turn tracing](https://www.braintrust.dev/blog/multi-turn-scoring)
- Arize Phoenix already supports SQLite and PostgreSQL and is a useful self-hosted comparison for footprint and startup. LangSmith's child-span filters inform the query contract. [Phoenix self-hosting](https://arize.com/docs/phoenix/self-hosting/deploying-phoenix) · [LangSmith trace filters](https://docs.langchain.com/langsmith/filter-traces-in-application)
