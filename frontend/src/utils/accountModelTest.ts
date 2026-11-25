import type { AccountListItem } from '@/types'
import type { ModelTestJob } from '@/api/admin/accountModelTest'

export const MODEL_TEST_PAGE_SIZE = 25
export const MODEL_TEST_MAX_PROMPT_BYTES = 16384
export const MODEL_TEST_REASONING = ['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const
export const CANDY_PROMPT = '小明原来有 12 颗糖，给了小红 5 颗，又从妈妈那里得到 2 颗。小明现在有多少颗糖？只输出最终数字，不要解释。'
export const PELICAN_PROMPT = '请返回一份完整、可独立打开的 HTML，包含 SVG 2D 动画：一只鹈鹕骑自行车。使用内联 SVG 和 CSS 或 SVG 动画展示车轮转动、踩踏和鹈鹕的动作。不要使用 JavaScript、外部资源、图片、链接或网络请求。直接输出 HTML 源码，不要解释。'

export function isActiveModelTest(job: Pick<ModelTestJob, 'status'>): boolean {
  return job.status === 'queued' || job.status === 'running'
}

export function isTextTestModel(model: string): boolean {
  return !!model.trim() && !/\s/.test(model.trim()) && new TextEncoder().encode(model.trim()).length <= 200 && !/(?:typesafe|sora|dall[ -]?e|whisper|transcrib|veo|(?:^|[-_/.])(?:image|imagen|video|audio|tts|stt|realtime|embedding|embeddings|rerank|vision-only)(?:$|[-_/.])|grok-imagine|seedance|stable-diffusion)/i.test(model)
}

export function compareModelTestTime(left: string, right: string): number {
  const milliseconds = Date.parse(left) - Date.parse(right)
  if (milliseconds !== 0) return milliseconds
  const remainder = (value: string) => Number((/\.(\d+)(?:Z|[+-]\d{2}:\d{2})$/i.exec(value)?.[1] ?? '').padEnd(9, '0').slice(3, 9))
  return remainder(left) - remainder(right)
}

export function latestModelTests(jobs: readonly ModelTestJob[], clearedAt: string | null = null): Map<number, ModelTestJob> {
  const latest = new Map<number, ModelTestJob>()
  for (const job of jobs) {
    const time = Date.parse(job.created_at)
    if (!Number.isFinite(time) || (clearedAt && compareModelTestTime(job.created_at, clearedAt) <= 0)) continue
    const previous = latest.get(job.account_id)
    if (!previous || compareModelTestTime(job.created_at, previous.created_at) > 0 || (job.created_at === previous.created_at && job.id === previous.id)) {
      latest.set(job.account_id, job)
    }
  }
  let completed = 0
  return new Map([...latest.entries()].sort(([, a], [, b]) => compareModelTestTime(b.created_at, a.created_at)).filter(([, job]) => isActiveModelTest(job) || ++completed <= 500))
}

export interface ModelTestAccountFilters {
  search: string
  platform: string
  type: string
  status: string
}

export function filterAndSortTestAccounts(accounts: readonly AccountListItem[], latest: ReadonlyMap<number, ModelTestJob>, filters: ModelTestAccountFilters): AccountListItem[] {
  const query = filters.search.trim().toLocaleLowerCase()
  return accounts.filter(account =>
    (!query || account.name.toLocaleLowerCase().includes(query) || String(account.id).includes(query)) &&
    (!filters.platform || account.platform === filters.platform) &&
    (!filters.type || account.type === filters.type) &&
    (!filters.status || account.status === filters.status)
  ).sort((a, b) => {
    const aJob = latest.get(a.id)
    const bJob = latest.get(b.id)
    return (aJob && bJob ? compareModelTestTime(bJob.created_at, aJob.created_at) : aJob ? -1 : bJob ? 1 : 0) || a.id - b.id
  })
}

export function localModelTestOptions(accounts: readonly AccountListItem[]): string[] {
  const models = new Set(['6a'])
  for (const account of accounts) {
    const mapping = account.credentials?.model_mapping
    if (mapping && typeof mapping === 'object' && !Array.isArray(mapping)) {
      for (const model of Object.keys(mapping)) {
        if (!model.includes('*') && isTextTestModel(model)) models.add(model)
      }
    }
    const allowed = account.credentials?.allowed_models
    if (Array.isArray(allowed)) {
      for (const model of allowed) if (typeof model === 'string' && isTextTestModel(model)) models.add(model)
    }
  }
  return ['6a', ...[...models].filter(model => model !== '6a').sort()]
}

export function snapshotUsagePercent(account: AccountListItem, window: '5h' | '7d', now: number): number | null {
  const extra = account.extra
  if (!extra) return null
  let value: unknown
  let reset: unknown
  if (account.platform === 'openai') {
    value = extra[window === '5h' ? 'codex_5h_used_percent' : 'codex_7d_used_percent']
    reset = extra[window === '5h' ? 'codex_5h_reset_at' : 'codex_7d_reset_at']
  } else if (account.platform === 'anthropic') {
    value = extra[window === '5h' ? 'session_window_utilization' : 'passive_usage_7d_utilization']
    reset = window === '5h' ? account.session_window_end : extra.passive_usage_7d_reset
    if (typeof value === 'number') value *= 100
  }
  if (typeof reset === 'string' && Number.isFinite(Date.parse(reset)) && Date.parse(reset) <= now) return null
  if (typeof reset === 'number' && Number.isFinite(reset) && reset * 1000 <= now) return null
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}

export function snapshotBillingRate(account: AccountListItem, now: number): number | null {
  const snapshot = account.extra?.upstream_billing_probe
  const billing = snapshot?.data
  if (account.type !== 'apikey' || !snapshot || !billing || billing.billing_scope !== 'token' || !['ok', 'failed'].includes(snapshot.status)) return null
  const received = Date.parse(snapshot.received_at ?? '')
  const next = Date.parse(snapshot.next_probe_at ?? '')
  const fresh = snapshot.fresh_until ? Date.parse(snapshot.fresh_until) : snapshot.status === 'ok' && next > received ? received + 2 * (next - received) : NaN
  if (!Number.isFinite(received) || received > now + 300_000 || !Number.isFinite(fresh) || fresh <= received || now > fresh) return null
  const base = billing.resolved_rate_multiplier
  if (typeof base !== 'number' || !Number.isFinite(base) || base < 0 || typeof billing.peak_rate_enabled !== 'boolean') return null
  if (!billing.peak_rate_enabled) return base
  const parseMinute = (text?: string): number | null => {
    const match = /^(\d{2}):(\d{2})$/.exec(text ?? '')
    return match && Number(match[1]) < 24 && Number(match[2]) < 60 ? Number(match[1]) * 60 + Number(match[2]) : null
  }
  const start = parseMinute(billing.peak_start)
  const end = parseMinute(billing.peak_end)
  const peak = billing.peak_rate_multiplier
  if (start == null || end == null || start >= end || typeof peak !== 'number' || !Number.isFinite(peak) || peak < 0 || !billing.timezone) return null
  try {
    const parts = new Intl.DateTimeFormat('en', { timeZone: billing.timezone, hourCycle: 'h23', hour: '2-digit', minute: '2-digit' }).formatToParts(now)
    const minute = Number(parts.find(part => part.type === 'hour')?.value) * 60 + Number(parts.find(part => part.type === 'minute')?.value)
    const value = minute >= start && minute < end ? base * peak : base
    return Number.isFinite(value) ? value : null
  } catch {
    return null
  }
}

export function modelTestError(error: unknown): string {
  if (error && typeof error === 'object') {
    const value = error as { message?: string; response?: { data?: { message?: string } } }
    return value.response?.data?.message || value.message || String(error)
  }
  return String(error)
}
