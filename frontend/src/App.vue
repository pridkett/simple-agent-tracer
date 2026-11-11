<script setup>
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { request } from './api'

const route = useRoute()
const router = useRouter()
const projects = ref([])
const auth = ref({ oidc: false, signedIn: false })
const project = computed(() => route.params.project || projects.value[0]?.slug || 'default')
const base = computed(() => '/projects/' + encodeURIComponent(project.value))

async function refresh() {
  try {
    projects.value = await request('/api/v1/projects')
  } catch { projects.value = [] }
}
refresh()
request('/auth/status').then(result => { auth.value = result }).catch(() => {})

async function signOut() {
  await request('/auth/logout', { method: 'POST' })
  window.location.assign('/')
}
</script>

<template>
  <div class="min-h-screen">
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
        <a v-if="auth.oidc && !auth.signedIn" href="/auth/login" class="btn-primary ml-auto">Sign in</a>
        <button v-if="auth.signedIn" class="btn-secondary" @click="signOut">Sign out</button>
      </div>
    </header>
    <main class="mx-auto max-w-screen-2xl px-5 py-7">
      <RouterView v-slot="{ Component }">
        <component :is="Component" @projects-changed="refresh" />
      </RouterView>
    </main>
  </div>
</template>
