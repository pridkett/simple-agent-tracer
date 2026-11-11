import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Explorer from './Explorer.vue'
import Detail from './Detail.vue'
import Login from './Login.vue'
import Projects from './Projects.vue'
import { unauthorized } from './api'
import { canBrowse, checkSession, session } from './session'
import './style.css'

const routes = [
  { path: '/', redirect: '/projects/default/sessions' },
  { path: '/login', name: 'login', component: Login },
  { path: '/projects', component: Projects },
  { path: '/projects/:project/:kind(sessions|traces)', component: Explorer },
  { path: '/projects/:project/:kind(sessions|traces)/:id', component: Detail },
  { path: '/projects/:project/traces/:id/spans/:span', component: Detail, props: { kind: 'traces' } },
  { path: '/projects/:project/sessions/:id/turns/:turn', component: Detail, props: { kind: 'sessions' } },
]

const router = createRouter({ history: createWebHistory(), routes })
router.beforeEach(async to => {
  await checkSession()
  if (to.name === 'login') return canBrowse.value ? '/' : true
  return canBrowse.value || { name: 'login' }
})

unauthorized.addEventListener('session-required', () => {
  session.value = undefined
  void router.replace({ name: 'login' })
})

const app = createApp(App).use(router)
router.isReady().then(() => app.mount('#app'))
