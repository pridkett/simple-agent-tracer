import { randomUUID } from 'node:crypto'
import { telemetry } from './telemetry.mjs'

export default function agentTracer(pi) {
  const tracer = telemetry('pi')
  if (!tracer) return
  let run
  let outbound
  const tools = new Map()

  pi.on('agent_start', (_event, ctx) => {
    if (!run && tracer.matches(ctx.model?.baseUrl)) {
      const session = ctx.sessionManager.getSessionId()
      run = { session, span: tracer.turn(session, randomUUID()) }
    }
  })
  pi.on('before_provider_headers', (event, ctx) => {
    if (!run || !tracer.matches(ctx.model?.baseUrl)) return
    outbound?.end()
    outbound = tracer.outbound(event.headers, run.session, ctx.model.id, run.span)
  })
  pi.on('after_provider_response', event => {
    if (event.status >= 400) tracer.error(outbound)
  })
  pi.on('turn_end', event => {
    if (run) run.failed = ['error', 'aborted'].includes(event.message?.stopReason)
    if (run?.failed) tracer.error(outbound)
    outbound?.end()
    outbound = undefined
  })
  pi.on('tool_execution_start', event => {
    if (!run) return
    const parent = tools.get(event.parentToolCallId) || run.span
    tools.set(event.toolCallId, tracer.tool(run.session, event.toolCallId, event.toolName, event.args, parent))
  })
  pi.on('tool_execution_end', event => {
    tracer.toolResult(tools.get(event.toolCallId), event.result, event.isError)
    tools.delete(event.toolCallId)
  })
  pi.on('agent_settled', async event => {
    if (!run) return
    if (event.aborted || run.failed) tracer.error(run.span)
    outbound?.end()
    outbound = undefined
    for (const tool of tools.values()) tool.end()
    tools.clear()
    run.span.end()
    run = undefined
    await tracer.flush()
  })
}
