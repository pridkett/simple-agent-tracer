<script setup>
import { computed } from 'vue'
import { distribution, readDecision } from './decision'
import JsonPayload from './JsonPayload.vue'

const props = defineProps({ span: { type: Object, required: true } })
const decision = computed(() => readDecision(props.span.attributes))
const protocol = computed(() => props.span.attributes?.['llmproxy.decision.protocol'])
const percent = new Intl.NumberFormat(undefined, { style: 'percent', maximumFractionDigits: 3 })
const score = new Intl.NumberFormat(undefined, { maximumSignificantDigits: 7 })
const types = { predicate: 'Predicate', noul: 'Noul', choice: 'Choice', score: 'Score', box: 'Bounding box' }
const results = computed(() => decision.value.rows.map(row => {
  const type = row.answer?.type ?? row.question?.type
  return { ...row, type: Object.hasOwn(types, type) ? types[type] : type, probabilities: distribution(row.answer, row.question) }
}))

function display(value) {
  return typeof value === 'string' ? value : JSON.stringify(value)
}

function isProbability(value) {
  return Number.isFinite(value) && value >= 0 && value <= 1
}

function formatProbability(value) {
  return isProbability(value) ? percent.format(value) : display(value)
}
</script>

<template>
  <section class="space-y-3 border-t border-gray-200 pt-3 dark:border-gray-700" aria-label="Decision results">
    <div>
      <h3 class="font-semibold">Decision</h3>
      <p v-if="decision.endpoint" class="mt-1 font-mono text-xs text-gray-600 dark:text-gray-300">{{ decision.endpoint }}</p>
    </div>
    <dl v-if="span.requestModel || span.responseModel || span.provider || protocol" class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-sm">
      <template v-if="span.requestModel"><dt>Requested model</dt><dd class="break-words">{{ span.requestModel }}</dd></template>
      <template v-if="span.responseModel"><dt>Served model</dt><dd class="break-words">{{ span.responseModel }}</dd></template>
      <template v-if="span.provider"><dt>Provider</dt><dd class="break-words">{{ span.provider }}</dd></template>
      <template v-if="protocol"><dt>Upstream protocol</dt><dd class="break-words">{{ protocol }}</dd></template>
    </dl>
    <details v-if="decision.context != null" class="text-sm">
      <summary class="cursor-pointer font-medium">{{ decision.systemOne ? 'Input state' : 'Decision input' }}</summary>
      <p v-if="typeof decision.context === 'string'" class="mt-2 whitespace-pre-wrap break-words">{{ decision.context }}</p>
      <JsonPayload v-else :value="decision.context" class="mt-2" />
    </details>
    <p v-if="decision.refusal != null" class="break-words text-sm">Refusal · {{ display(decision.refusal) }}</p>
    <p v-if="!results.length && decision.refusal == null" class="text-sm text-gray-600 dark:text-gray-300">
      No decision questions or answers were captured for this span.
    </p>
    <ul v-if="results.length" class="space-y-2">
      <li v-for="({ name, type, question, answer, probabilities }, index) in results" :key="index"
        class="space-y-2 rounded-md border border-gray-200 p-3 text-sm dark:border-gray-700">
        <div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
          <h4 class="min-w-0 break-words font-semibold">{{ name || `Question ${index + 1}` }}</h4>
          <span v-if="type" class="break-all text-xs text-gray-600 dark:text-gray-300">{{ type }}</span>
        </div>
        <p v-if="typeof question?.instructions === 'string'" class="whitespace-pre-wrap break-words">{{ question.instructions }}</p>
        <p v-if="!answer" class="text-gray-600 dark:text-gray-300">No answer captured</p>
        <dl v-else class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1">
          <template v-if="answer.refusal != null"><dt>Refusal</dt><dd class="break-words">{{ display(answer.refusal) }}</dd></template>
          <template v-if="answer.choice != null"><dt>Selected</dt><dd class="break-words font-semibold">{{ display(answer.choice) }}</dd></template>
          <template v-if="answer.score != null"><dt>Score</dt><dd class="font-semibold">{{ typeof answer.score === 'number' ? score.format(answer.score) : display(answer.score) }}</dd></template>
          <template v-if="answer.probability != null || answer.noul != null"><dt>Yes probability</dt><dd class="font-semibold">{{ formatProbability(answer.probability ?? answer.noul) }}</dd></template>
          <template v-if="answer.confidence != null"><dt>Confidence</dt><dd>{{ formatProbability(answer.confidence) }}</dd></template>
        </dl>
        <JsonPayload v-if="answer?.box != null" :value="answer.box" />
        <table v-if="probabilities.length" class="w-full table-fixed text-xs">
          <thead>
            <tr class="text-left text-gray-600 dark:text-gray-300">
              <th scope="col" class="pb-1 font-medium">{{ answer.type === 'score' ? 'Level' : 'Option' }}</th>
              <th scope="col" class="w-1/3 pb-1 text-right font-medium">Probability</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="({ value, label, probability }, i) in probabilities" :key="i" class="border-t border-gray-200 dark:border-gray-700">
              <th scope="row" class="py-1 pr-2 text-left font-normal [overflow-wrap:anywhere]">
                {{ display(label ?? value) }}<span v-if="label != null && value != null" class="text-gray-600 dark:text-gray-300"> ({{ display(value) }})</span>
              </th>
              <td class="py-1 text-right tabular-nums">{{ formatProbability(probability) }}</td>
            </tr>
          </tbody>
        </table>
        <details v-if="question" class="text-xs">
          <summary class="cursor-pointer font-medium">Question details</summary>
          <JsonPayload :value="question" class="mt-2" />
        </details>
        <details v-if="answer" class="text-xs">
          <summary class="cursor-pointer font-medium">Answer details</summary>
          <JsonPayload :value="answer" class="mt-2" />
        </details>
      </li>
    </ul>
  </section>
</template>
