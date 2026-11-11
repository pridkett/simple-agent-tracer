import { telemetry } from './telemetry.mjs'

export const AgentTracer = async () => {
  const tracer = telemetry('opencode')
  const providerId = process.env.TRACER_OPENCODE_PROVIDER
  if (!tracer || !providerId) return {}

  const runs = new Map()
  const background = new Map()
  function endRequests(requests, failed = false) {
    for (const request of requests) {
      if (failed) tracer.error(request)
      request.end()
    }
    requests.clear()
  }
  function finish(session, failed = false) {
    const run = runs.get(session)
    if (run) {
      if (failed) tracer.error(run.span)
      endRequests(run.requests, failed)
      for (const tool of run.tools.values()) tool.end()
      run.span?.end()
      runs.delete(session)
    }
    const requests = background.get(session)
    if (requests) endRequests(requests, failed)
    background.delete(session)
  }

  return {
    'chat.message': async (input, output) => {
      const id = output.message?.id || input.messageID
      if (!id) return
      finish(input.sessionID)
      runs.set(input.sessionID, { id, started: Date.now(), tools: new Map(), requests: new Set() })
    },
    'chat.headers': async (input, output) => {
      const url = input.provider.options?.baseURL ?? input.model.api?.url
      if (input.model.providerID !== providerId || !tracer.matches(url)) return
      const session = input.sessionID
      const run = runs.get(session)
      const auxiliary = ['title', 'summary', 'compaction'].includes(input.agent) ? input.agent : undefined
      const matched = run && run.id === input.message?.id && !auxiliary
      if (matched && !run.span) run.span = tracer.turn(session, run.id, run.started)
      const span = tracer.outbound(output.headers, session, input.model.id, matched ? run.span : undefined, auxiliary)
      if (!span) return
      if (matched) {
        run.requests.add(span)
      } else {
        if (!background.has(session)) background.set(session, new Set())
        background.get(session).add(span)
      }
    },
    'tool.execute.before': async (input, output) => {
      const run = runs.get(input.sessionID)
      if (!run?.span) return
      endRequests(run.requests)
      run.tools.set(input.callID, tracer.tool(input.sessionID, input.callID, input.tool, output.args, run.span))
    },
    'tool.execute.after': async (input, output) => {
      const run = runs.get(input.sessionID)
      tracer.toolResult(run?.tools.get(input.callID), output.output, false)
      run?.tools.delete(input.callID)
    },
    event: async ({ event }) => {
      const properties = event.properties
      const part = properties?.part
      const session = properties?.sessionID ?? part?.sessionID
      if (!session) return
      if (event.type === 'message.part.updated' && part?.type === 'tool' && part.state?.status === 'error') {
        const run = runs.get(session)
        tracer.toolResult(run?.tools.get(part.callID), part.state.error, true)
        run?.tools.delete(part.callID)
      }
      if (event.type === 'session.idle' || (event.type === 'session.status' && properties.status?.type === 'idle') ||
          event.type === 'session.error') {
        finish(session, event.type === 'session.error')
        await tracer.flush()
      }
    },
  }
}
