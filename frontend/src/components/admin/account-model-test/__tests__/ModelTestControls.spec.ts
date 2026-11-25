import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import ModelTestControls from '../ModelTestControls.vue'
import ModelTestResult from '../ModelTestResult.vue'
import { CANDY_PROMPT } from '@/utils/accountModelTest'
import type { ModelTestJob } from '@/api/admin/accountModelTest'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh-CN' } }) }))

describe('账号模型测试界面', () => {
  it('修改预设题目后切换为自定义，不修改传入对象', async () => {
    const draft = { model_id: '6a', prompt: CANDY_PROMPT, prompt_key: 'candy' as const, reasoning_effort: '' as const }
    const wrapper = mount(ModelTestControls, { props: { modelValue: draft, models: ['6a'] } })
    await wrapper.get('textarea').setValue('我的新题目')
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toEqual({ ...draft, prompt_key: 'custom', prompt: '我的新题目' })
    expect(draft.prompt).toBe(CANDY_PROMPT)
    expect(wrapper.find('option[value="6a"]').exists()).toBe(true)
  })

  it('预览 iframe 使用空 sandbox，源码以文本显示', async () => {
    const source = '<script>alert(1)</script><svg><circle r="10"/></svg>'
    const job = { id: 'x', status: 'success', prompt_key: 'pelican', response_text: source, created_at: '2026-11-24T03:00:00Z', latency_ms: 12, model_id: '6a', prompt: '画图', persisted: true } as ModelTestJob
    const wrapper = mount(ModelTestResult, { props: { job, now: Date.now() } })
    expect(wrapper.get('iframe').attributes('sandbox')).toBe('')
    expect(wrapper.get('iframe').attributes('srcdoc')).not.toContain('<script>')
    await wrapper.get('button').trigger('click')
    expect(wrapper.find('iframe').exists()).toBe(false)
    expect(wrapper.get('pre').text()).toBe(source)
    expect(wrapper.find('script').exists()).toBe(false)
  })
})
