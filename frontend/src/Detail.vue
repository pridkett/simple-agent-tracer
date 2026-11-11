<script setup>
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import MessageContent from './MessageContent.vue'
import SpanInspector from './SpanInspector.vue'
import SpanTimeline from './SpanTimeline.vue'
import { dateLabel, projectPath, request, statusLabel, totals } from './api'

const props = defineProps({ kind: String })
const route = useRoute()
const router = useRouter()
const kind = computed(() => props.kind || route.params.kind)
const project = computed(() => projectPath(route.params.project))
const browserBase = computed(() => '/projects/' + encodeURIComponent(route.params.project))
const entityPath = computed(() => project.value + '/' + kind.value + '/' + route.params.id)
const isTrace = computed(() => kind.value === 'traces')
const isTurn = computed(() => Boolean(route.params.turn))
const timelinePath = computed(() => entityPath.value + (isTrace.value ? '/spans'
  : isTurn.value ? '/turns/' + route.params.turn : '/timeline'))
const currentPath = computed(() => isTrace.value ? browserBase.value + '/traces/' + route.params.id : route.path)
const cursor = computed(() => route.query.cursor)
const turnCursor = computed(() => route.query.turnCursor)

const detail = ref(null)
const originalPage = ref({ items: [] })
const focusedPage = ref(null)
const page = computed(() => focusedPage.value || originalPage.value)
const activityRows = computed(() => [...page.value.items, ...(page.value.parents || [])])
const turns = ref({ items: [] })
const error = ref('')
const loading = ref(false)
const busy = ref(false)
const outside = ref(null)
const inspectorError = ref('')
const inspectorLoading = ref(false)

const spanKey = (trace, span) => trace + '.' + span
const rowKey = row => spanKey(row.trace, row.id)
const selectedKey = computed(() => {
  const value = route.params.span ? spanKey(route.params.id, route.params.span) : route.query.span
  return typeof value === 'string' && /^[0-9a-f]{32}\.[0-9a-f]{16}$/.test(value) ? value : ''
})
const selectedInTimeline = computed(() => activityRows.value.find(row => rowKey(row) === selectedKey.value))
const selected = computed(() => selectedInTimeline.value ||
  (outside.value && rowKey(outside.value) === selectedKey.value ? outside.value : null))
const traceUrl = (trace, span) => ({
  path: browserBase.value + '/traces/' + trace,
  query: { span: spanKey(trace, span) },
})
const spanLocation = (trace, span) => ({
  path: currentPath.value,
  query: { ...route.query, span: spanKey(trace, span) },
})
const pageLocation = next => ({ path: currentPath.value, query: { ...route.query, span: undefined, cursor: next } })

function closeInspector() {
  router.push({ path: currentPath.value, query: { ...route.query, span: undefined } })
}

function firstPage() {
  if (!cursor.value) focusedPage.value = null
}

function reveal(key) {
  document.getElementById('timeline-' + key)?.scrollIntoView({ block: 'nearest', inline: 'nearest' })
}

watch([timelinePath, cursor, turnCursor], async ([timeline, after, afterTurn], _, cleanup) => {
  const abort = new AbortController()
  cleanup(() => abort.abort())
  loading.value = true
  error.value = ''
  detail.value = null
  originalPage.value = { items: [] }
  turns.value = { items: [] }
  focusedPage.value = null
  const path = entityPath.value
  const get = url => request(url, { signal: abort.signal })
  const params = new URLSearchParams({ includeParents: 'true' })
  if (after) params.set('cursor', after)
  try {
    const [summary, result, turnList] = await Promise.all([
      get(path),
      get(timeline + '?' + params),
      !isTrace.value && !isTurn.value ? get(path + '/turns' + (afterTurn ? '?cursor=' + encodeURIComponent(afterTurn) : '')) : null,
    ])
    if (abort.signal.aborted) return
    detail.value = summary
    originalPage.value = result
    if (turnList) turns.value = turnList
  } catch (e) {
    if (!abort.signal.aborted) error.value = e.message
  } finally {
    if (!abort.signal.aborted) loading.value = false
  }
}, { immediate: true })

watch([selectedKey, loading], async ([key, waiting], _, cleanup) => {
  const abort = new AbortController()
  cleanup(() => abort.abort())
  outside.value = null
  inspectorError.value = ''
  inspectorLoading.value = false
  if (waiting || !key || !detail.value || selectedInTimeline.value) return

  inspectorLoading.value = true
  const timeline = timelinePath.value
  const apiBase = project.value
  try {
    try {
      const params = new URLSearchParams({ focus: key, includeParents: 'true' })
      const window = await request(timeline + '?' + params, { signal: abort.signal })
      if (abort.signal.aborted) return
      focusedPage.value = window
    } catch (e) {
      if (abort.signal.aborted) return
      if (e.status !== 404) throw e
      const [trace, span] = key.split('.')
      const row = await request(apiBase + '/traces/' + trace + '/spans/' + span, { signal: abort.signal })
      if (!abort.signal.aborted) outside.value = row
    }
  } catch (e) {
    if (!abort.signal.aborted) inspectorError.value = e.status === 404 ? 'Span not found.' : e.message
  } finally {
    if (!abort.signal.aborted) inspectorLoading.value = false
  }
}, { immediate: true })

