import type {
  DataLocationsPayload,
  DraftFetchInput,
  HealthInfo,
  LogItem,
  ModelItem,
  ProviderDTO,
  ProviderExportPayload,
  ProviderInput,
  SessionInfo,
  SettingsPayload,
  TestResult,
} from './types'

const JSON_HEADERS = { 'Content-Type': 'application/json' }

/** ApiError 携带 HTTP 状态码，便于调用方区分 401/400。 */
export class ApiError extends Error {
  readonly status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, { credentials: 'same-origin', ...init })
  const text = await resp.text()

  let payload: unknown
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = text
    }
  }

  if (!resp.ok) {
    let message = `HTTP ${resp.status}`
    if (payload && typeof payload === 'object' && 'error' in payload) {
      message = String((payload as { error: unknown }).error)
    } else if (typeof payload === 'string' && payload) {
      message = payload
    }
    throw new ApiError(resp.status, message)
  }
  return payload as T
}

function query(params: Record<string, string | number | boolean | undefined>): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === '' || value === false) continue
    search.set(key, String(value))
  }
  const raw = search.toString()
  return raw ? `?${raw}` : ''
}

export const api = {
  // 会话
  session: () => request<SessionInfo>('/api/auth/session'),
  login: (password: string) =>
    request<{ ok: boolean }>('/api/auth/login', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify({ password }),
    }),
  logout: () => request<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  health: () => request<HealthInfo>('/api/health'),

  // 供应商
  listProviders: () => request<{ items: ProviderDTO[] }>('/api/providers'),
  createProvider: (body: ProviderInput) =>
    request<ProviderDTO>('/api/providers', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  updateProvider: (id: string, body: ProviderInput) =>
    request<ProviderDTO>(`/api/providers/${encodeURIComponent(id)}`, {
      method: 'PUT',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  deleteProvider: (id: string) =>
    request<{ ok: boolean }>(`/api/providers/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  testProvider: (id: string) =>
    request<TestResult>(`/api/providers/${encodeURIComponent(id)}/test`, { method: 'POST' }),
  /** 拉取上游模型候选列表（勾选在编辑页完成） */
  fetchModels: (id: string) =>
    request<ProviderDTO>(`/api/providers/${encodeURIComponent(id)}/fetch-models`, { method: 'POST' }),
  /** 供应商尚未保存时，用表单里填写的信息直接拉取候选模型（不落库） */
  fetchDraftModels: (body: DraftFetchInput) =>
    request<{ candidate_models: string[]; fetched_at: string }>('/api/providers/fetch-models', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),

  // 供应商导入导出（导出含 API Key 明文）
  exportProviders: () => request<ProviderExportPayload>('/api/export'),
  importProviders: (payload: { providers: ProviderInput[] }) =>
    request<{ imported: number }>('/api/import', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(payload),
    }),

  // 模型
  listModels: () => request<{ items: ModelItem[] }>('/api/models'),

  // 设置与网关 Key
  getSettings: () => request<SettingsPayload>('/api/settings'),
  updateSettings: (body: { log_success?: boolean }) =>
    request<SettingsPayload>('/api/settings', {
      method: 'PUT',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  resetGatewayKey: () =>
    request<{ gateway_key: string; key_hint: string; warning: string }>('/api/gateway-key/reset', {
      method: 'POST',
    }),

  // 数据位置
  getDataLocations: () => request<DataLocationsPayload>('/api/data-locations'),

  // 日志
  listLogs: (params: {
    failed?: string
    model?: string
    provider_id?: string
    status_min?: number
    limit?: number
    offset?: number
  }) => request<{ items: LogItem[]; total: number; limit: number; offset: number }>(`/api/logs${query(params)}`),
}
