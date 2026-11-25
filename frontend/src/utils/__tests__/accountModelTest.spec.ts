import { describe, expect, it } from 'vitest'
import type { AccountListItem } from '@/types'
import type { ModelTestJob } from '@/api/admin/accountModelTest'
import { filterAndSortTestAccounts, isTextTestModel, latestModelTests, localModelTestOptions, snapshotBillingRate, snapshotUsagePercent } from '../accountModelTest'
import { createModelTestArchive, parseModelTestArchive, splitModelTestImport } from '../accountModelTestArchive'
import { buildModelTestPreview, MODEL_TEST_PREVIEW_CSP } from '../accountModelTestPreview'

function account(id: number, extra: Partial<AccountListItem> = {}): AccountListItem {
  return { id, name: `账号 ${id}`, platform: 'openai', type: 'oauth', status: 'active', ...extra } as AccountListItem
}

function job(extra: Partial<ModelTestJob> = {}): ModelTestJob {
  return { id: 'job-1', account_id: 30, account_name: '账号 30', platform: 'openai', model_id: '6a', prompt: '题目', prompt_key: 'custom', reasoning_effort: '', status: 'success', response_text: '回答', error_message: '', latency_ms: 123, created_at: '2026-11-24T03:00:00Z', started_at: null, finished_at: '2026-11-24T03:00:01Z', persisted: true, ...extra }
}

describe('账号模型测试数据', () => {
  it('先全局按最新结果排序，再进行 25 条分页，且筛选账号 ID', () => {
    const accounts = Array.from({ length: 40 }, (_, index) => account(index + 1))
    const latest = latestModelTests([job()])
    const filters = { search: '', platform: '', type: '', status: '' }
    expect(filterAndSortTestAccounts(accounts, latest, filters).slice(0, 25)[0].id).toBe(30)
    expect(filterAndSortTestAccounts(accounts, latest, { ...filters, search: '30' }).map(item => item.id)).toEqual([30])
    expect(filterAndSortTestAccounts(accounts, latest, { ...filters, status: 'inactive' })).toEqual([])
  })

  it('同一账号仅保留最近一次结果，并屏蔽水位之前的活跃任务', () => {
    expect(latestModelTests([job(), job({ id: 'old', created_at: '2026-11-23T00:00:00Z' })]).get(30)?.id).toBe('job-1')
    expect(latestModelTests([job({ status: 'running' })], '2026-11-24T04:00:00Z').size).toBe(0)
  })

  it('保留清空后同一毫秒内新建的任务，最多保留 500 个终态账号', () => {
    expect(latestModelTests([job({ created_at: '2026-11-24T03:00:00.123900Z' })], '2026-11-24T03:00:00.123100Z').size).toBe(1)
    expect(latestModelTests(Array.from({ length: 520 }, (_, index) => job({ id: String(index), account_id: index + 1 }))).size).toBe(500)
  })

  it.each(['gpt-image-1', 'sora-2', 'dall-e-3', 'whisper-1', 'tts-1', 'gpt-realtime', 'text-embedding-3-small', 'grok-imagine', 'typesafe'])('拒绝非文本模型 %s', model => {
    expect(isTextTestModel(model)).toBe(false)
  })

  it('始终保留 6a，且只读取配置里的文本模型', () => {
    expect(localModelTestOptions([])).toEqual(['6a'])
    expect(localModelTestOptions([account(1, { credentials: { model_mapping: { 'my-text': 'gpt-5', 'gpt-image-1': 'x', '*': 'y' }, allowed_models: ['6a', 'my-text', 'claude-sonnet'] } })])).toEqual(['6a', 'claude-sonnet', 'my-text'])
  })

  it('区分缺失、真实零值、过期用量', () => {
    const now = Date.parse('2026-11-24T04:00:00Z')
    expect(snapshotUsagePercent(account(1), '5h', now)).toBeNull()
    expect(snapshotUsagePercent(account(1, { extra: { codex_5h_used_percent: 0 } }), '5h', now)).toBe(0)
    expect(snapshotUsagePercent(account(1, { extra: { codex_5h_used_percent: 5, codex_5h_reset_at: '2026-11-24T03:00:00Z' } }), '5h', now)).toBeNull()
    const claude = account(2, { platform: 'anthropic', extra: { session_window_utilization: 0.35, passive_usage_7d_utilization: 0, passive_usage_7d_reset: now / 1000 + 100 } })
    expect(snapshotUsagePercent(claude, '5h', now)).toBe(35)
    expect(snapshotUsagePercent(claude, '7d', now)).toBe(0)
    expect(snapshotUsagePercent(claude, '7d', now + 101000)).toBeNull()
  })

  it('倍率使用有效快照和上游时区，过期后显示缺失', () => {
    const value = account(1, { type: 'apikey', extra: { upstream_billing_probe: {
      status: 'ok', received_at: '2026-11-24T03:00:00Z', fresh_until: '2026-11-24T06:00:00Z',
      data: { billing_scope: 'token', resolved_rate_multiplier: 2, peak_rate_enabled: true, peak_rate_multiplier: 1.5, peak_start: '11:00', peak_end: '13:00', timezone: 'Asia/Shanghai' }
    } } } as unknown as Partial<AccountListItem>)
    expect(snapshotBillingRate(value, Date.parse('2026-11-24T04:00:00Z'))).toBe(3)
    expect(snapshotBillingRate(value, Date.parse('2026-11-24T05:30:00Z'))).toBe(2)
    expect(snapshotBillingRate(value, Date.parse('2026-11-24T06:00:01Z'))).toBeNull()
  })

  it('导入只接受同一管理员的终态归档', () => {
    const archive = createModelTestArchive(42, [job(), job({ id: 'active', status: 'running' })])
    expect(archive.results).toHaveLength(1)
    expect(parseModelTestArchive(JSON.stringify(archive), 42)).toHaveLength(1)
    expect(() => parseModelTestArchive(JSON.stringify(archive), 43)).toThrow('INVALID_ARCHIVE')
    expect(() => parseModelTestArchive(JSON.stringify([job()]), 42)).toThrow('INVALID_ARCHIVE')
    expect(() => parseModelTestArchive(JSON.stringify({ ...archive, results: [job({ status: 'running' })] }), 42)).toThrow('INVALID_ARCHIVE')
  })

  it('大归档按 UTF-8 字节分批，保持记录完整并限制每批请求大小', () => {
    const records = Array.from({ length: 5 }, (_, index) => job({ id: String(index), response_text: '测试回答'.repeat(100) }))
    const batches = splitModelTestImport(records, 3000)
    expect(batches.length).toBeGreaterThan(1)
    expect(batches.flat()).toEqual(records)
    for (const results of batches) expect(new TextEncoder().encode(JSON.stringify({ results })).length).toBeLessThanOrEqual(3000)
  })
})

