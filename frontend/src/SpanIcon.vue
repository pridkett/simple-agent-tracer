<script setup>
import { computed } from 'vue'

const props = defineProps({ span: { type: Object, required: true } })

const icons = {
  http: { label: 'HTTP request', color: 'text-sky-700 dark:text-sky-300' },
  model: { label: 'Model call', color: 'text-indigo-700 dark:text-indigo-300' },
  tool: { label: 'Tool call', color: 'text-amber-800 dark:text-amber-300' },
  other: { label: 'Span', color: 'text-gray-500 dark:text-gray-400' },
}

const type = computed(() => {
  const { attributes = {}, modelCall, name, operation, tool } = props.span
  if (modelCall) return 'model'
  if (tool || operation === 'execute_tool') return 'tool'
  if (attributes['http.request.method'] || attributes['http.method'] || attributes['http.route'] ||
      /^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|CONNECT|TRACE)\s/.test(name)) return 'http'
  return 'other'
})
</script>

<template>
  <span class="inline-flex shrink-0" :class="icons[type].color" :title="icons[type].label">
    <span class="sr-only">{{ icons[type].label }} </span>
    <svg aria-hidden="true" class="size-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7"
      stroke-linecap="round" stroke-linejoin="round">
      <template v-if="type === 'http'">
        <circle cx="12" cy="12" r="9" />
        <path d="M3 12h18M12 3a15 15 0 0 1 0 18 15 15 0 0 1 0-18Z" />
      </template>
      <path v-else-if="type === 'model'" d="M4 5h16v11H10l-5 4v-4H4zM8 9h8m-8 3h5" />
      <path v-else-if="type === 'tool'" d="M14 6a5 5 0 0 0-6 6L3 17a2 2 0 0 0 3 3l5-5a5 5 0 0 0 6-6l-3 3-2-2 3-3z" />
      <rect v-else x="5" y="5" width="14" height="14" rx="3" />
    </svg>
  </span>
</template>
