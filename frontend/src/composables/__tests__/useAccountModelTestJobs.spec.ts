import { defineComponent, h, ref } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ModelTestJob, ModelTestRequest, ModelTestResults } from '@/api/admin/accountModelTest'
import { useAccountModelTestJobs } from '../useAccountModelTestJobs'
import { modelTestLegacyKey } from '@/utils/accountModelTestArchive'

const api = vi.hoisted(() => ({ listJobs: vi.fn(), getResults: vi.fn(), createJob: vi.fn(), cancelJob: vi.fn(), clearResults: vi.fn(), importResults: vi.fn() }))
vi.mock('@/api/admin/accountModelTest', () => ({ accountModelTestAPI: api }))

function result(extra: Partial<ModelTestJob> = {}): ModelTestJob {
  return { id: 'a', account_id: 1, account_name: '账号', platform: 'openai', model_id: '6a', prompt: '题目', prompt_key: 'custom', reasoning_effort: '', status: 'running', response_text: '', error_message: '', latency_ms: 0, created_at: '2026-11-24T03:00:00Z', started_at: '2026-11-24T03:00:00Z', finished_at: null, persisted: false, ...extra }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

describe('后台账号测试生命周期', () => {
  let wrapper: VueWrapper | undefined
  let state: ReturnType<typeof useAccountModelTestJobs>
  const adminId = ref<number | undefined>(42)

  function setup() {
    wrapper = mount(defineComponent({ setup() { state = useAccountModelTestJobs(adminId); return () => h('div') } }))
  }

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    localStorage.clear()
    adminId.value = 42
    api.getResults.mockResolvedValue({ results: [], cleared_at: null })
    api.listJobs.mockResolvedValue([])
    api.clearResults.mockResolvedValue({ cleared_at: '2026-11-24T04:00:00Z' })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    vi.useRealTimers()
  })

  it('恢复活跃任务，离开页面只停止轮询而不发送取消', async () => {
    api.listJobs.mockResolvedValue([result()])
    setup()
    await flushPromises()
    expect(state.ready.value).toBe(true)
    expect(state.activeCount.value).toBe(1)
    await vi.advanceTimersByTimeAsync(2000)
    expect(api.listJobs).toHaveBeenCalledTimes(2)
    wrapper!.unmount()
    wrapper = undefined
    await vi.advanceTimersByTimeAsync(20000)
    expect(api.listJobs).toHaveBeenCalledTimes(2)
    expect(api.cancelJob).not.toHaveBeenCalled()
  })

  it('提交使用点击时参数，阻止同一账号的重复提交', async () => {
    setup()
    await flushPromises()
    const pending = deferred<ModelTestJob>()
    api.createJob.mockReturnValue(pending.promise)
    const request: ModelTestRequest = { account_id: 1, model_id: '6a', prompt: '原题', prompt_key: 'custom', reasoning_effort: 'high' }
    const started = state.start(request)
    request.prompt = '新题'
    expect(await state.start(request)).toBe(false)
    expect(api.createJob).toHaveBeenCalledTimes(1)
    expect(api.createJob.mock.calls[0][0].prompt).toBe('原题')
    pending.resolve(result())
    expect(await started).toBe(true)
  })

  it('单独取消只作用于目标任务', async () => {
    api.listJobs.mockResolvedValue([result(), result({ id: 'b', account_id: 2 })])
    api.cancelJob.mockResolvedValue(result({ status: 'cancelled' }))
    setup()
    await flushPromises()
    await state.cancel(result())
    expect(api.cancelJob).toHaveBeenCalledWith('a')
    expect(state.latest.value.get(1)?.status).toBe('cancelled')
    expect(state.latest.value.get(2)?.status).toBe('running')
  })

  it('清空后不会被迟到的轮询结果复活', async () => {
    api.listJobs.mockResolvedValue([result()])
    setup()
    await flushPromises()
    const results = deferred<ModelTestResults>()
    const jobs = deferred<ModelTestJob[]>()
    api.getResults.mockReturnValueOnce(results.promise)
    api.listJobs.mockReturnValueOnce(jobs.promise)
    const refreshing = state.refresh()
    expect(await state.clear()).toBe(true)
    results.resolve({ results: [], cleared_at: null })
    jobs.resolve([result()])
    await refreshing
    expect(state.latest.value.size).toBe(0)
    api.getResults.mockResolvedValue({ results: [], cleared_at: '2026-11-24T04:00:00Z' })
    api.listJobs.mockResolvedValue([result()])
    await state.refresh()
    expect(state.latest.value.size).toBe(0)
  })

  it('服务端读取失败显示错误并保留已恢复的结果', async () => {
    api.listJobs.mockResolvedValue([result({ status: 'success', response_text: '9', persisted: false })])
    setup()
    await flushPromises()
    expect(state.unsavedCount.value).toBe(1)
    api.getResults.mockRejectedValueOnce(new Error('数据库不可用'))
    await state.refresh()
    expect(state.error.value).toBe('数据库不可用')
    expect(state.latest.value.get(1)?.response_text).toBe('9')
  })

  it('切换管理员后不展示前一个管理员的结果', async () => {
    api.listJobs.mockResolvedValueOnce([result()])
    setup()
    await flushPromises()
    expect(state.latest.value.size).toBe(1)
    adminId.value = 99
    await flushPromises()
    expect(state.latest.value.size).toBe(0)
  })

  it('只迁移已知版本且管理员匹配的缓存，并遵循清空水位', async () => {
    localStorage.setItem(modelTestLegacyKey(42), JSON.stringify({ version: 1, admin_id: 42, results: [result({ status: 'success', response_text: '9' })] }))
    api.getResults.mockResolvedValue({ results: [], cleared_at: '2026-11-24T04:00:00Z' })
    setup()
    await flushPromises()
    expect(api.importResults).not.toHaveBeenCalled()
    expect(localStorage.getItem(modelTestLegacyKey(42))).toBeNull()
  })

  it('不导入无归属的未知旧缓存', async () => {
    localStorage.setItem(modelTestLegacyKey(42), JSON.stringify([result({ status: 'success' })]))
    setup()
    await flushPromises()
    expect(api.importResults).not.toHaveBeenCalled()
    expect(localStorage.getItem(modelTestLegacyKey(42))).not.toBeNull()
  })
})
