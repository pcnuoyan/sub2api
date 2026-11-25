import { defineComponent, h } from 'vue'
import { mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AccountListItem, PaginatedResponse } from '@/types'
import { useModelTestAccounts } from '../useModelTestAccounts'

const { list } = vi.hoisted(() => ({ list: vi.fn() }))
vi.mock('@/api/admin/accounts', () => ({ list }))

function page(ids: number[], current = 1, pages = 1): PaginatedResponse<AccountListItem> {
  return { items: ids.map(id => ({ id, name: `账号 ${id}`, platform: 'openai', type: 'oauth', status: 'active' } as AccountListItem)), page: current, pages, total: ids.length, page_size: 100 }
}

describe('账号测试全量元数据加载', () => {
  let wrapper: VueWrapper
  let state: ReturnType<typeof useModelTestAccounts>

  beforeEach(() => {
    vi.clearAllMocks()
    wrapper = mount(defineComponent({ setup() { state = useModelTestAccounts(); return () => h('div') } }))
  })

  afterEach(() => wrapper.unmount())

  it('完成所有分页后一次发布，并去除跨页重复账号', async () => {
    list.mockResolvedValueOnce(page([1, 2], 1, 2)).mockResolvedValueOnce(page([2, 30], 2, 2))
    await state.reload()
    expect(list).toHaveBeenCalledTimes(2)
    expect(state.accounts.value.map(account => account.id)).toEqual([1, 2, 30])
    expect(state.error.value).toBe('')
  })

  it('请求失败时不发布不完整分页结果', async () => {
    list.mockResolvedValueOnce(page([1], 1, 2)).mockRejectedValueOnce(new Error('第二页失败'))
    await state.reload()
    expect(state.accounts.value).toEqual([])
    expect(state.error.value).toBe('第二页失败')
  })

  it('新加载取消旧加载，旧响应不能覆盖新列表', async () => {
    let resolve!: (value: PaginatedResponse<AccountListItem>) => void
    list.mockImplementationOnce(() => new Promise(done => { resolve = done })).mockResolvedValueOnce(page([99]))
    const oldLoad = state.reload()
    const oldSignal = list.mock.calls[0][3].signal as AbortSignal
    await state.reload()
    expect(oldSignal.aborted).toBe(true)
    resolve(page([1]))
    await oldLoad
    expect(state.accounts.value.map(account => account.id)).toEqual([99])
  })

  it('拒绝异常页数，避免无界拉取或错误的局部排序', async () => {
    list.mockResolvedValueOnce(page([1], 1, 501))
    await state.reload()
    expect(list).toHaveBeenCalledTimes(1)
    expect(state.accounts.value).toEqual([])
    expect(state.error.value).toBe('ACCOUNT_PAGE_LIMIT')
  })
})
