<script setup>
import { computed } from 'vue'

const props = defineProps({ data: { required: true } })
const text = computed(() => {
  const data = props.data
  if (typeof data === 'string') return data
  if (typeof data?.content === 'string') return data.content
  if (!Array.isArray(data?.parts)) return ''
  return data.parts.filter(part => part?.type === 'text' && typeof part.content === 'string')
    .map(part => part.content).join('\n')
})
const structured = computed(() => {
  if (Array.isArray(props.data?.parts)) {
    return props.data.parts.filter(part => part?.type !== 'text')
  }
  return Array.isArray(props.data?.tool_calls) ? props.data.tool_calls : []
})
</script>

<template>
  <div class="space-y-2">
    <p v-if="text" class="whitespace-pre-wrap break-words rounded-md bg-gray-50 p-3 text-sm dark:bg-gray-900">{{ text }}</p>
    <details v-if="structured.length">
      <summary class="cursor-pointer text-sm">{{ structured.some(part => part?.type === 'tool_call_response') ? 'Tool result' : 'Structured content' }}</summary>
      <pre class="payload mt-2">{{ JSON.stringify(structured, null, 2) }}</pre>
    </details>
    <pre v-if="!text && !structured.length" class="payload">{{ JSON.stringify(data, null, 2) }}</pre>
  </div>
</template>
