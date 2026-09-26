// 与后端 internal/api 的 DTO 一一对应（字段名不可随意改动）

export interface ProviderDTO {
  id: string
  name: string
  openai_base_url: string
  anthropic_base_url: string
  openai_endpoint_override: string
  anthropic_endpoint_override: string
  api_key_hint: string
  models_manual: string[]
  models_excluded: string[]
  auto_fetch_models: boolean
  priority: number
  timeout_seconds: number
  extra_headers?: Record<string, string>
  extra_body?: Record<string, unknown>
  allow_internal: boolean
  enabled: boolean
  model_count: number
  available_models: string[]
  last_fetch_at?: string
  last_fetch_error?: string
}

export interface ProviderInput {
  id?: string
  name: string
  openai_base_url: string
  anthropic_base_url: string
  openai_endpoint_override?: string
  anthropic_endpoint_override?: string
  /** 编辑时留空表示不修改凭证 */
  api_key?: string
  models_manual?: string[]
  models_excluded?: string[]
  auto_fetch_models?: boolean
  priority?: number
  timeout_seconds?: number
  extra_headers?: Record<string, string>
  extra_body?: Record<string, unknown>
  allow_internal?: boolean
  enabled?: boolean
}

export interface ProbeResult {
  ok: boolean
  status_code: number
  latency_ms: number
  message: string
  model_count?: number
}

export interface TestResult {
  ok: boolean
  openai: ProbeResult
  anthropic: ProbeResult
  checked_at: string
}

export interface ModelItem {
  model: string
  provider_id: string
  default: boolean
  source: string
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
  gateway_key_created_at?: string
  gateway_key_last_used?: string
  model_refresh_seconds?: string
  log_success?: string
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
