<script setup>
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { request } from './api'
import { canBrowse, session } from './session'

const route = useRoute()
const router = useRouter()
const projects = ref([])
const logoutError = ref('')
const project = computed(() => route.params.project || projects.value[0]?.slug || 'default')
const base = computed(() => '/projects/' + encodeURIComponent(project.value))

async function refresh() {
  try {
    projects.value = await request('/api/v1/projects')
  } catch { projects.value = [] }
}
watch(canBrowse, allowed => {
  if (allowed) refresh()
  else projects.value = []
}, { immediate: true })

async function signOut() {
  logoutError.value = ''
  try {
    await request('/auth/logout', { method: 'POST' })
  } catch (error) {
    if (error.status !== 403) {
      logoutError.value = 'Unable to sign out. Try again.'
      return
    }
  }
  window.location.assign('/')
}
</script>

<template>
  <RouterView v-if="route.name === 'login'" />
  <div v-else-if="canBrowse" class="min-h-screen">
    <header class="border-b border-gray-200 bg-white dark:bg-gray-800">
      <div class="mx-auto flex max-w-screen-2xl flex-wrap items-center gap-x-6 gap-y-3 px-5 py-4">
        <RouterLink :to="base + '/sessions'" class="font-semibold tracking-tight">Simple Agent Tracer</RouterLink>
        <nav aria-label="Main navigation" class="flex items-center gap-5 text-sm">
          <RouterLink :to="base + '/sessions'" class="link">Sessions</RouterLink>
          <RouterLink :to="base + '/traces'" class="link">Traces</RouterLink>
          <RouterLink to="/projects" class="link">Projects</RouterLink>
        </nav>
        <label v-if="projects.length" class="ml-auto flex items-center gap-2 text-sm">
          Project
          <select :value="project" class="input" @change="router.push('/projects/' + encodeURIComponent($event.target.value) + '/' + (route.params.kind || 'sessions'))">
            <option v-for="p in projects" :key="p.id" :value="p.slug">{{ p.slug }}</option>
          </select>
        </label>
        <button v-if="session?.signedIn" class="btn-secondary" @click="signOut">Sign out</button>
      </div>
    </header>
    <main class="mx-auto max-w-screen-2xl px-5 py-7">
      <p v-if="logoutError" role="alert" class="mb-4 text-sm text-red-700 dark:text-red-300">{{ logoutError }}</p>
      <RouterView v-slot="{ Component }">
        <component :is="Component" @projects-changed="refresh" />
      </RouterView>
    </main>
  </div>
</template>
