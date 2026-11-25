<script setup lang="ts">
import { computed, onMounted, onScopeDispose, reactive, shallowRef, useTemplateRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { saveAs } from 'file-saver'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import ModelTestControls from '@/components/admin/account-model-test/ModelTestControls.vue'
import AccountModelTestCard from '@/components/admin/account-model-test/AccountModelTestCard.vue'
import { useAuthStore } from '@/stores/auth'
import { useModelTestAccounts } from '@/composables/useModelTestAccounts'
import { useAccountModelTestJobs } from '@/composables/useAccountModelTestJobs'
import type { ModelTestRequest } from '@/api/admin/accountModelTest'
import { CANDY_PROMPT, MODEL_TEST_PAGE_SIZE, MODEL_TEST_MAX_PROMPT_BYTES, filterAndSortTestAccounts, isActiveModelTest, isTextTestModel, localModelTestOptions } from '@/utils/accountModelTest'
import { createModelTestArchive, parseModelTestArchive, MODEL_TEST_ARCHIVE_MAX_BYTES } from '@/utils/accountModelTestArchive'

const { t } = useI18n()
const auth = useAuthStore()
const adminId = computed(() => auth.user?.id)
const { accounts, loading: accountsLoading, error: accountsError, reload: reloadAccounts } = useModelTestAccounts()
const { latest, activeCount, unsavedCount, ready, error: jobsError, pendingAccounts, clearing, importing, refresh, start, cancel, clear, importResults } = useAccountModelTestJobs(adminId)
const draft = shallowRef<Omit<ModelTestRequest, 'account_id'>>({ model_id: '6a', prompt: CANDY_PROMPT, prompt_key: 'candy', reasoning_effort: '' })
const filters = reactive({ search: '', platform: '', type: '', status: '' })
const page = shallowRef(1)
const now = shallowRef(Date.now())
const showClear = shallowRef(false)
const localError = shallowRef('')
const importNotice = shallowRef('')
const importInput = useTemplateRef<HTMLInputElement>('importInput')
const models = computed(() => localModelTestOptions(accounts.value))
const platforms = computed(() => [...new Set(accounts.value.map(account => account.platform))].sort())
const types = computed(() => [...new Set(accounts.value.map(account => account.type))].sort())
const sorted = computed(() => filterAndSortTestAccounts(accounts.value, latest.value, filters))
const visible = computed(() => sorted.value.slice((page.value - 1) * MODEL_TEST_PAGE_SIZE, page.value * MODEL_TEST_PAGE_SIZE))
const resultCount = computed(() => [...latest.value.values()].filter(job => !isActiveModelTest(job)).length)
const busy = computed(() => clearing.value || importing.value)
const pageError = computed(() => localError.value || jobsError.value || (accountsError.value === 'ACCOUNT_PAGE_LIMIT' ? t('admin.accountModelTest.accountPageLimit') : accountsError.value))
const invalidDraft = computed(() => !isTextTestModel(draft.value.model_id) || !draft.value.prompt.trim() || new TextEncoder().encode(draft.value.prompt.trim()).length > MODEL_TEST_MAX_PROMPT_BYTES)
const ticker = setInterval(() => { now.value = Date.now() }, 1000)

watch(filters, () => { page.value = 1 })
watch(() => sorted.value.length, length => { page.value = Math.min(page.value, Math.max(1, Math.ceil(length / MODEL_TEST_PAGE_SIZE))) })
onMounted(() => { void reloadAccounts() })
onScopeDispose(() => clearInterval(ticker))

async function run(accountId: number) {
  localError.value = ''
  const request = { ...draft.value, account_id: accountId, model_id: draft.value.model_id.trim(), prompt: draft.value.prompt.trim() }
  if (invalidDraft.value) {
    localError.value = t(isTextTestModel(request.model_id) ? 'admin.accountModelTest.invalidPrompt' : 'admin.accountModelTest.textOnly')
    return
  }
  if (await start(request)) page.value = 1
}

async function reload() {
  localError.value = ''
  await Promise.all([reloadAccounts(), refresh()])
}

function exportResults() {
  if (!adminId.value) return
  const archive = createModelTestArchive(adminId.value, latest.value.values())
  const blob = new Blob([JSON.stringify(archive, null, 2)], { type: 'application/json;charset=utf-8' })
  if (blob.size > MODEL_TEST_ARCHIVE_MAX_BYTES) {
    localError.value = t('admin.accountModelTest.archiveTooLarge')
    return
  }
  saveAs(blob, `account-model-tests-${new Date().toISOString().replace(/[:.]/g, '-')}.json`)
}

async function importFile(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file || !adminId.value) return
  localError.value = ''
  importNotice.value = ''
  if (file.size > MODEL_TEST_ARCHIVE_MAX_BYTES) {
    localError.value = t('admin.accountModelTest.archiveTooLarge')
    return
  }
  try {
    const records = parseModelTestArchive(await file.text(), adminId.value)
    if (await importResults(records)) importNotice.value = t('admin.accountModelTest.importDone')
  } catch {
    localError.value = t('admin.accountModelTest.archiveInvalid')
  }
}