describe('模型回答隔离预览', () => {
  it('完整 HTML 保留 head 内 CSS 动画，CSP 先于样式生效', () => {
    const html = buildModelTestPreview('<!doctype html><html><head><style>@keyframes wheel { to { transform: rotate(360deg) } } .wheel { animation: wheel 1s linear infinite }</style></head><body><svg viewBox="0 0 100 100"><circle class="wheel" cx="50" cy="50" r="20" /></svg></body></html>')
    expect(html).toContain('@keyframes wheel')
    expect(html).toContain('animation: wheel')
    expect(html!.indexOf('Content-Security-Policy')).toBeLessThan(html!.indexOf('@keyframes'))
    expect(html).toContain(MODEL_TEST_PREVIEW_CSP)
  })

  it('删除脚本、导航、外部资源、事件处理、foreignObject 与危险 SMIL 目标', () => {
    const html = buildModelTestPreview('<meta http-equiv="refresh" content="0;url=https://evil.test"><script>alert(1)</script><form action="https://evil.test"><input></form><svg onload="alert(1)"><foreignObject><iframe src="https://evil.test"></iframe></foreignObject><image href="https://evil.test/img"/><a href="https://evil.test"><circle r="10" /></a><use href="https://evil.test/a.svg#x"/><animate attributeName="href" values="https://evil.test"/><circle r="10"/></svg>')
    expect(html).not.toMatch(/<script|<form|<input|<iframe|<foreignObject|<image|<a\s|onload=|http-equiv="refresh"/i)
    expect(html).not.toContain('https://evil.test')
    expect(html).not.toContain('attributeName="href"')
  })

  it('保留本地 SVG 引用和安全动画，外部 CSS 请求由 CSP 阻断', () => {
    const html = buildModelTestPreview('<svg><defs><path id="wheel" d="M0 0L10 10"/></defs><use href="#wheel"/><circle r="10"><animate attributeName="r" from="1" to="10" dur="2s" repeatCount="indefinite"/></circle></svg>')
    expect(html).toContain('href="#wheel"')
    expect(html).toContain('<animate')
    expect(html).toContain("default-src 'none'")
    expect(html).toContain("form-action 'none'")
  })

  it.each(['普通文字', '<svg></svg>', '<html><body>无法绘制</body></html>'])('无有效图形时保留源码入口，不创建预览：%s', text => {
    expect(buildModelTestPreview(text)).toBeNull()
  })
})
