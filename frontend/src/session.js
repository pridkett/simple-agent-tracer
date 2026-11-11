import { computed, shallowRef } from 'vue'
import { request } from './api'

export const session = shallowRef(undefined)
export const canBrowse = computed(() => Boolean(session.value?.local || session.value?.signedIn))

let pending
export async function checkSession() {
  if (session.value !== undefined) return
  pending ??= request('/auth/status')
    .then(status => { session.value = status })
    .catch(() => { session.value = null })
    .finally(() => { pending = undefined })
  await pending
}
