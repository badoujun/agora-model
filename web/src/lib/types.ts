// 与后端 internal/api 的 DTO 一一对应（字段名不可随意改动）

/** 供应商的单个 API Key（永不回传明文）。 */
export interface ProviderKeyDTO {
  id: string
  label: string
  api_key_hint: string
  enabled: boolean
  /** 是否为网关因凭证类失败自动停用 */
  auto_disabled: boolean
  /** 自动停用原因（仅 auto_disabled 为真时有值） */
  disabled_reason?: string
  created_at?: string
  updated_at?: string
}

/** 单个 Key 的运行期统计（易变，导出文件里不含）。 */
export interface KeyStatsDTO {
  last_used_at?: string
  failure_count: number
  last_failure_at?: string
  last_error?: string
  today_tokens: number
  week7_tokens: number
  month30_tokens: number
  month30_requests: number
}

export interface ProviderDTO {
  id: string
  name: string
  openai_base_url: string
  openai_endpoint_override: string
  /** 供应商官网地址（仅展示/跳转用，可留空） */
  website_url: string
  /** 兼容字段：首个启用 Key 的掩码 */
  api_key_hint: string
  /** 该供应商的全部凭证（顺序即轮询顺序） */
  keys: ProviderKeyDTO[]
  /** key_id -> 运行期统计 */
  key_stats?: Record<string, KeyStatsDTO>
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

/** 创建/更新请求里的单个凭证；id 为空表示新增（必须给明文）。 */
export interface ProviderKeyInput {
  id?: string
  label?: string
  /** 编辑既有 Key 时留空表示沿用原密文 */
  api_key?: string
  enabled?: boolean
}

export interface ProviderInput {
  id?: string
  name: string
  openai_base_url: string
  openai_endpoint_override?: string
  /** 供应商官网地址；编辑时传空串表示清空，不传表示沿用原值 */
  website_url?: string
  /** 兼容字段：单凭证的旧写法；新前端一律走 keys */
  api_key?: string
  /** 凭证集合；不传表示不改动，传空数组/完整列表表示全量替换 */
  keys?: ProviderKeyInput[]
  models_selected?: string[]
  model_aliases?: Record<string, string>
  timeout_seconds?: number
  extra_headers?: Record<string, string>
  extra_body?: Record<string, unknown>
  allow_internal?: boolean
  enabled?: boolean
}

/** 草稿（尚未保存）供应商的单个凭证 */
export interface DraftKeyInput {
  label?: string
  api_key: string
  enabled?: boolean
}

/** 尚未保存的供应商（草稿）拉取模型候选 / 测试凭证的请求体 */
export interface DraftFetchInput {
  openai_base_url: string
  openai_endpoint_override?: string
  /** 兼容字段：单 Key */
  api_key?: string
  keys?: DraftKeyInput[]
  extra_headers?: Record<string, string>
  timeout_seconds?: number
  allow_internal?: boolean
  models_selected?: string[]
}

/** /api/export 的响应：providers[].api_key 是明文（换机迁移用） */
export interface ProviderExportPayload {
  format: string
  exported_at: string
  gateway: unknown
  providers: Array<ProviderDTO & { api_key: string }>
  notes?: Record<string, string>
}

/** 单个 Key 的连接测试结果 */
export interface KeyTestResult {
  key_id: string
  label?: string
  hint?: string
  ok: boolean
  status_code?: number
  latency_ms: number
  message: string
  model_count?: number
}

export interface TestResult {
  ok: boolean
  status_code: number
  latency_ms: number
  message: string
  model_count?: number
  checked_at: string
  /** 实际尝试过的 Key（多 Key 时逐个尝试，首个成功即停止） */
  keys?: KeyTestResult[]
}

export interface ModelItem {
  /** 对外模型标识：供应商名/对外模型名（配置了别名时是 供应商名/别名） */
  model: string
  /** 转发给上游时使用的真实模型名 */
  upstream_model: string
  alias?: string
  provider_id: string
  provider_name: string
}

export interface LogItem {
  ts: string
  request_id: string
  inbound_protocol: string
  model: string
  provider_id: string
  /** 实际服务该请求的供应商 Key 掩码 */
  key_hint?: string
  /** 发起该请求的网关 API Key id（空表示静态 Key 或历史无归属数据） */
  gateway_key_id?: string
  /** 网关 API Key 的展示名（别名，附备注）；查询时由服务端回填 */
  gateway_key_name?: string
  upstream_url: string
  status_code: number
  latency_ms: number
  first_byte_ms: number
  stream: boolean
  error_msg?: string
  client_ip: string
}

/* -------------------------- 网关 API Key -------------------------- */

/** 网关 Key 可调用的一个「供应商 × 上游模型」。 */
export interface GatewayKeyScope {
  provider_id: string
  /** 上游模型名（非对外别名）；对外展示时需映射回 alias */
  model: string
}

/** 某个网关 Key 的全部历史累计 token 用量。 */
export interface GatewayKeyUsageDTO {
  /** 输入 + 输出 */
  total_tokens: number
  /** 输入中命中缓存的部分 */
  input_cached: number
  /** 输入中未命中缓存的部分（= 输入总量 - 命中缓存） */
  input_uncached: number
  output: number
}

export interface GatewayKeyDTO {
  id: string
  name: string
  note?: string
  key_hint: string
  enabled: boolean
  /** 是否已软删除（记录保留以便历史日志/统计显示名称） */
  deleted: boolean
  deleted_at?: string
  created_at?: string
  updated_at?: string
  last_used_at?: string
  /** 明文是否可还原；false 表示只能重置 */
  revealable: boolean
  /** 模型范围；空数组表示不限制（可调用全部模型） */
  scopes: GatewayKeyScope[]
  /** 累计用量；没有产生过请求时各项为 0 */
  usage?: GatewayKeyUsageDTO
}

/** 新建 / 更新网关 Key 的请求体；更新时字段可省略表示沿用现值。 */
export interface GatewayKeyInput {
  name?: string
  note?: string
  enabled?: boolean
  /** 传数组表示全量替换模型范围，传空数组表示改为不限制 */
  scopes?: GatewayKeyScope[]
}

export interface GatewayKeyListPayload {
  items: GatewayKeyDTO[]
}

/** 新建 / 重置网关 Key 的响应：gateway_key 是明文，仅本次返回。 */
export interface GatewayKeyPlainPayload {
  gateway_key: string
  key_hint: string
  warning?: string
  key?: GatewayKeyDTO
}

export interface SettingsPayload {
  listen: string
  port: number
  sse_idle_seconds: number
  max_body_bytes: number
  /** 默认网关 Key（最早创建的启用 Key）的 id；空表示尚未生成 */
  gateway_key_id?: string
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

/** 国内常用供应商预设（DeepSeek / MiniMax / SCNet / Agnes 等）。 */
export interface PresetDTO {
  id: string
  name: string
  openai_base_url: string
  website_url: string
}

export interface PresetListPayload {
  items: PresetDTO[]
  /** 当前生效的预设来源（"内置" 或用户覆盖文件绝对路径）。 */
  source: string
  /** 编译期内置预设的 SHA256 前 8 字节，便于和覆盖文件对比。 */
  builtin_fingerprint: string
}

/** WebDAV 配置（GET/PUT 往返）。password 字段写入时必填，读取时永远为空。 */
export interface WebDAVConfig {
  configured: boolean
  url: string
  username: string
  password: string
  has_password: boolean
  last_synced_at?: string
}

export interface WebDAVStatus {
  configured: boolean
  url?: string
  username?: string
  has_remote: boolean
  local_fingerprint: string
  remote_fingerprint?: string
  match: boolean
  last_synced_at?: string
}

export interface WebDAVSyncResult {
  ok: boolean
  direction: 'push' | 'pull'
  imported?: number
  bytes?: number
  fingerprint: string
  synced_at: string
}

/** GET /api/sync/webdav/test：连通性 + 凭证 + 远端可达 */
export interface WebDAVTestResult {
  ok: boolean
  url: string
  username: string
  has_remote: boolean
  remote_fingerprint?: string
  remote_bytes?: number
  error?: string
}

/* -------------------------- 使用统计 -------------------------- */

export interface UsageKPI {
  /** 今日 token 总数 */
  today_total: number
  /** 最近 7 天 token 总数 */
  week7_total: number
  /** 最近 30 天 token 总数 */
  month30_total: number
}

export interface UsageModelShare {
  name: string
  provider: string
  total: number
  percent: number
  input_cached: number
  input_uncached: number
  output: number
  requests: number
}

export interface UsageProviderShare {
  id: string
  name: string
  total: number
  percent: number
  model_count: number
}

export interface UsageDailyRow {
  /** YYYY-MM-DD */
  date: string
  total: number
  input_cached: number
  input_uncached: number
  output: number
  requests: number
  /** 当天各（供应商, 模型）的用量明细；补齐出来的空日子缺省。 */
  breakdown?: UsageBreakdownItem[]
}

/** 某一天内一个供应商/模型的用量明细，用于悬浮弹窗展开。 */
export interface UsageBreakdownItem {
  provider: string
  model: string
  input_cached: number
  input_uncached: number
  output: number
}

export interface UsageHeatmapCell {
  /** YYYY-MM-DD */
  date: string
  total: number
}

export interface UsageTrendPoint {
  /** YYYY-MM-DD */
  date: string
  total: number
}

export type UsageGranularity = 'daily' | 'weekly' | 'monthly'

export interface UsagePayload {
  first_used_at: string
  active_days: number
  granularity: UsageGranularity
  /** 本次统计作用的网关 Key；空串表示「全部」 */
  gateway_key_id: string
  kpi: UsageKPI
  models: UsageModelShare[]
  providers: UsageProviderShare[]
  daily: UsageDailyRow[]
  heatmap: UsageHeatmapCell[]
  trend: UsageTrendPoint[]
}
