<script setup>
import { RouterLink } from 'vue-router'

defineProps({
  number: { type: Number, required: true },
  to: String,
})
</script>

<template>
  <component :is="to ? RouterLink : 'span'" v-bind="to ? { to } : {}"
    class="block rounded border-l-2 border-indigo-500 bg-indigo-50 px-3 py-1.5 text-sm font-semibold text-indigo-900 dark:border-indigo-400 dark:bg-indigo-950/60 dark:text-indigo-200"
    :class="{ 'hover:underline focus-visible:underline': to }">
    Turn {{ number }}
  </component>
</template>
