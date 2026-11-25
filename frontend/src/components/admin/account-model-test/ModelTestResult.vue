<script setup lang="ts">
import { computed, shallowRef } from 'vue'
import { useI18n } from 'vue-i18n'
import type { ModelTestJob } from '@/api/admin/accountModelTest'
import { isActiveModelTest } from '@/utils/accountModelTest'
import { buildModelTestPreview } from '@/utils/accountModelTestPreview'

const { job, now } = defineProps<{ job: ModelTestJob; now: number }>()
const { t, locale } = useI18n()
const showSource = shallowRef(false)
const active = computed(() => isActiveModelTest(job))
const isDrawing = computed(() => job.prompt_key === 'pelican')
const preview = computed(() => isDrawing.value && !active.value && job.response_text ? buildModelTestPreview(job.response_text) : null)
const timestamp = computed(() => {
  const value = new Date(job.created_at)
  return Number.isFinite(value.getTime()) ? value.toLocaleString(locale.value) : '—'
})
const duration = computed(() => {
  const elapsed = active.value && job.started_at ? now - Date.parse(job.started_at) : job.latency_ms
  return t('admin.accountModelTest.seconds', { value: (Math.max(0, Number.isFinite(elapsed) ? elapsed : 0) / 1000).toFixed(1) })
})
</script>

<template>
  <div class="space-y-3 border-t border-gray-100 pt-3 dark:border-dark-700">
    <dl class="space-y-1 text-xs">
      <div class="flex justify-between gap-3"><dt class="text-gray-500">{{ t('admin.accountModelTest.model') }}</dt><dd class="break-all text-right font-mono">{{ job.model_id }}</dd></div>
      <div v-if="job.reasoning_effort" class="flex justify-between gap-3"><dt class="text-gray-500">{{ t('admin.accountModelTest.reasoning') }}</dt><dd>{{ job.reasoning_effort }}</dd></div>
      <div class="flex justify-between gap-3"><dt class="text-gray-500">{{ t('admin.accountModelTest.testTime') }}</dt><dd class="text-right">{{ timestamp }}</dd></div>
      <div class="flex justify-between gap-3"><dt class="text-gray-500">{{ t('admin.accountModelTest.latency') }}</dt><dd class="tabular-nums">{{ duration }}</dd></div>
    </dl>
    <p v-if="job.error_message" role="alert" class="break-words rounded-lg bg-red-50 p-2 text-xs text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ job.error_message }}</p>
    <p v-if="!active && !job.persisted" class="text-xs text-amber-700 dark:text-amber-400">{{ t('admin.accountModelTest.unsaved') }}</p>
    <div v-if="job.response_text" class="space-y-2">
      <div class="flex items-center justify-between gap-2">
        <span class="text-xs font-medium">{{ t('admin.accountModelTest.answer') }}</span>
        <button v-if="isDrawing" type="button" class="text-xs text-primary-600 hover:underline dark:text-primary-400" :aria-pressed="showSource" @click="showSource = !showSource">{{ t(showSource ? 'admin.accountModelTest.preview' : 'admin.accountModelTest.source') }}</button>
      </div>
      <template v-if="isDrawing && !showSource && !active">
        <iframe v-if="preview" :srcdoc="preview" sandbox="" referrerpolicy="no-referrer" :title="t('admin.accountModelTest.preview')" class="h-64 w-full rounded-lg border border-gray-200 bg-white dark:border-dark-600" />
        <p v-else role="alert" class="rounded-lg bg-amber-50 p-3 text-xs text-amber-800 dark:bg-amber-900/20 dark:text-amber-300">{{ t('admin.accountModelTest.previewUnavailable') }}</p>
        <p v-if="preview" class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accountModelTest.previewHint') }}</p>
      </template>
      <pre v-else class="max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-3 text-xs leading-relaxed dark:bg-dark-900">{{ job.response_text }}</pre>
    </div>
    <details class="text-xs text-gray-500 dark:text-gray-400"><summary class="cursor-pointer">{{ t('admin.accountModelTest.promptContent') }}</summary><p class="mt-2 whitespace-pre-wrap break-words">{{ job.prompt }}</p></details>
  </div>
</template>
