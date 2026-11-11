import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import Explorer from './Explorer.vue'
import Detail from './Detail.vue'
import Projects from './Projects.vue'
import './style.css'

const routes = [
  { path: '/', redirect: '/projects/default/sessions' },
  { path: '/projects', component: Projects },
  { path: '/projects/:project/:kind(sessions|traces)', component: Explorer },
  { path: '/projects/:project/:kind(sessions|traces)/:id', component: Detail },
  { path: '/projects/:project/traces/:id/spans/:span', component: Detail, props: { kind: 'traces' } },
  { path: '/projects/:project/sessions/:id/turns/:turn', component: Detail, props: { kind: 'sessions' } },
]

createApp(App).use(createRouter({ history: createWebHistory(), routes })).mount('#app')
