<script setup>
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import MessageContent from "./MessageContent.vue"
import { dateLabel, elapsed, projectPath, request, statusLabel, totals } from './api'

const props = defineProps({ kind: String })
const route = useRoute()
const kind = computed(() => props.kind || route.params.kind)
const project = computed(() => projectPath(route.params.project))
const browserBase = computed(() => '/projects/' + encodeURIComponent(route.params.project))
const entityPath = computed(() => project.value + '/' + kind.value + '/' + route.params.id)
const detail = ref(null)
const page = ref({ items: [] })
const turns = ref({ items: [] })
const selected = ref(null)
const error = ref('')
const loading = ref(false)
const busy = ref(false)
const isTrace = computed(() => kind.value === 'traces')
const isTurn = computed(() => Boolean(route.params.turn))
const spanUrl = (trace, span) => browserBase.value + '/traces/' + trace + '/spans/' + span
const items = computed(() => page.value.items.map(row => row.data))
const traceExtent = computed(() => {
  let first, last
  for (const span of items.value) {
    if (span.start === '0') continue
    const start = BigInt(span.start)
    const end = span.end === '0' ? start : BigInt(span.end)
    first = first === undefined || start < first ? start : first
    last = last === undefined || end > last ? end : last
  }
  return { first, last }
})

const treeRows = computed(() => {
  if (!isTrace.value) return items.value.map(span => ({ span, depth: 0 }))
  const ids = new Set(items.value.map(span => span.id))
  const children = new Map()
  for (const span of items.value) {
    const parent = ids.has(span.parentId) ? span.parentId : ''
    if (!children.has(parent)) children.set(parent, [])
    children.get(parent).push(span)
  }
  const rows = []
  const seen = new Set()
  function visit(span, depth) {
    if (seen.has(span.id)) return
    seen.add(span.id)
    rows.push({ span, depth })
    for (const child of children.get(span.id) || []) visit(child, depth + 1)
  }
  for (const span of children.get('') || []) visit(span, 0)
  for (const span of items.value) visit(span, 0)
  return rows
})

function waterfall(span) {
  const { first, last } = traceExtent.value
  if (first === undefined || last <= first || span.start === '0') return { left: '0%', width: '0%' }
  const start = BigInt(span.start)
  const end = span.end === '0' ? start : BigInt(span.end)
  const percent = value => Number(value * 10000n / (last - first)) / 100
  return { left: percent(start - first) + '%', width: Math.max(1, percent(end - start)) + '%' }
}

watch(() => route.fullPath, async (_, __, cleanup) => {
  const abort = new AbortController()
  cleanup(() => abort.abort())
  loading.value = true
  error.value = ''
  detail.value = null
  selected.value = null
  page.value = { items: [] }
  turns.value = { items: [] }
  const path = entityPath.value
  const get = url => request(url, { signal: abort.signal })
  try {
    const suffix = isTrace.value ? '/spans' : isTurn.value ? '/turns/' + route.params.turn : '/timeline'
    const params = new URLSearchParams()
    if (route.query.cursor) params.set('cursor', route.query.cursor)
    const [summary, result, turnList, span] = await Promise.all([
      get(path),
      get(path + suffix + '?' + params),
      !isTrace.value && !isTurn.value ? get(path + '/turns' + (route.query.turnCursor ? '?cursor=' + encodeURIComponent(route.query.turnCursor) : '')) : null,
      route.params.span ? get(path + '/spans/' + route.params.span) : null,
    ])
    detail.value = summary
    page.value = result
    if (turnList) turns.value = turnList
    selected.value = span
  } catch (e) {
    if (!abort.signal.aborted) error.value = e.message
  } finally {
    if (!abort.signal.aborted) loading.value = false
  }
}, { immediate: true })

