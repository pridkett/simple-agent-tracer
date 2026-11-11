<script setup>
import { computed } from 'vue'

const props = defineProps({ value: { required: true } })
const tokens = computed(() => {
  const json = JSON.stringify(props.value, null, 2) ?? ''
  const pattern = /"(?:[^"\\]|\\.)*"(\s*:)?|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|\b(?:true|false|null)\b/g
  const result = []
  let start = 0
  for (const match of json.matchAll(pattern)) {
    const [text, colon] = match
    if (match.index > start) result.push({ text: json.slice(start, match.index) })
    if (colon) {
      result.push({ text: text.slice(0, -colon.length), kind: 'key' }, { text: colon })
    } else {
      const kind = text[0] === '"' ? 'string' : /^[\d-]/.test(text) ? 'number' : 'literal'
      result.push({ text, kind })
    }
    start = match.index + text.length
  }
  if (start < json.length) result.push({ text: json.slice(start) })
  return result
})
const colors = {
  key: 'text-indigo-700 dark:text-indigo-300',
  string: 'text-emerald-800 dark:text-emerald-300',
  number: 'text-amber-800 dark:text-amber-300',
  literal: 'text-violet-700 dark:text-violet-300',
}
</script>

<template>
  <pre class="payload"><code><span v-for="(token, i) in tokens" :key="i" :class="colors[token.kind]">{{ token.text }}</span></code></pre>
</template>
