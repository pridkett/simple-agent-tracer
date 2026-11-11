<script setup>
import { ref } from 'vue'
import { projectPath, request } from './api'

const emit = defineEmits(['projects-changed'])
const projects = ref([])
const slug = ref('')
const error = ref('')
const busy = ref(false)

async function load() {
  try { projects.value = await request('/api/v1/projects') } catch (e) { error.value = e.message }
}

async function save(project) {
  busy.value = true
  error.value = ''
  try {
    if (project) {
      await request(projectPath(project.slug), { method: 'PATCH', body: { retentionDays: project.retentionDays, captureContent: project.captureContent } })
    } else {
      await request('/api/v1/projects', { method: 'POST', body: { slug: slug.value } })
      slug.value = ''
    }
    await load()
    emit('projects-changed')
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
load()
</script>

<template>
  <h1 class="mb-5 text-2xl font-semibold">Projects</h1>
  <p v-if="error" role="alert" class="mb-4 text-sm text-red-700 dark:text-red-300">{{ error }}</p>
  <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
    <form v-for="project in projects" :key="project.id" class="card space-y-4 p-5" @submit.prevent="save(project)">
      <h2 class="font-semibold">{{ project.slug }}</h2>
      <label class="field block">Retention days
        <input v-model.number="project.retentionDays" type="number" min="1" max="36500" required class="input mt-1">
      </label>
      <label class="flex items-center gap-2 text-sm">
        <input v-model="project.captureContent" type="checkbox"> Store captured content from new spans
      </label>
      <button class="btn-primary" :disabled="busy">Save</button>
    </form>
    <form class="card space-y-4 p-5" @submit.prevent="save()">
      <h2 class="font-semibold">New project</h2>
      <label class="field block">Project slug
        <input v-model="slug" pattern="[a-z0-9][a-z0-9\-]{0,62}" required class="input mt-1">
      </label>
      <button class="btn-secondary" :disabled="busy">Create project</button>
    </form>
  </div>
</template>