watch(selectedInTimeline, row => {
  if (row) reveal(rowKey(row))
}, { flush: 'post' })

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

</script>

<template>
  <nav class="mb-4 flex flex-wrap items-center gap-2 text-sm" aria-label="Breadcrumb">
    <RouterLink :to="browserBase + '/' + kind" class="link">{{ isTrace ? 'Traces' : 'Sessions' }}</RouterLink>
    <RouterLink v-if="isTurn" :to="browserBase + '/sessions/' + route.params.id" class="link">Session</RouterLink>
  </nav>
  <p v-if="loading" role="status" class="text-sm">Loading…</p>
  <p v-if="error" role="alert" class="mb-4 text-sm text-red-700 dark:text-red-300">{{ error }}</p>
  <template v-if="detail">
    <div class="mb-5 flex flex-wrap items-start justify-between gap-4">
      <div class="min-w-0">
        <h1 class="text-2xl font-semibold">{{ isTurn ? 'Turn' : isTrace ? 'Trace' : 'Session' }}</h1>
        <p class="mt-1 break-all font-mono text-xs text-gray-600 dark:text-gray-300">{{ isTurn ? route.params.turn : detail.externalId || detail.id }}</p>
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


    <div class="detail-layout" :class="{ 'trace-layout': isTrace, 'has-inspector': selectedKey || isTrace }">
      <SpanTimeline :rows="activityRows" :trace="isTrace" :selected-key="selectedKey" :span-location="spanLocation">
        <template v-if="turns.items.length" #turns>
          <details class="border-b border-gray-200 px-4 py-2 text-sm dark:border-gray-700">
            <summary class="cursor-pointer font-medium">Turns</summary>
            <ul class="mt-2 max-h-32 space-y-1 overflow-auto">
              <li v-for="turn in turns.items" :key="turn.id">
                <RouterLink :to="browserBase + '/sessions/' + detail.id + '/turns/' + turn.id" class="link">
                  Turn {{ turn.number }} · {{ dateLabel(turn.start) }}
                </RouterLink>
              </li>
            </ul>
            <RouterLink v-if="turns.nextCursor" :to="{ query: { ...route.query, turnCursor: turns.nextCursor } }" class="link mt-2 block text-xs">Next turns</RouterLink>
          </details>
        </template>
        <template v-if="page.nextCursor || cursor || focusedPage" #pagination>
          <nav aria-label="Span pagination" class="flex flex-wrap gap-3 border-t border-gray-200 px-4 py-2 text-xs dark:border-gray-700">
            <RouterLink v-if="cursor || focusedPage" :to="pageLocation(undefined)" class="link" @click="firstPage">First page</RouterLink>
            <RouterLink v-if="page.nextCursor" :to="pageLocation(page.nextCursor)" class="link">Next spans</RouterLink>
          </nav>
        </template>
      </SpanTimeline>

      <SpanInspector v-if="selected" :key="selectedKey" :row="selected" :outside-timeline="!selectedInTimeline"
        :trace-url="traceUrl(selected.trace, selected.id)"
        :linked-to="selected.usageFallbackSpanId ? spanLocation(selected.trace, selected.usageFallbackSpanId) : undefined"
        @close="closeInspector" />
      <aside v-else-if="selectedKey || isTrace" id="span-inspector" class="card span-inspector p-4 text-sm" aria-label="Span details">
        <div class="flex items-start justify-between gap-2">
          <h2 class="font-semibold">Span details</h2>
          <button v-if="selectedKey" type="button" class="link" @click="closeInspector">Close</button>
        </div>
        <p v-if="inspectorError" role="alert" class="mt-2 text-red-700 dark:text-red-300">{{ inspectorError }}</p>
        <p v-else-if="inspectorLoading" role="status" class="mt-2">Locating span…</p>
        <p v-else class="mt-2 text-gray-600 dark:text-gray-300">Select a span to inspect its details.</p>
      </aside>

      <section v-if="!isTrace" class="card conversation-panel min-w-0 space-y-4 p-4">
        <h2 class="font-semibold">Conversation</h2>
        <p v-if="!page.transcript?.length" class="text-sm">No messages captured in this window.</p>
        <article v-for="(message, i) in page.transcript" :key="i" class="border-t pt-3">
          <div class="mb-2 flex items-center justify-between gap-2 text-sm">
            <h3 class="font-medium capitalize">{{ message.role || 'Message' }}</h3>
            <RouterLink :to="spanLocation(message.traceId, message.spanId)" class="link shrink-0 text-xs"
              @click="selectedInTimeline && selectedKey === spanKey(message.traceId, message.spanId) && reveal(selectedKey)">Inspect span</RouterLink>
          </div>
          <MessageContent :data="message.data" />
        </article>
      </section>
    </div>
  </template>
</template>
