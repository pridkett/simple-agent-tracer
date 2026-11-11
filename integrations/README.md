# Pi and OpenCode telemetry

Run `npm ci --prefix integrations` in the tracer checkout. The adapters use Node's OpenTelemetry SDK and export OTLP HTTP protobuf directly to the tracer. Use a different ingestion credential from the proxy's credential and scope both to the same project.

Set the environment variables in the agent process.

```sh
export TRACER_PROXY_ORIGIN=http://127.0.0.1:8080
export TRACER_OTLP_TRACES_URL=http://127.0.0.1:4318/v1/traces
export TRACER_INGEST_TOKEN='your-agent-ingestion-token'
export PROXY_APP_KEY='your-proxy-application-key'
```

Use the exact scheme, host and port of the proxy origin, without the `/v1` path. Use a trusted proxy endpoint that serves model requests directly. Enable `TRACER_AGENT_CAPTURE=true` to export small tool arguments and results. Proxy capture remains separately controlled by the proxy application allowlist.

## Pi

Tested with `@earendil-works/pi-coding-agent@1.5.2`, Node.js 24, and Pi's `openai-completions` provider. Add the provider to Pi's `models.json` and replace the model ID with your proxy alias.

```json
{
  "providers": {
    "tracer": {
      "baseUrl": "http://127.0.0.1:8080/v1",
      "api": "openai-completions",
      "apiKey": "$PROXY_APP_KEY",
      "models": [{"id": "my-proxy-model"}]
    }
  }
}
```

Run `pi --model tracer/my-proxy-model -e /absolute/path/to/simple_agent_tracer/integrations/pi.mjs`. The extension uses the native session ID and creates a turn ID once per run. It closes the turn at `agent_settled`, after the model and tool loop. Pi's HTTP header hook provides the outbound request context. [Pi extension API](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md)

## OpenCode

Tested with `opencode-ai@1.19.5`, the V1 plugin interface, and `@ai-sdk/openai-compatible`. Add the provider to `opencode.json`.

```json
{
  "provider": {
    "tracer": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://127.0.0.1:8080/v1",
        "apiKey": "{env:PROXY_APP_KEY}"
      },
      "models": {
        "my-proxy-model": {"name": "Proxy model", "tool_call": true}
      }
    }
  }
}
```

Set `TRACER_OPENCODE_PROVIDER=tracer`. Add the following export to `.opencode/plugins/tracer.js` in the OpenCode project and use the real checkout path.

```js
export { AgentTracer } from '/absolute/path/to/simple_agent_tracer/integrations/opencode.mjs'
```

Select `tracer/my-proxy-model`. The plugin uses OpenCode's native session and user-message IDs. The request header hook attaches calls for the current user message to that turn. Distinct title, summary and compaction operations stay visible in trace activity. OpenCode's title call contributes its reported usage and cost to the session. The V2 server plugin interface is outside the tested version set. [OpenCode V1 plugin interface](https://github.com/anomalyco/opencode/blob/v1.19.5/packages/plugin/src/index.ts)

## Smoke test results

The proxy test used `simple_llm_proxy` revision `c417dea` with a local OpenAI-compatible fixture upstream and a dedicated application key. The proxy-only run produced two model calls, an ordered four-message transcript, two proxy request IDs, and the expected usage and USD total. The Pi run produced two turns, one read tool span, and three streamed proxy calls. The OpenCode run produced the same conversational activity and a separate title call. In both agent runs, the tracer linked the agent request and proxy model spans and counted each proxy model operation once.

With the adapter disabled and an explicit session header in the provider configuration, both harnesses produced proxy-only sessions with the same model-call totals and no explicit turns. Separate proxy requests verified trusted ancestry, ignored untrusted context, missing context, and application-scoped sessions with an identical client header.
