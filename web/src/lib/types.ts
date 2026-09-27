// 与后端 internal/api 的 DTO 一一对应（字段名不可随意改动）

export interface ProviderDTO {
  id: string
  name: string
  openai_base_url: string
  openai_endpoint_override: string
  /** 供应商官网地址（仅展示/跳转用，可留空） */
  website_url: string
  api_key_hint: string
  /** 已勾选启用的上游模型名（顺序即展示顺序） */
  models_selected: string[]
  /** 上游模型名 -> 对外别名 */
  model_aliases: Record<string, string>
  /** 对外暴露的模型名（配置了别名时用别名） */
  exposed_models: string[]
  timeout_seconds: number
  extra_headers?: Record<string, string>
  extra_body?: Record<string, unknown>
  allow_internal: boolean
  enabled: boolean
  model_count: number
  /** 最近一次「拉取模型」的候选结果 */
  candidate_models?: string[]
  last_fetch_at?: string
  last_fetch_error?: string
}

export interface ProviderInput {
  id?: string
  name: string
  openai_base_url: string
  openai_endpoint_override?: string
  /** 供应商官网地址；编辑时传空串表示清空，不传表示沿用原值 */
  website_url?: string
  /** 编辑时留空表示不修改凭证 */
  api_key?: string
  models_selected?: string[]
  model_aliases?: Record<string, string>
  timeout_seconds?: number
  extra_headers?: Record<string, string>
  extra_body?: Record<string, unknown>
  allow_internal?: boolean
  enabled?: boolean
}

/** 尚未保存的供应商（草稿）拉取模型候选的请求体 */
export interface DraftFetchInput {
  openai_base_url: string
  openai_endpoint_override?: string
  api_key: string
  extra_headers?: Record<string, string>
  timeout_seconds?: number
  allow_internal?: boolean
}

/** /api/export 的响应：providers[].api_key 是明文（换机迁移用） */
export interface ProviderExportPayload {
  format: string
  exported_at: string
  gateway: unknown
  providers: Array<ProviderDTO & { api_key: string }>
  notes?: Record<string, string>
}

export interface TestResult {
  ok: boolean
  status_code: number
  latency_ms: number
  message: string
  model_count?: number
  checked_at: string
}

export interface ModelItem {
  /** 对外模型名（配置了别名时是别名） */
  model: string
  /** 转发给上游时使用的真实模型名 */
  upstream_model: string
  alias?: string
  provider_id: string
  provider_name: string
  default: boolean
}

export interface LogItem {
  ts: string
  request_id: string
  inbound_protocol: string
  model: string
  provider_id: string
  upstream_url: string
  status_code: number
  latency_ms: number
  first_byte_ms: number
  stream: boolean
  error_msg?: string
  client_ip: string
}

export interface SettingsPayload {
  listen: string
  port: number
  sse_idle_seconds: number
  max_body_bytes: number
  gateway_key_hint: string
  /** 当前网关 Key 明文（可随时查看/复制） */
  gateway_key: string
  /** 明文是否可还原：false 表示旧版本只存了哈希，或主密钥与数据库不匹配，只能重置 */
  gateway_key_revealable: boolean
  gateway_key_created_at?: string
  gateway_key_last_used?: string
  log_success?: string
}

/** 「数据位置」页面：本程序在磁盘上的落点 */
export interface DataPathItem {
  key: string
  kind: 'dir' | 'file' | 'stdout'
  label: string
  note: string
  path: string
  exists: boolean
  size_bytes: number
  size_label?: string
}

export interface DataLocationsPayload {
  os: string
  os_label: string
  version: string
  data_dir: string
  executable: string
  items: DataPathItem[]
}

export interface SessionInfo {
  version: string
  login_required: boolean
  authenticated: boolean
  local_only: boolean
}

export interface HealthInfo {
  status: string
  version: string
  providers: number
  models: number
  logs: number
  logs_written: number
  logs_dropped: number
  login_required: boolean
}
