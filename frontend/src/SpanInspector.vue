<script setup>
import { computed, ref } from 'vue'
import { elapsed, statusLabel } from './api'
import JsonPayload from './JsonPayload.vue'

const props = defineProps({
  row: { type: Object, required: true },
  outsideTimeline: Boolean,
  linkedTo: Object,
  permalink: { type: String, required: true },
})
defineEmits(['close'])
const span = computed(() => props.row.data)
const openFields = ref(new Set(['input', 'output', 'messages', 'instructions']))
const fields = {
  input: 'Input', output: 'Output', messages: 'Messages', instructions: 'Instructions',
  usage: 'Usage', attributes: 'Attributes', resource: 'Resource', scopeAttributes: 'Scope attributes',
  events: 'Events', links: 'Links', diagnostics: 'Diagnostics', raw: 'Stored span JSON',
}

function toggle(field, event) {
  if (event.target.open) openFields.value.add(field)
  else openFields.value.delete(field)
}
</script>

<template>
  <aside id="span-inspector" class="card span-inspector min-w-0" aria-label="Span details">
    <header class="flex items-start justify-between gap-2 border-b border-gray-200 p-4 dark:border-gray-700">
      <div class="min-w-0">
        <h2 class="field">Span details</h2>
        <p class="mt-1 break-words text-lg font-semibold">{{ span.name || span.id }}</p>
        <p class="mt-1 break-all font-mono text-xs text-gray-600 dark:text-gray-300">{{ span.id }}</p>
        <RouterLink :to="permalink" class="link mt-2 inline-block text-xs">Span permalink</RouterLink>
      </div>
      <button type="button" class="btn-secondary px-2.5 py-1" aria-label="Close span details" @click="$emit('close')">Close</button>
    </header>
    <div class="inspector-body space-y-3 overflow-auto p-4">
      <p class="text-sm">{{ span.operation || span.kind }} · {{ statusLabel(span.status) }} · {{ elapsed(span.start, span.end) }}</p>
      <p v-if="span.statusMessage" class="break-words text-sm">{{ span.statusMessage }}</p>
      <p v-if="span.partial || span.truncated" class="text-sm text-amber-700 dark:text-amber-300">Partial or truncated capture</p>
      <p v-if="outsideTimeline" class="text-sm">This span is outside the current timeline. <RouterLink :to="permalink" class="link">Open in trace</RouterLink></p>
      <p v-if="row.linkedModel" class="text-sm">Reported usage is counted on the linked proxy operation.</p>
      <p v-if="linkedTo" class="text-sm">Missing GenAI token counts can come from the <RouterLink :to="linkedTo" class="link">linked client span</RouterLink>.</p>
      <p v-if="row.conflictingExports" class="text-sm">Conflicting re-exports: {{ row.conflictingExports }}</p>
      <template v-for="(label, field) in fields" :key="field">
        <details v-if="field === 'raw' || span[field] != null" class="border-t border-gray-200 pt-3 dark:border-gray-700"
          :open="openFields.has(field)" @toggle="toggle(field, $event)">
          <summary class="cursor-pointer text-sm font-medium">{{ label }}</summary>
          <JsonPayload v-if="openFields.has(field)" :value="field === 'raw' ? span : span[field]" class="mt-2" />
        </details>
      </template>
    </div>
  </aside>
</template>
