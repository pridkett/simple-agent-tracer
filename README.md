# Simple Agent Tracer

A self-hosted OpenTelemetry trace viewer for agent conversations. Open a session to read its transcript and inspect turns, model calls, tool activity, token counts, and reported cost. Open a trace to inspect the span tree and waterfall, including traffic without a session.

## Run locally

Build the embedded binary with Go 1.26 and Node.js 22 or later.

```sh
npm --prefix frontend ci
npm --prefix frontend run build
CGO_ENABLED=0 go build -trimpath -tags frontend -ldflags="-s -w" -o tracer ./cmd/tracer
./tracer
```

Open [the local explorer](http://127.0.0.1:4318). The default listener is `127.0.0.1:4318` and the default database is `tracer.db`. The `default` project starts with 30 days of retention and content capture enabled. A local request without an ingest credential writes to the `default` project and the `local` source. Supply an ingest credential to give each producer a distinct namespace.

With `TRACER_OIDC_ISSUER` unset, the browser opens as a local administrator without signing in. Set `TRACER_AUTH_MODE=local` to select this mode explicitly. Local mode requires a loopback IP bind and accepts browser requests from a loopback peer with a loopback Host and a matching Origin when present. Keep the unauthenticated listener on your machine.

`GET /health` checks the process. `GET /ready` checks database connectivity. `GET /openapi.json` describes the query API, timestamp and token encodings, pagination, and the versioned JSON export format. The explorer supports the same filters as the API and keeps filters in the URL. For example, `?tool=search&status=STATUS_CODE_ERROR` finds sessions with a failed `search` span. A typed metadata exact match uses a JSON literal in `metadataValue`. Use `1.0` to match a double and `1` to match an integer.

## Configure deployment

| Setting | Behavior |
| --- | --- |
| `TRACER_BIND` or `-bind` | HTTP address. Defaults to `127.0.0.1:4318`. |
| `TRACER_DATABASE_URL` or `-database` | SQLite path or `file:` URI, or a `postgres://` or `postgresql://` DSN. |
| `TRACER_PROJECTS` | Comma-separated project slugs to provision at startup. Existing project settings take precedence. |
| `TRACER_AUTH_MODE` | `auto` uses local access for loopback IP binds without OIDC. `authenticated` requires a read credential or OIDC on any bind and authenticates every ingest request. `local` requires a loopback IP bind and an unset OIDC issuer. |
| `TRACER_CREDENTIALS` | JSON array of deployment-managed bearer credentials. Each token needs at least 24 characters. Give each ingest token a project and source. Give each read token a project. OIDC deployments can create ingest keys in the UI without setting this variable. |
| `TRACER_OIDC_ADMIN_GROUP` | Exact group name from the verified OIDC ID token. Defaults to `admin`, matching the proxy. |

For PostgreSQL, give the database role permission to create the schema objects. Startup applies versioned migrations. The tracer uses at most 10 connections per process. Use a TLS DSN such as `sslmode=verify-full` for network deployments. Changing the DSN starts the tracer against the selected database. SQLite files and PostgreSQL databases are independent.

For deployment-managed credentials, replace the three placeholders in this example with separate random tokens.

```json
[
  {"access":"ingest","project":"default","source":"proxy","token":"replace-with-a-random-proxy-token"},
  {"access":"ingest","project":"default","source":"pi","token":"replace-with-a-random-pi-token"},
  {"access":"read","project":"default","token":"replace-with-a-random-read-token"}
]
```

Send tokens as `Authorization: Bearer TOKEN`. Use separate ingest credentials for the proxy, Pi, and OpenCode in the same project. Read credentials can query and export data for their project. Project changes and Keep require local administrator access or an OIDC administrator session. Exported user IDs are attribution.

For browser access through a TLS reverse proxy, set `TRACER_AUTH_MODE=authenticated` and configure `TRACER_OIDC_ISSUER`, `TRACER_OIDC_CLIENT_ID`, `TRACER_OIDC_CLIENT_SECRET`, and `TRACER_OIDC_CALLBACK`. Register the public HTTPS callback, for example `https://tracer.example.com/auth/callback`. Configure the provider to include group names in the ID token. The tracer requests the `openid email profile groups` scopes. Members of the configured administrator group can access every tracer project. The callback origin must match browser writes. Login sessions are kept in memory and expire on restart. The listener serves HTTP behind the TLS proxy.

Signed-out visitors see the login screen before the explorer opens. To migrate an existing `TRACER_OIDC_OWNER_SUBJECT` deployment, set `TRACER_OIDC_ADMIN_GROUP` to the group that should have access, then remove `TRACER_OIDC_OWNER_SUBJECT`. A deployment that supplies only the old setting retains its single-user access.

### Container

```sh
docker build -t simple-agent-tracer .
docker volume create tracer-data
docker run --rm --name tracer \
  -p 127.0.0.1:4318:4318 \
  -v tracer-data:/data \
  -e TRACER_BIND=0.0.0.0:4318 \
  -e TRACER_CREDENTIALS \
  simple-agent-tracer
```

Export `TRACER_CREDENTIALS` before running the command. This example uses authenticated ingest and read tokens. Pass the OIDC settings to enable browser sign-in. The image runs as UID and GID `65532` and includes the CA bundle. The default container SQLite path is `/data/tracer.db`. If you bind mount a host directory, ensure that UID `65532` can write to the directory and any existing SQLite database and sidecar files. PostgreSQL only needs the DSN, so omit the data mount. For unauthenticated local UI in a Linux container, use host networking with the default loopback bind and omit the port publication.

## Ingest, capture, and retention

To send spans to a project, open **Projects → your project → Ingestion keys**. Give the exporter a source name (for example, `simple-llm-proxy`) and select **Create key**. Copy the `Bearer …` value when it appears and use it as the exporter's `Authorization` header. For simple-llm-proxy, set `TRACER_OTLP_AUTH_HEADER` to that full value. The key selects the project and source.

The tracer stores a hash and shows only a prefix after creation. To rotate a key, create a replacement with the same source name, update the exporter, then revoke the old key. Revoking a key leaves existing spans intact. Keys supplied through `TRACER_CREDENTIALS` are managed through deployment configuration.

The receiver accepts OTLP HTTP protobuf at `POST /v1/traces`, including gzip. Configure the full URL for `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and a base URL for exporters that append `/v1/traces`. The receiver handles up to four concurrent exports, 8 MiB of request data, 16 MiB after decoding, and 2,000 spans per batch. It caps normalized batch data at approximately 48 MiB to bound repeated resource metadata. Individual attribute values are capped at 64 KiB, with a shared 96 KiB metadata budget and a 64 KiB content budget per span. Oversized or malformed requests get a permanent client error. Invalid spans in a valid envelope are reported in OTLP partial success. A busy receiver or temporary database failure includes `Retry-After`.

Recognized message and tool payloads are stored as data. Disable capture on a project to strip recognized content from future spans, including Langfuse's mirrored copies. OTLP byte attributes and recognized inline media are omitted. Span detail shows captured snapshots, attributes, events, links, and partial or truncated flags. Conversation transcripts collapse overlapping snapshots when there is a reliable match. The span view retains each captured call's context. Reported costs are kept as decimals and grouped by currency. Totals include coverage counts when only some calls reported a value.

Every minute the sweeper deletes up to 200 eligible traces. Eligibility follows the project retention period, the latest span end time or receive-time fallback, and a five-minute grace period after the last new span. Replays do not extend grace. Keep a trace or session from its detail page. The bulk Keep API validates all IDs before updating them. A kept session protects its related traces, including future traces. Clearing session Keep preserves individually kept traces.

## Proxy and agent integration

Use the [proxy setup in the design](DESIGN.md#proxy-integration-setup) to configure the OTLP HTTP exporter, a dedicated application key, and the application's capture allowlist. Set `TRACER_OTLP_AUTH_HEADER` to the full bearer header value. Set `X-Session-ID` in the client to keep proxy-only traffic grouped. The tracer correlates agent and proxy exports when a trusted incoming `traceparent` preserves ancestry and the proxy's application-scoped session digest matches the native session ID. Separate application keys can use the same client session ID without sharing their proxy sessions across applications.

Install the optional integrations from [the agent setup instructions](integrations/README.md). Both use the proxy for model snapshots and billing. Enable agent tool payload capture independently.

## Bulk model-call queries

Query model responses, costs, and timings across a project with
`POST /api/v1/projects/{project}/query`. Send structured filters or a read-only
SQL `SELECT FROM model_calls`. Select `output_messages` to include captured
model responses. Use `POST /api/v1/projects/{project}/query/export` for NDJSON.

For agents, configure Streamable HTTP at
`/api/v1/projects/{project}/mcp` with a project read bearer token and call
`query_model_calls`. See the [query design and examples](QUERY-DESIGN.md).

## Verification

```sh
go test -race ./...
TRACER_TEST_POSTGRES='postgres://USER:PASSWORD@127.0.0.1:5432/DB?sslmode=disable' go test -race ./...
```

The parity test creates and drops an isolated PostgreSQL schema in the test database. It covers migrations, child-first ingestion, replay, parallel writes, typed child-span filters, capture controls, project credentials, pagination, export, and retention. A TLS fixture exercises OIDC group-based sign-in, PKCE, state, nonce, and origin enforcement.

See [the benchmark](scripts/benchmark/README.md) for the 100,000-span fixture, commands, and results.
