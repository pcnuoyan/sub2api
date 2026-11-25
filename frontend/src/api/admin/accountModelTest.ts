import { apiClient } from '../client'

export type ModelTestStatus = 'queued' | 'running' | 'success' | 'failed' | 'cancelled' | 'interrupted'
export type ModelTestPromptKey = 'candy' | 'pelican' | 'custom'
export type ModelTestReasoning = '' | 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max'

export interface ModelTestRequest {
  account_id: number
  model_id: string
  prompt: string
  prompt_key: ModelTestPromptKey
  reasoning_effort: ModelTestReasoning
}

export interface ModelTestJob extends ModelTestRequest {
  id: string
  account_name: string
  platform: string
  status: ModelTestStatus
  response_text: string
  error_message: string
  latency_ms: number
  created_at: string
  started_at: string | null
  finished_at: string | null
  persisted: boolean
}

export interface ModelTestResults {
  results: ModelTestJob[]
  cleared_at: string | null
}

const jobsPath = '/admin/account-model-test-jobs'
const resultsPath = '/admin/account-model-test-results'

export const accountModelTestAPI = {
  async listJobs(signal?: AbortSignal): Promise<ModelTestJob[]> {
    const { data } = await apiClient.get<{ jobs: ModelTestJob[] }>(jobsPath, { signal })
    return data.jobs ?? []
  },
  async createJob(request: ModelTestRequest): Promise<ModelTestJob> {
    const { data } = await apiClient.post<ModelTestJob>(jobsPath, request)
    return data
  },
  async cancelJob(id: string): Promise<ModelTestJob> {
    const { data } = await apiClient.delete<ModelTestJob>(`${jobsPath}/${encodeURIComponent(id)}`)
    return data
  },
  async getResults(signal?: AbortSignal): Promise<ModelTestResults> {
    const { data } = await apiClient.get<ModelTestResults>(resultsPath, { signal })
    return { results: data.results ?? [], cleared_at: data.cleared_at ?? null }
  },
  async importResults(results: ModelTestJob[]): Promise<ModelTestResults> {
    const { data } = await apiClient.post<ModelTestResults>(resultsPath, { results })
    return data
  },
  async clearResults(): Promise<{ cleared_at: string }> {
    const { data } = await apiClient.delete<{ cleared_at: string }>(resultsPath)
    return data
  }
}
