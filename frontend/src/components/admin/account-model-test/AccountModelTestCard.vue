<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { AccountListItem } from '@/types'
import type { ModelTestJob } from '@/api/admin/accountModelTest'
import { isActiveModelTest, snapshotBillingRate, snapshotUsagePercent } from '@/utils/accountModelTest'
import ModelTestResult from './ModelTestResult.vue'

const { account, job, now, pending, disabled } = defineProps<{ account: AccountListItem; job?: ModelTestJob; now: number; pending: boolean; disabled: boolean }>()
const emit = defineEmits<{ test: []; cancel: [job: ModelTestJob] }>()
const { t } = useI18n()
const active = computed(() => !!job && isActiveModelTest(job))
const unsupported = computed(() => ['typesafe', 'sora'].includes(account.platform.toLowerCase()))
const unavailable = computed(() => unsupported.value ? t('admin.accountModelTest.unsupportedAccount') : '')
const plan = computed(() => {
  const value = account.credentials?.plan_type || account.parent_plan_type || account.extra?.subscription_tier
  return typeof value === 'string' && value.trim() ? value : '—'
})
const authMode = computed(() => {
  if (account.platform !== 'openai' || account.type !== 'oauth') return ''
  const mode = String(account.credentials?.auth_mode ?? '').toLowerCase()
  return mode.includes('pat') ? 'PAT' : mode.includes('agent') ? 'Agent Identity' : ''
})
const rate = computed(() => {
  const value = snapshotBillingRate(account, now)
  return value == null ? '—' : `${Number(value.toPrecision(6))}×`
})
const usage = computed(() => (['5h', '7d'] as const).map(window => {
  const value = snapshotUsagePercent(account, window, now)
  return { window, label: t(window === '5h' ? 'admin.accountModelTest.usage5h' : 'admin.accountModelTest.usage7d'), value: value == null ? '—' : `${Number(value.toFixed(1))}%` }
}))
const statusClass = computed(() => {
  if (active.value) return 'bg-blue-50 text-blue-700 dark:bg-blue-900/20 dark:text-blue-300'
  if (job?.status === 'success') return 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300'
  if (job?.status === 'failed') return 'bg-red-50 text-red-700 dark:bg-red-900/20 dark:text-red-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
})
</script>

<template>
  <article class="card flex min-w-0 flex-col gap-4 p-4" :data-account-id="account.id">
    <div class="space-y-2">
      <div class="flex items-start justify-between gap-2">
        <h2 class="min-w-0 break-words font-semibold text-gray-900 dark:text-gray-100">{{ account.name }}</h2>
        <span class="shrink-0 rounded-full px-2 py-0.5 text-xs" :class="statusClass">{{ t(job ? `admin.accountModelTest.${job.status}` : 'admin.accountModelTest.noResult') }}</span>
      </div>
      <div class="flex flex-wrap gap-x-2 gap-y-1 text-xs text-gray-500 dark:text-gray-400"><span class="font-mono">#{{ account.id }}</span><span>{{ account.platform }}</span><span>{{ account.type === 'apikey' ? 'API Key' : account.type === 'oauth' ? 'OAuth' : account.type }}</span><span v-if="authMode">{{ authMode }}</span><span v-if="account.status !== 'active'">{{ t(account.status === 'error' ? 'admin.accountModelTest.errorStatus' : 'admin.accountModelTest.inactive') }}</span></div>
    </div>
    <dl class="grid grid-cols-2 gap-3 text-xs" :title="t('admin.accountModelTest.snapshotHint')">
      <div><dt class="mb-1 text-gray-500">{{ t('admin.accountModelTest.plan') }}</dt><dd class="break-words">{{ plan }}</dd></div>
      <div><dt class="mb-1 text-gray-500">{{ t('admin.accountModelTest.billingRate') }}</dt><dd class="font-mono">{{ rate }}</dd></div>
      <div v-for="window in usage" :key="window.window"><dt class="mb-1 text-gray-500">{{ window.label }}</dt><dd class="font-mono">{{ window.value }}</dd></div>
    </dl>
    <div>
      <button v-if="active && job" type="button" class="btn btn-secondary w-full" :disabled="pending || disabled" @click="emit('cancel', job)">{{ t(pending ? 'admin.accountModelTest.cancelling' : 'admin.accountModelTest.cancel') }}</button>
      <button v-else type="button" class="btn btn-primary w-full" :disabled="pending || disabled || !!unavailable" :title="unavailable" @click="emit('test')">{{ t(pending ? 'admin.accountModelTest.starting' : 'admin.accountModelTest.test') }}</button>
      <p v-if="unavailable" class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">{{ unavailable }}</p>
    </div>
    <ModelTestResult v-if="job" :key="job.id" :job="job" :now="now" />
  </article>
</template>
