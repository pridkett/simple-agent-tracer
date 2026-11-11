<script setup>
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { dateLabel, projectPath, request, statusLabel, totals } from './api'

const route = useRoute()
const router = useRouter()
const kind = computed(() => route.params.kind)
const path = computed(() => projectPath(route.params.project))
const fields = { source: 'Source', service: 'Service', session: 'Session ID', operation: 'Operation', model: 'Model', status: 'Status', tool: 'Tool', trace: 'Trace ID', from: 'From (UTC)', to: 'Before (UTC)', metadataKey: 'Metadata key', metadataValue: 'Metadata value (JSON)' }
const form = ref({})
const page = ref({ items: [] })
const keys = ref([])
const values = ref([])
const loading = ref(false)
const error = ref('')

watch(() => route.fullPath, async (_, __, cleanup) => {
  const abort = new AbortController()
  cleanup(() => abort.abort())
  form.value = Object.fromEntries(Object.keys(fields).map(key => [key, route.query[key] || '']))
  loading.value = true
  error.value = ''
  page.value = { items: [] }
  try {
    const params = new URLSearchParams(route.query)
    page.value = await request(`${path.value}/${kind.value}?${params}`, { signal: abort.signal })
  } catch (e) {
    if (!abort.signal.aborted) error.value = e.message
  } finally {
    if (!abort.signal.aborted) loading.value = false
  }
}, { immediate: true })

watch(path, async (_, __, cleanup) => {
  const abort = new AbortController()
  cleanup(() => abort.abort())
  keys.value = []
  try { keys.value = await request(path.value + '/metadata/keys', { signal: abort.signal }) } catch { /* Manual entry remains available. */ }
}, { immediate: true })

watch(() => [path.value, form.value.metadataKey], async ([project, key], _, cleanup) => {
  const abort = new AbortController()
  cleanup(() => abort.abort())
  values.value = []
  if (!keys.value.includes(key)) return
  try {
    const params = new URLSearchParams({ key })
    values.value = await request(project + '/metadata/values?' + params, { signal: abort.signal })
  } catch { /* Manual entry remains available. */ }
})

function filter() {
  router.push({ query: Object.fromEntries(Object.entries(form.value).filter(([, value]) => value)) })
}
</script>

<template>
  <h1 class="mb-5 text-2xl font-semibold">{{ kind === 'sessions' ? 'Sessions' : 'Traces' }}</h1>
  <form class="card mb-5 p-4" @submit.prevent="filter">
    <div class="grid gap-3 sm:grid-cols-2 md:grid-cols-4 xl:grid-cols-6">
      <label v-for="(label, field) in fields" :key="field" class="field">
        {{ label }}
        <select v-if="field === 'status'" v-model="form[field]" class="input mt-1">
          <option value="">Any status</option>
          <option value="STATUS_CODE_ERROR">Error</option>
          <option value="STATUS_CODE_OK">OK</option>
          <option value="STATUS_CODE_UNSET">Unset</option>
        </select>
        <input v-else v-model="form[field]" class="input mt-1"
          :list="field === 'metadataKey' || field === 'metadataValue' ? field : undefined"
          :placeholder="field === 'from' || field === 'to' ? '2026-01-01T00:00:00Z' : undefined">
      </label>
      <datalist id="metadataKey"><option v-for="key in keys" :key="key" :value="key" /></datalist>
      <datalist id="metadataValue"><option v-for="item in values" :key="item.type + item.json" :value="item.json" /></datalist>
    </div>
    <div class="mt-4 flex gap-2">
      <button class="btn-primary">Apply filters</button>
      <RouterLink :to="{ query: {} }" class="btn-secondary">Clear</RouterLink>
    </div>
  </form>

  <p v-if="error" role="alert" class="mb-4 text-sm text-red-700 dark:text-red-300">{{ error }}</p>
  <p v-if="loading" role="status" class="text-sm">Loading…</p>
  <p v-else-if="!error && !page.items.length" class="card p-5 text-sm">No matching {{ kind }}.</p>
  <div v-else-if="page.items.length" class="card overflow-x-auto">
    <table class="w-full text-sm">
      <thead class="table-head">
        <tr>
          <th scope="col" class="cell">{{ kind === 'sessions' ? 'Session' : 'Trace' }}</th>
          <th scope="col" class="cell">Last activity</th>
          <th scope="col" class="cell">Application / source</th>
          <th scope="col" class="cell">Status</th>
          <th scope="col" class="cell">Model calls</th>
          <th scope="col" class="cell">Tokens</th>
          <th scope="col" class="cell">Reported cost</th>
          <th scope="col" class="cell">Keep</th>
        </tr>
      </thead>
      <tbody class="divide-y divide-gray-100 dark:divide-gray-700">
        <tr v-for="item in page.items" :key="item.id">
          <td class="cell"><RouterLink :to="route.path + '/' + item.id" class="link break-all font-mono text-xs">{{ item.externalId || item.id }}</RouterLink></td>
          <td class="cell whitespace-nowrap">{{ dateLabel(item.lastActivity) }}</td>
          <td class="cell">{{ item.application || item.source || '—' }}</td>
          <td class="cell" :class="{ 'text-red-700 dark:text-red-300': item.status === 'STATUS_CODE_ERROR' }">{{ statusLabel(item.status) }}</td>
          <td class="cell">{{ item.modelCalls }}</td>
          <td class="cell">{{ totals(item.tokens, item.modelCalls) }}</td>
          <td class="cell">{{ totals(item.reportedCost, item.modelCalls) }}</td>
          <td class="cell">{{ item.kept ? 'Kept' : '—' }}</td>
        </tr>
      </tbody>
    </table>
  </div>
  <nav aria-label="Pagination" class="mt-4 flex gap-2">
    <RouterLink v-if="route.query.cursor" :to="{ query: { ...route.query, cursor: undefined } }" class="btn-secondary">First page</RouterLink>
    <RouterLink v-if="page.nextCursor" :to="{ query: { ...route.query, cursor: page.nextCursor } }" class="btn-secondary">Next page</RouterLink>
  </nav>
</template>
