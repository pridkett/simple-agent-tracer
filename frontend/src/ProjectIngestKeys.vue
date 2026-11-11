<script setup>
import { computed, ref } from 'vue'
import { projectPath, request } from './api'

const props = defineProps({ project: { type: String, required: true } })
const path = projectPath(props.project) + '/ingest-keys'
const endpoint = window.location.origin + '/v1/traces'
const keys = ref(null)
const source = ref('')
const issued = ref(null)
const copied = ref(false)
const error = ref('')
const busy = ref(false)
const authValue = computed(() => issued.value ? 'Bearer ' + issued.value.token : '')

async function run(action) {
  busy.value = true
  error.value = ''
  try { await action() } catch (e) { error.value = e.message } finally { busy.value = false }
}

const load = () => run(async () => { keys.value = await request(path) })

function create() {
  return run(async () => {
    const { token, ...key } = await request(path, { method: 'POST', body: { source: source.value.trim() } })
    keys.value.unshift(key)
    issued.value = { id: key.id, token }
    copied.value = false
    source.value = ''
  })
}

async function copy() {
  try {
    await navigator.clipboard.writeText(authValue.value)
    copied.value = true
  } catch {
    error.value = 'Clipboard unavailable. Select and copy the header value directly.'
  }
}

function revoke(key) {
  if (!window.confirm(`Revoke this key for ${key.source}? Exporters using it will no longer be able to send spans.`)) return
  return run(async () => {
    await request(path + '/' + encodeURIComponent(key.id), { method: 'DELETE' })
    keys.value = keys.value.filter(item => item.id !== key.id)
    if (issued.value?.id === key.id) issued.value = null
  })
}

load()
</script>

<template>
  <div class="mt-3 space-y-3 text-sm">
    <p class="text-gray-600 dark:text-gray-300">Send spans to this project using OTLP/HTTP with Protobuf.</p>
    <p class="break-all"><span class="font-medium">Endpoint:</span> <code>{{ endpoint }}</code></p>
    <p v-if="error" role="alert" class="text-red-700 dark:text-red-300">{{ error }}</p>
    <p v-if="keys === null && busy" role="status">Loading keys…</p>
    <button v-else-if="keys === null" type="button" class="btn-secondary" @click="load">Retry</button>
    <template v-else>
      <div v-if="issued" class="space-y-2 rounded-md border bg-gray-50 p-3 dark:bg-gray-900">
        <p role="status" class="font-medium">Key created. Copy it now; it is only shown once.</p>
        <label class="field block">Authorization header value
          <input :value="authValue" readonly autocomplete="off" spellcheck="false" class="input mt-1 font-mono" @focus="$event.target.select()">
        </label>
        <div class="flex flex-wrap items-center gap-2">
          <button type="button" class="btn-secondary" @click="copy">{{ copied ? 'Copied' : 'Copy header value' }}</button>
          <button type="button" class="btn-secondary" @click="issued = null">Done</button>
        </div>
      </div>
      <p v-if="!keys.length" class="text-gray-600 dark:text-gray-300">No keys created here yet.</p>
      <ul v-else class="divide-y border-y">
        <li v-for="key in keys" :key="key.id" class="flex items-center justify-between gap-2 py-2">
          <div class="min-w-0">
            <p class="break-words font-medium">{{ key.source }}</p>
            <code class="text-xs">{{ key.prefix }}…</code>
          </div>
          <button type="button" class="link" :aria-label="'Revoke key ' + key.prefix + ' for ' + key.source" :disabled="busy" @click="revoke(key)">Revoke</button>
        </li>
      </ul>
      <form class="space-y-2" @submit.prevent="create">
        <label class="field block">Source name
          <input v-model="source" placeholder="simple-llm-proxy" pattern="[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,127}" maxlength="128" required class="input mt-1">
        </label>
        <p class="text-xs text-gray-600 dark:text-gray-300">Use the same source name when replacing a key for an exporter.</p>
        <button class="btn-secondary" :disabled="busy || !!issued">Create key</button>
      </form>
    </template>
  </div>
</template>
