<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useReveal } from '@/composables/useReveal'
import Button from '@/components/ui/Button.vue'
import { Home, ArrowLeft } from 'lucide-vue-next'

const router = useRouter()
const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

function goHome() {
  router.push('/')
}

function goBack() {
  router.back()
}
</script>

<template>
  <div
    ref="revealRoot"
    class="min-h-screen flex items-center justify-center p-4 sm:p-6 bg-[var(--bg-primary)] transition-colors duration-200"
  >
    <div class="relative z-10 w-full max-w-md text-center" data-reveal>
      <div
        class="text-[clamp(6rem,20vw,9rem)] font-bold leading-none tracking-tight text-[var(--text-primary)] select-none opacity-20"
        aria-hidden="true"
      >
        404
      </div>

      <div class="mt-4">
        <h1 class="text-2xl font-bold tracking-tight text-[var(--text-primary)]">
          {{ $t('error.notFound') }}
        </h1>
        <p class="mt-2 text-sm text-[var(--text-secondary)]">
          {{ $t('error.notFoundDesc') || $t('error.notFound') }}
        </p>
      </div>

      <div class="mt-8 flex flex-col sm:flex-row items-center justify-center gap-3">
        <Button variant="secondary" size="md" @click="goBack">
          <ArrowLeft class="w-4 h-4 mr-2" />
          {{ $t('common.back') }}
        </Button>
        <Button variant="primary" size="md" @click="goHome">
          <Home class="w-4 h-4 mr-2" />
          {{ $t('error.backHome') }}
        </Button>
      </div>
    </div>
  </div>
</template>
