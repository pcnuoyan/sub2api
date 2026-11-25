import { onScopeDispose, shallowRef } from 'vue'
import { list } from '@/api/admin/accounts'
import type { AccountListItem } from '@/types'
import { modelTestError } from '@/utils/accountModelTest'

export function useModelTestAccounts() {
  const accounts = shallowRef<AccountListItem[]>([])
  const loading = shallowRef(false)
  const error = shallowRef('')
  let controller: AbortController | undefined

  async function reload() {
    controller?.abort()
    const current = new AbortController()
    controller = current
    loading.value = true
    error.value = ''
    try {
      const first = await list(1, 100, { include_scheduler_score: 'false', sort_by: 'id', sort_order: 'asc' }, { signal: current.signal })
      const pages = Math.max(1, first.pages || Math.ceil(first.total / first.page_size))
      // 全部元数据就绪后才排序、分页，避免把后续页的最近测试账号遗漏。
      if (!Number.isFinite(pages) || pages > 500) throw new Error('ACCOUNT_PAGE_LIMIT')
      const all = new Map(first.items.map(account => [account.id, account]))
      for (let page = 2; page <= pages; page++) {
        const result = await list(page, first.page_size || 100, { include_scheduler_score: 'false', sort_by: 'id', sort_order: 'asc' }, { signal: current.signal })
        if (current.signal.aborted) return
        for (const account of result.items) all.set(account.id, account)
      }
      if (!current.signal.aborted) accounts.value = [...all.values()]
    } catch (cause) {
      if (!current.signal.aborted) error.value = modelTestError(cause)
    } finally {
      if (controller === current) loading.value = false
    }
  }

  onScopeDispose(() => controller?.abort())
  return { accounts, loading, error, reload }
}
