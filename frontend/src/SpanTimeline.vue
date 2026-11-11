<script setup>
import { computed } from 'vue'
import { elapsed, statusLabel } from './api'

const props = defineProps({
  rows: { type: Array, required: true },
  trace: Boolean,
  selectedKey: String,
  spanLocation: { type: Function, required: true },
})

const extent = computed(() => {
  let first, last
  for (const { data: span } of props.rows) {
    if (span.start === '0') continue
    const start = BigInt(span.start)
    const end = span.end === '0' ? start : BigInt(span.end)
    first = first === undefined || start < first ? start : first
    last = last === undefined || end > last ? end : last
  }
  return { first, last }
})

const rows = computed(() => {
  const spans = props.rows.map(row => row.data)
  if (!props.trace) return spans.map(span => ({ span, depth: 0 }))
  const ids = new Set(spans.map(span => span.id))
  const children = new Map()
  for (const span of spans) {
    const parent = ids.has(span.parentId) ? span.parentId : ''
    if (!children.has(parent)) children.set(parent, [])
    children.get(parent).push(span)
  }
  const ordered = []
  const seen = new Set()
  function visit(span, depth) {
    if (seen.has(span.id)) return
    seen.add(span.id)
    ordered.push({ span, depth })
    for (const child of children.get(span.id) || []) visit(child, depth + 1)
  }
  for (const span of children.get('') || []) visit(span, 0)
  for (const span of spans) visit(span, 0)
  return ordered
})

function waterfall(span) {
  const { first, last } = extent.value
  if (first === undefined || last <= first || span.start === '0') return { width: '0%' }
  const start = BigInt(span.start)
  const end = span.end === '0' ? start : BigInt(span.end)
  const percent = value => Number(value * 10000n / (last - first)) / 100
  return { left: percent(start - first) + '%', width: Math.max(1, percent(end - start)) + '%' }
}

const keyOf = span => span.traceId + '.' + span.id
</script>

<template>
  <section class="card activity-panel min-w-0" aria-labelledby="activity-title">
    <h2 id="activity-title" class="border-b border-gray-200 px-4 py-3 font-semibold dark:border-gray-700">{{ trace ? 'Span timeline' : 'Activity' }}</h2>
    <slot name="turns" />
    <div class="activity-list overflow-auto">
      <p v-if="!rows.length" class="px-4 py-3 text-sm text-gray-600 dark:text-gray-300">No spans in this window.</p>
      <ul v-else class="divide-y divide-gray-100 dark:divide-gray-700">
        <li v-for="{ span, depth } in rows" :id="'timeline-' + keyOf(span)" :key="keyOf(span)" class="scroll-my-1">
          <RouterLink :to="spanLocation(span.traceId, span.id)"
            class="block border-l-2 px-3 py-2.5 focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-indigo-500"
            :class="keyOf(span) === selectedKey ? 'border-indigo-600 bg-indigo-50 dark:border-indigo-400 dark:bg-indigo-950/60' : 'border-transparent hover:bg-gray-50 dark:hover:bg-gray-900/60'"
            :aria-current="keyOf(span) === selectedKey ? 'true' : undefined"
            :style="{ paddingLeft: 12 + Math.min(depth, 5) * 10 + 'px' }">
            <span class="block truncate text-sm font-medium" :title="span.name || span.id">{{ span.name || span.id }}</span>
            <span class="mt-0.5 flex flex-wrap gap-x-2 text-xs text-gray-600 dark:text-gray-300">
              <span>{{ elapsed(span.start, span.end) }}</span>
              <span :class="{ 'text-red-700 dark:text-red-300': span.status === 'STATUS_CODE_ERROR' }">{{ statusLabel(span.status) }}</span>
              <span v-if="span.partial || span.truncated" class="text-amber-700 dark:text-amber-300">Partial capture</span>
            </span>
            <span v-if="trace" class="relative mt-1.5 block h-1 overflow-hidden rounded bg-gray-100 dark:bg-gray-900" aria-hidden="true">
              <span class="absolute h-full rounded bg-indigo-500" :style="waterfall(span)" />
            </span>
          </RouterLink>
        </li>
      </ul>
    </div>
    <slot name="pagination" />
  </section>
</template>