async function keep() {
  busy.value = true
  error.value = ''
  const path = entityPath.value
  try {
    await request(project.value + '/retention/keep', {
      method: 'POST',
      body: { [isTrace.value ? 'traceIds' : 'sessionIds']: [detail.value.id], keep: !detail.value.kept },
    })
    const result = await request(path)
    if (entityPath.value === path) detail.value = result
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

const pretty = data => JSON.stringify(data, null, 2)
</script>

<template>
  <nav class="mb-4 flex flex-wrap items-center gap-2 text-sm" aria-label="Breadcrumb">
    <RouterLink :to="browserBase + '/' + kind" class="link">{{ isTrace ? 'Traces' : 'Sessions' }}</RouterLink>
    <RouterLink v-if="isTurn || selected" :to="browserBase + '/' + kind + '/' + route.params.id" class="link">
      {{ isTrace ? 'Trace' : 'Session' }}
    </RouterLink>
  </nav>
  <p v-if="loading" role="status" class="text-sm">Loading…</p>
  <p v-if="error" role="alert" class="mb-4 text-sm text-red-700 dark:text-red-300">{{ error }}</p>
  <template v-if="detail">
    <div class="mb-5 flex flex-wrap items-start justify-between gap-4">
      <div class="min-w-0">
        <h1 class="text-2xl font-semibold">{{ selected ? selected.data.name : isTurn ? 'Turn' : isTrace ? 'Trace' : 'Session' }}</h1>
        <p class="mt-1 break-all font-mono text-xs text-gray-600 dark:text-gray-300">{{ selected?.id || (isTurn ? route.params.turn : detail.externalId || detail.id) }}</p>
      </div>
      <div class="flex gap-2">
        <button class="btn-secondary" :disabled="busy" @click="keep">{{ detail.kept ? 'Unkeep' : 'Keep' }}</button>
        <a :href="entityPath + '/export'" class="btn-secondary">Export JSON</a>
      </div>
    </div>
    <div class="card mb-5 grid gap-4 p-4 text-sm md:grid-cols-3">
      <div><span class="field block">Last activity</span>{{ dateLabel(detail.lastActivity) }}</div>
      <div><span class="field block">Status</span>{{ statusLabel(detail.status) }} · {{ detail.modelCalls }} model calls · {{ detail.spans }} spans</div>
      <div><span class="field block">Retention</span>{{ detail.kept ? 'Kept' : detail.protectedBySession ? 'Kept by session' : detail.sweepEligibleAfter ? 'Eligible ' + dateLabel(detail.sweepEligibleAfter) : 'Project policy' }}</div>
      <div><span class="field block">Tokens</span>{{ totals(detail.tokens, detail.modelCalls) }}</div>
      <div><span class="field block">Reported cost</span>{{ totals(detail.reportedCost, detail.modelCalls) }}</div>
      <div v-if="detail.sessionConflict" class="text-amber-700 dark:text-amber-300">Multiple or conflicting session associations</div>
    </div>

    <section v-if="selected" class="card mb-5 space-y-4 p-4">
      <h2 class="font-semibold">Span details</h2>
      <p class="text-sm">{{ selected.data.operation || selected.data.kind }} · {{ statusLabel(selected.data.status) }} · {{ elapsed(selected.data.start, selected.data.end) }}</p>
      <p v-if="selected.data.statusMessage" class="text-sm">{{ selected.data.statusMessage }}</p>
      <p v-if="selected.data.partial || selected.data.truncated" class="text-sm text-amber-700 dark:text-amber-300">Partial or truncated capture</p>
      <p v-if="selected.linkedModel" class="text-sm">Reported usage is counted on the linked proxy operation.</p>
      <p v-if="selected.usageFallbackSpanId" class="text-sm">Missing GenAI token counts can come from the <RouterLink :to="spanUrl(selected.data.traceId, selected.usageFallbackSpanId)" class="link">linked client span</RouterLink>.</p>
      <p v-if="selected.conflictingExports" class="text-sm">Conflicting re-exports: {{ selected.conflictingExports }}</p>
      <details v-for="field in ['input', 'output', 'messages', 'instructions', 'usage', 'attributes', 'resource', 'scopeAttributes', 'events', 'links', 'diagnostics'].filter(field => selected.data[field] != null)" :key="field" :open="['input', 'output', 'messages', 'instructions'].includes(field)">
          <summary class="cursor-pointer text-sm font-medium capitalize">{{ field }}</summary>
          <pre class="payload mt-2">{{ pretty(selected.data[field]) }}</pre>
      </details>
      <details><summary class="cursor-pointer text-sm font-medium">Stored span JSON</summary><pre class="payload mt-2">{{ pretty(selected.data) }}</pre></details>
    </section>

    <div class="grid items-start gap-5" :class="{ 'xl:grid-cols-2': !isTrace }">
      <section v-if="!isTrace" class="card space-y-4 p-4">
        <h2 class="font-semibold">Conversation</h2>
        <p v-if="!page.transcript?.length" class="text-sm">No messages captured in this window.</p>
        <article v-for="(message, i) in page.transcript" :key="i" class="border-t pt-3">
          <div class="mb-2 flex items-center justify-between gap-2 text-sm">
            <h3 class="font-medium capitalize">{{ message.role || 'Message' }}</h3>
            <RouterLink :to="spanUrl(message.traceId, message.spanId)" class="link text-xs">Inspect span</RouterLink>
          </div>
          <MessageContent :data="message.data" />
        </article>
      </section>
      <div class="space-y-5">
        <section v-if="turns.items.length" class="card space-y-3 p-4">
          <h2 class="font-semibold">Turns</h2>
          <ul class="space-y-2 text-sm">
            <li v-for="turn in turns.items" :key="turn.id"><RouterLink :to="browserBase + '/sessions/' + detail.id + '/turns/' + turn.id" class="link">Turn {{ turn.number }} · {{ dateLabel(turn.start) }}</RouterLink></li>
          </ul>
          <RouterLink v-if="turns.nextCursor" :to="{ query: { ...route.query, turnCursor: turns.nextCursor } }" class="btn-secondary">Next turns</RouterLink>
        </section>
        <section class="card overflow-x-auto">
          <h2 class="px-4 py-3 font-semibold">{{ isTrace ? 'Span timeline' : 'Activity' }}</h2>
          <table class="w-full text-sm">
            <thead class="table-head"><tr><th scope="col" class="cell">Span</th><th v-if="isTrace" scope="col" class="cell">Timeline</th><th scope="col" class="cell">Duration</th><th scope="col" class="cell">Status</th></tr></thead>
            <tbody class="divide-y divide-gray-100 dark:divide-gray-700">
              <tr v-for="{ span, depth } in treeRows" :key="span.traceId + span.id">
                <td class="cell" :style="{ paddingLeft: 16 + Math.min(depth, 8) * 14 + 'px' }">
                  <RouterLink :to="spanUrl(span.traceId, span.id)" class="link break-all">{{ span.name || span.id }}</RouterLink>
                  <p v-if="span.operation || span.tool || span.partial || span.truncated" class="mt-1 text-xs text-gray-600 dark:text-gray-300">
                    {{ [span.operation, span.tool, span.partial || span.truncated ? 'Partial capture' : ''].filter(Boolean).join(' · ') }}
                  </p>
                </td>
                <td v-if="isTrace" class="cell w-1/3">
                  <div class="relative h-3 rounded bg-gray-100 dark:bg-gray-900">
                    <div class="absolute h-full rounded bg-indigo-500" :style="waterfall(span)" />
                  </div>
                </td>
                <td class="cell whitespace-nowrap">{{ elapsed(span.start, span.end) }}</td>
                <td class="cell" :class="{ 'text-red-700 dark:text-red-300': span.status === 'STATUS_CODE_ERROR' }">{{ statusLabel(span.status) }}</td>
              </tr>
            </tbody>
          </table>
        </section>
        <nav v-if="page.nextCursor || route.query.cursor" aria-label="Span pagination" class="flex gap-2">
          <RouterLink v-if="route.query.cursor" :to="{ query: { ...route.query, cursor: undefined } }" class="btn-secondary">First page</RouterLink>
          <RouterLink v-if="page.nextCursor" :to="{ query: { ...route.query, cursor: page.nextCursor } }" class="btn-secondary">Next spans</RouterLink>
        </nav>
      </div>
    </div>
  </template>
</template>
