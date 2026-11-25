import type { ModelTestJob } from '@/api/admin/accountModelTest'
import { compareModelTestTime, isActiveModelTest, MODEL_TEST_REASONING } from './accountModelTest'

export const MODEL_TEST_ARCHIVE_MAX_BYTES = 128 * 1024 * 1024

export interface ModelTestArchive {
  version: 1
  admin_id: number
  exported_at: string
  results: ModelTestJob[]
}

export function modelTestLegacyKey(adminId: number): string {
  return `sub2api:account-model-test-results:${adminId}`
}

export function parseModelTestArchive(source: string, adminId: number): ModelTestJob[] {
  const archive: unknown = JSON.parse(source)
  if (!archive || typeof archive !== 'object') throw new Error('INVALID_ARCHIVE')
  const data = archive as Partial<ModelTestArchive>
  if (data.version !== 1 || data.admin_id !== adminId || !Array.isArray(data.results) || data.results.length > 500) throw new Error('INVALID_ARCHIVE')
  for (const result of data.results) {
    if (!result || typeof result !== 'object' || !Number.isSafeInteger(result.account_id) || result.account_id <= 0 ||
      !['success', 'failed', 'cancelled', 'interrupted'].includes(result.status) ||
      !['candy', 'pelican', 'custom'].includes(result.prompt_key) ||
      !MODEL_TEST_REASONING.includes(result.reasoning_effort) ||
      !['id', 'account_name', 'platform', 'model_id', 'prompt', 'response_text', 'error_message', 'created_at'].every(key => typeof result[key as keyof ModelTestJob] === 'string') ||
      !Number.isFinite(Date.parse(result.created_at)) ||
      typeof result.latency_ms !== 'number' || !Number.isFinite(result.latency_ms) || result.latency_ms < 0) throw new Error('INVALID_ARCHIVE')
  }
  return data.results
}

export function createModelTestArchive(adminId: number, results: Iterable<ModelTestJob>): ModelTestArchive {
  return {
    version: 1,
    admin_id: adminId,
    exported_at: new Date().toISOString(),
    results: [...results].filter(job => !isActiveModelTest(job)).sort((a, b) => compareModelTestTime(b.created_at, a.created_at)).slice(0, 500)
  }
}

export function splitModelTestImport(results: ModelTestJob[], maxBytes = 8 * 1024 * 1024): ModelTestJob[][] {
  const batches: ModelTestJob[][] = []
  let batch: ModelTestJob[] = []
  let bytes = 14
  for (const result of results) {
    const size = new TextEncoder().encode(JSON.stringify(result)).length + 1
    if (size + 14 > maxBytes) throw new Error('INVALID_ARCHIVE')
    if (bytes + size > maxBytes) {
      batches.push(batch)
      batch = []
      bytes = 14
    }
    batch.push(result)
    bytes += size
  }
  if (batch.length) batches.push(batch)
  return batches
}
