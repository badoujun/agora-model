import type {
  DataLocationsPayload,
  DraftFetchInput,
  GatewayKeyDTO,
  GatewayKeyInput,
  GatewayKeyListPayload,
  GatewayKeyPlainPayload,
  HealthInfo,
  LogItem,
  ModelItem,
  PresetListPayload,
  ProviderDTO,
  ProviderExportPayload,
  ProviderInput,
  SessionInfo,
  SettingsPayload,
  TestResult,
  UsagePayload,
  WebDAVConfig,
  WebDAVStatus,
  WebDAVSyncResult,
  WebDAVTestResult,
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
  /** 只测试某一个已保存的凭证（含已停用的 Key） */
  testProviderKey: (id: string, keyId: string) =>
    request<TestResult>(
      `/api/providers/${encodeURIComponent(id)}/keys/${encodeURIComponent(keyId)}/test`,
      { method: 'POST' },
    ),
  /** 草稿（尚未保存）凭证的连接测试 */
  testDraftKey: (body: DraftFetchInput) =>
    request<TestResult>('/api/providers/test-key', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
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
    request<GatewayKeyPlainPayload>('/api/gateway-key/reset', {
      method: 'POST',
    }),

  // 多网关 API Key：列表（含已删除）/ 新建 / 更新 / 软删除 / 查看明文 / 重置明文
  listGatewayKeys: (params: { includeDeleted?: boolean } = {}) =>
    request<GatewayKeyListPayload>(
      `/api/gateway-keys${params.includeDeleted ? '?include_deleted=1' : ''}`,
    ),
  createGatewayKey: (body: GatewayKeyInput) =>
    request<GatewayKeyPlainPayload>('/api/gateway-keys', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  updateGatewayKey: (id: string, body: GatewayKeyInput) =>
    request<{ key: GatewayKeyDTO }>(`/api/gateway-keys/${encodeURIComponent(id)}`, {
      method: 'PUT',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  deleteGatewayKey: (id: string) =>
    request<{ ok: boolean; deleted: boolean }>(`/api/gateway-keys/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    }),
  /** 查看某个网关 Key 的明文（旧记录只存哈希时返回 409） */
  revealGatewayKey: (id: string) =>
    request<GatewayKeyPlainPayload>(`/api/gateway-keys/${encodeURIComponent(id)}/reveal`),
  /** 只换明文，保留别名、备注、状态与模型范围 */
  resetGatewayKeyById: (id: string) =>
    request<GatewayKeyPlainPayload>(`/api/gateway-keys/${encodeURIComponent(id)}/reset`, {
      method: 'POST',
    }),

  // 数据位置
  getDataLocations: () => request<DataLocationsPayload>('/api/data-locations'),

  // 预设供应商（国内常用：DeepSeek / MiniMax / SCNet / Agnes）
  listProviderPresets: () => request<PresetListPayload>('/api/provider-presets'),
  // 注意：从预设创建不在后端独立端点上做；前端在「新增供应商」对话框里调用 listProviderPresets
  // 拿到列表，点预设后只把 name/官网/Base URL 预填到表单，仍走 POST /api/providers。
  // 这样能复用原有校验（SSRF、名称冲突、加密）与「拉取模型」交互。

  // WebDAV 同步
  getWebDAVConfig: () => request<WebDAVConfig>('/api/settings/webdav'),
  updateWebDAVConfig: (body: Partial<WebDAVConfig>) =>
    request<WebDAVConfig>('/api/settings/webdav', {
      method: 'PUT',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  webdavStatus: () => request<WebDAVStatus>('/api/sync/webdav/status'),
  webdavTest: () => request<WebDAVTestResult>('/api/sync/webdav/test'),
  webdavPush: (body: { force?: boolean } = {}) =>
    request<WebDAVSyncResult>('/api/sync/webdav/push', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),
  webdavPull: (body: { force?: boolean } = {}) =>
    request<WebDAVSyncResult>('/api/sync/webdav/pull', {
      method: 'POST',
      headers: JSON_HEADERS,
      body: JSON.stringify(body),
    }),

  // 日志
  listLogs: (params: {
    failed?: string
    model?: string
    provider_id?: string
    gateway_key_id?: string
    status_min?: number
    limit?: number
    offset?: number
  }) => request<{ items: LogItem[]; total: number; limit: number; offset: number }>(`/api/logs${query(params)}`),

  // 使用统计（前端「使用统计」页面读取）
  usage: (params: {
    granularity?: 'daily' | 'weekly' | 'monthly'
    from?: string
    to?: string
    /** 留空 = 统计全部网关 Key */
    gateway_key_id?: string
  } = {}) => request<UsagePayload>(`/api/usage${query(params)}`),
}
