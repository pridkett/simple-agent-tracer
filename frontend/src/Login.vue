<script setup>
import { session } from './session'

function retry() {
  window.location.reload()
}
</script>

<template>
  <main class="flex min-h-screen items-center justify-center px-4">
    <section aria-labelledby="login-title" class="w-full max-w-md space-y-8 rounded-lg bg-white p-8 shadow dark:bg-gray-800">
      <div class="text-center">
        <h1 id="login-title" class="text-3xl font-bold">Simple Agent Tracer</h1>
        <p v-if="session?.oidc" class="mt-2 text-sm text-gray-600 dark:text-gray-300">Sign in to access the trace explorer</p>
        <p v-else-if="session === null" role="alert" class="mt-2 text-sm text-gray-600 dark:text-gray-300">Unable to check sign-in status.</p>
        <p v-else class="mt-2 text-sm text-gray-600 dark:text-gray-300">Configure OIDC or open the tracer in local mode to use the web interface.</p>
      </div>
      <a v-if="session?.oidc" href="/auth/login" class="btn-primary w-full py-3">Sign in with SSO</a>
      <button v-else-if="session === null" class="btn-secondary w-full py-3" @click="retry">Try again</button>
    </section>
  </main>
</template>