async function confirmClear() {
  showClear.value = false
  importNotice.value = ''
  if (await clear()) page.value = 1
}
</script>

<template>
  <AppLayout>
    <div class="space-y-5 text-gray-800 dark:text-gray-200">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <h1 class="text-xl font-semibold">{{ t('admin.accountModelTest.title') }}</h1>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary" :disabled="accountsLoading || busy" @click="reload"><Icon name="refresh" size="sm" class="mr-1.5" />{{ t('admin.accountModelTest.refresh') }}</button>
          <button type="button" class="btn btn-secondary" :disabled="!resultCount" @click="exportResults">{{ t('admin.accountModelTest.export') }}</button>
          <button type="button" class="btn btn-secondary" :disabled="!ready || busy" :title="t('admin.accountModelTest.importHint')" @click="importInput?.click()">{{ t(importing ? 'admin.accountModelTest.saving' : 'admin.accountModelTest.import') }}</button>
          <button type="button" class="btn btn-danger" :disabled="!ready || busy || (!latest.size && !resultCount)" @click="showClear = true">{{ t('admin.accountModelTest.clear') }}</button>
          <input ref="importInput" type="file" accept="application/json,.json" class="hidden" :aria-label="t('admin.accountModelTest.import')" @change="importFile" />
        </div>
      </div>
      <ModelTestControls v-model="draft" :models="models" />
      <div v-if="pageError" role="alert" class="flex items-center justify-between gap-3 rounded-xl border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-900/20 dark:text-red-300"><span class="break-words">{{ pageError }}</span><button type="button" class="shrink-0 underline" :disabled="accountsLoading || busy" @click="reload">{{ t('admin.accountModelTest.retry') }}</button></div>
      <p v-if="unsavedCount" role="status" class="rounded-xl bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300">{{ t('admin.accountModelTest.unsavedBanner', { count: unsavedCount }) }}</p>
      <p v-if="importNotice" role="status" class="text-sm text-emerald-700 dark:text-emerald-400">{{ importNotice }}</p>
      <div class="space-y-3">
        <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <input v-model="filters.search" class="input" :placeholder="t('admin.accountModelTest.search')" :aria-label="t('admin.accountModelTest.search')" />
          <select v-model="filters.platform" class="input" :aria-label="t('admin.accountModelTest.platform')"><option value="">{{ t('admin.accountModelTest.allPlatforms') }}</option><option v-for="platform in platforms" :key="platform" :value="platform">{{ platform }}</option></select>
          <select v-model="filters.type" class="input" :aria-label="t('admin.accountModelTest.accountType')"><option value="">{{ t('admin.accountModelTest.allTypes') }}</option><option v-for="type in types" :key="type" :value="type">{{ type === 'apikey' ? 'API Key' : type === 'oauth' ? 'OAuth' : type }}</option></select>
          <select v-model="filters.status" class="input" :aria-label="t('admin.accountModelTest.accountStatus')"><option value="">{{ t('admin.accountModelTest.allStatuses') }}</option><option value="active">{{ t('admin.accountModelTest.active') }}</option><option value="inactive">{{ t('admin.accountModelTest.inactive') }}</option><option value="error">{{ t('admin.accountModelTest.errorStatus') }}</option></select>
        </div>
        <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-gray-500 dark:text-gray-400"><span>{{ t('admin.accountModelTest.sortNotice') }}</span><span>{{ t('admin.accountModelTest.runningCount', { count: activeCount }) }} · {{ t('admin.accountModelTest.resultCount', { count: resultCount }) }}</span></div>
      </div>
      <div v-if="accountsLoading || !ready" role="status" class="card p-8 text-center text-sm text-gray-500">{{ t(pageError ? 'admin.accountModelTest.loadFailed' : 'admin.accountModelTest.loading') }}</div>
      <div v-else-if="!visible.length" class="card space-y-2 p-10 text-center"><p>{{ t('admin.accountModelTest.empty') }}</p><p class="text-sm text-gray-500">{{ t('admin.accountModelTest.emptyHint') }}</p></div>
      <div v-else class="grid grid-cols-1 items-start gap-4 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4">
        <AccountModelTestCard v-for="account in visible" :key="account.id" :account="account" :job="latest.get(account.id)" :now="now" :pending="pendingAccounts.has(account.id)" :disabled="busy || (!isActiveModelTest(latest.get(account.id) ?? { status: 'failed' }) && invalidDraft)" @test="run(account.id)" @cancel="cancel" />
      </div>
      <Pagination v-if="sorted.length > MODEL_TEST_PAGE_SIZE && !accountsLoading" v-model:page="page" :total="sorted.length" :page-size="MODEL_TEST_PAGE_SIZE" :show-page-size-selector="false" />
      <ConfirmDialog :show="showClear" :title="t('admin.accountModelTest.clearTitle')" :message="t('admin.accountModelTest.clearMessage')" :confirm-text="t('admin.accountModelTest.clearConfirm')" danger @confirm="confirmClear" @cancel="showClear = false" />
    </div>
  </AppLayout>
</template>
