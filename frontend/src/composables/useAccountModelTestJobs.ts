import { computed, onScopeDispose, shallowRef, watch, type Ref } from 'vue'
import { accountModelTestAPI, type ModelTestJob, type ModelTestRequest } from '@/api/admin/accountModelTest'
import { compareModelTestTime, isActiveModelTest, latestModelTests, modelTestError } from '@/utils/accountModelTest'
import { modelTestLegacyKey, parseModelTestArchive, splitModelTestImport } from '@/utils/accountModelTestArchive'

export function useAccountModelTestJobs(adminId: Readonly<Ref<number | undefined>>) {
  const jobs = shallowRef<ModelTestJob[]>([])
  const clearedAt = shallowRef<string | null>(null)
  const ready = shallowRef(false)
  const loading = shallowRef(false)
  const error = shallowRef('')
  const pendingAccounts = shallowRef(new Set<number>())
  const clearing = shallowRef(false)
  const importing = shallowRef(false)
  let disposed = false
  let generation = 0
  let controller: AbortController | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  let migrationChecked = false

  const latest = computed(() => latestModelTests(jobs.value, clearedAt.value))
  const activeCount = computed(() => [...latest.value.values()].filter(isActiveModelTest).length)
  const unsavedCount = computed(() => [...latest.value.values()].filter(job => !isActiveModelTest(job) && !job.persisted).length)

  function updateJob(job: ModelTestJob) {
    jobs.value = jobs.value.filter(existing => existing.id !== job.id).concat(job)
  }

  function schedule() {
    clearTimeout(timer)
    if (!disposed && adminId.value) timer = setTimeout(() => { void refresh() }, activeCount.value ? 2000 : 10000)
  }

  async function refresh() {
    clearTimeout(timer)
    controller?.abort()
    if (disposed || !adminId.value) return
    const current = new AbortController()
    controller = current
    const version = generation
    loading.value = true
    try {
      const [results, remoteJobs] = await Promise.all([
        accountModelTestAPI.getResults(current.signal),
        accountModelTestAPI.listJobs(current.signal)
      ])
      if (disposed || current.signal.aborted || version !== generation) return
      clearedAt.value = results.cleared_at
      jobs.value = [...results.results, ...remoteJobs]
      ready.value = true
      error.value = ''
      if (!migrationChecked) {
        migrationChecked = true
        await migrateKnownCache()
      }
    } catch (cause) {
      if (!disposed && !current.signal.aborted && version === generation) error.value = modelTestError(cause)
    } finally {
      if (controller === current) {
        loading.value = false
        schedule()
      }
    }
  }

  async function start(request: ModelTestRequest): Promise<boolean> {
    if (!ready.value || clearing.value || importing.value || pendingAccounts.value.has(request.account_id) || isActiveModelTest(latest.value.get(request.account_id) ?? { status: 'failed' })) return false
    const owner = adminId.value
    if (!owner) return false
    pendingAccounts.value = new Set(pendingAccounts.value).add(request.account_id)
    generation++
    try {
      const job = await accountModelTestAPI.createJob({ ...request })
      if (disposed || owner !== adminId.value) return false
      generation++
      updateJob(job)
      error.value = ''
      schedule()
      return true
    } catch (cause) {
      if (!disposed && owner === adminId.value) error.value = modelTestError(cause)
      return false
    } finally {
      if (owner === adminId.value) {
        const pending = new Set(pendingAccounts.value)
        pending.delete(request.account_id)
        pendingAccounts.value = pending
      }
    }
  }

  async function cancel(job: ModelTestJob) {
    if (pendingAccounts.value.has(job.account_id)) return
    const owner = adminId.value
    pendingAccounts.value = new Set(pendingAccounts.value).add(job.account_id)
    generation++
    try {
      const result = await accountModelTestAPI.cancelJob(job.id)
      if (disposed || owner !== adminId.value) return
      generation++
      updateJob(result)
      error.value = ''
    } catch (cause) {
      if (!disposed && owner === adminId.value) error.value = modelTestError(cause)
    } finally {
      if (owner === adminId.value) {
        const pending = new Set(pendingAccounts.value)
        pending.delete(job.account_id)
        pendingAccounts.value = pending
        schedule()
      }
    }
  }

  async function clear(): Promise<boolean> {
    if (clearing.value) return false
    const owner = adminId.value
    clearing.value = true
    generation++
    controller?.abort()
    try {
      const result = await accountModelTestAPI.clearResults()
      if (disposed || owner !== adminId.value) return false
      generation++
      clearedAt.value = result.cleared_at
      jobs.value = []
      try { if (owner) localStorage.removeItem(modelTestLegacyKey(owner)) } catch { /* 浏览器禁用存储时仍以服务端水位为准。 */ }
      error.value = ''
      return true
    } catch (cause) {
      if (!disposed && owner === adminId.value) error.value = modelTestError(cause)
      return false
    } finally {
      clearing.value = false
      schedule()
    }
  }

  async function importResults(results: ModelTestJob[]): Promise<boolean> {
    if (importing.value || clearing.value || !ready.value) return false
    const owner = adminId.value
    const cutoff = clearedAt.value
    const eligible = results.filter(job => !cutoff || compareModelTestTime(job.created_at, cutoff) > 0)
    if (!eligible.length) return true
    importing.value = true
    generation++
    try {
      for (const batch of splitModelTestImport(eligible)) {
        if (disposed || owner !== adminId.value) return false
        const result = await accountModelTestAPI.importResults(batch)
        if (disposed || owner !== adminId.value) return false
        generation++
        clearedAt.value = result.cleared_at
        jobs.value = [...latestModelTests([...jobs.value, ...result.results], result.cleared_at).values()]
      }
      error.value = ''
      return true
    } catch (cause) {
      if (!disposed && owner === adminId.value) error.value = modelTestError(cause)
      return false
    } finally {
      importing.value = false
      schedule()
    }
  }

  async function migrateKnownCache() {
    const owner = adminId.value
    if (!owner) return
    try {
      const cached = localStorage.getItem(modelTestLegacyKey(owner))
      if (!cached) return
      const records = parseModelTestArchive(cached, owner)
      if (await importResults(records)) localStorage.removeItem(modelTestLegacyKey(owner))
    } catch {
      // 未知旧二开格式保留原样，不猜测归属，也不把它导入其他管理员的结果。
    }
  }

  watch(adminId, () => {
    generation++
    controller?.abort()
    jobs.value = []
    clearedAt.value = null
    ready.value = false
    migrationChecked = false
    pendingAccounts.value = new Set()
    void refresh()
  }, { immediate: true })

  onScopeDispose(() => {
    disposed = true
    controller?.abort()
    clearTimeout(timer)
    // 这里只终止界面读取；后台任务的取消必须由用户点击对应按钮。
  })

  return { latest, activeCount, unsavedCount, ready, loading, error, pendingAccounts, clearing, importing, refresh, start, cancel, clear, importResults }
}
