import { ROOT_CONTEXT, SpanKind, SpanStatusCode, trace } from '@opentelemetry/api'
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto'
import { resourceFromAttributes } from '@opentelemetry/resources'
import { TracerProvider, BatchSpanProcessor } from '@opentelemetry/sdk-trace'

export function telemetry(service) {
  try {
    if (!process.env.TRACER_PROXY_ORIGIN || !process.env.TRACER_OTLP_TRACES_URL) return
    const proxy = new URL(process.env.TRACER_PROXY_ORIGIN)
    if (!['https:', 'http:'].includes(proxy.protocol) || proxy.username || proxy.password ||
        proxy.pathname !== '/' || proxy.search || proxy.hash) return
    const receiver = new URL(process.env.TRACER_OTLP_TRACES_URL)
    if (!['https:', 'http:'].includes(receiver.protocol) || receiver.username || receiver.password) return
    const token = process.env.TRACER_INGEST_TOKEN
    const exporter = new OTLPTraceExporter({
      url: receiver.href,
      headers: token ? { Authorization: 'Bearer ' + token } : {},
      timeoutMillis: 1500,
    })
    const provider = new TracerProvider({
      resource: resourceFromAttributes({ 'service.name': service }),
      spanProcessors: [new BatchSpanProcessor({
        exporter,
        maxQueueSize: 256,
        maxExportBatchSize: 64,
        scheduledDelayMillis: 250,
        exportTimeoutMillis: 1800,
      })],
      forceFlushTimeoutMillis: 2000,
    })
    const tracer = provider.getTracer('simple-agent-tracer')
    const capture = process.env.TRACER_AGENT_CAPTURE === 'true'

    function start(name, attributes, parent, kind, startTime) {
      return tracer.startSpan(name, { attributes, kind, startTime }, parent ? trace.setSpan(ROOT_CONTEXT, parent) : ROOT_CONTEXT)
    }

    function toolContent(span, key, content) {
      if (!capture) return
      try {
        const data = JSON.stringify(content)
        if (data && Buffer.byteLength(data) <= 16384) span.setAttribute(key, data)
        else if (data) span.setAttribute('tracer.tool.content_truncated', true)
      } catch {
        // Some tools return values that have no JSON representation.
      }
    }

    return {
      matches(baseURL) {
        try {
          const url = new URL(baseURL)
          return url.origin === proxy.origin && !url.username && !url.password
        } catch {
          return false
        }
      },
      turn(session, id, startTime) {
        return start('invoke_agent', { 'gen_ai.operation.name': 'invoke_agent', 'gen_ai.conversation.id': session, 'gen_ai.agent.call.id': id }, undefined, undefined, startTime)
      },
      outbound(headers, session, model, parent, operation = 'chat') {
        if (!/^[\x21-\x7e]{1,256}$/.test(session)) return
        const span = start('proxy ' + operation, {
          'gen_ai.operation.name': operation,
          'gen_ai.conversation.id': session,
          'gen_ai.request.model': model,
        }, parent, SpanKind.CLIENT)
        const { traceId, spanId, traceFlags } = span.spanContext()
        for (const key of Object.keys(headers)) {
          if (['traceparent', 'x-session-id'].includes(key.toLowerCase())) delete headers[key]
        }
        headers.traceparent = `00-${traceId}-${spanId}-${traceFlags.toString(16).padStart(2, '0')}`
        headers['x-session-id'] = session
        return span
      },
      tool(session, id, name, args, parent) {
        const span = start('execute_tool ' + name, {
          'gen_ai.operation.name': 'execute_tool',
          'gen_ai.conversation.id': session,
          'gen_ai.tool.name': name,
          'gen_ai.tool.call.id': id,
        }, parent)
        toolContent(span, 'gen_ai.tool.call.arguments', args)
        return span
      },
      toolResult(span, result, failed) {
        if (!span) return
        toolContent(span, 'gen_ai.tool.call.result', result)
        if (failed) span.setStatus({ code: SpanStatusCode.ERROR })
        span.end()
      },
      error(span) {
        span?.setStatus({ code: SpanStatusCode.ERROR })
      },
      async flush() {
        try { await provider.forceFlush() } catch { /* Export failures are independent of the run. */ }
      },
    }
  } catch {
    return undefined
  }
}
