# AgoraModel 功能明细设计说明书

> 对应 PRD：`docs/PRD.md` v1.0
> 本文档面向实现，描述架构、数据模型、接口契约、核心流程、前端页面与测试/部署方案。
> 版本：v1.0（初稿）

---

## 1. 设计目标与边界

**设计目标**：以最小实现代价交付「一个对外入口 + 双协议原样透传 + 零模板动态供应商 + 模型聚合 + 轻量 Web UI」，并把对抗性审查中确认的工程风险（SSRF、明文凭证、SSE 空闲超时、断连泄漏、配置损坏、黑盒排查）在低成本范围内一次性修掉。

**设计边界（明确的"不做"）**：

- 不做请求/响应/SSE 的协议转换——上游供应商自带双协议 URL，网关只做「按入站协议选上游 URL」。
- 不做凭证池、轮询、故障转移——每个供应商一个凭证。
- 不做用量统计、计费、WebDAV。
- 不做多实例/分布式；不做多租户。
- 不为 `/v1/models` 做 Anthropic 格式内容协商。

---

## 2. 总体架构

### 2.1 架构图

```
┌──────────────────────────────────────────────────────────────┐
│                      Web UI（React SPA）                      │
│  供应商管理 │ 模型列表 │ 网关设置 │ 日志                        │
└───────────────────────────┬──────────────────────────────────┘
                            │ /api/*（REST + 会话认证）
┌───────────────────────────▼──────────────────────────────────┐
│                   AgoraModel 单进程（Go）                      │
│ ┌────────────┐ ┌────────────┐ ┌────────────┐ ┌────────────┐  │
│ │ 供应商管理  │ │ 模型聚合    │ │ 路由引擎    │ │ 日志/审计   │  │
│ │ (双 URL)   │ │ /v1/models │ │ (按模型名)  │ │ (SQLite)   │  │
│ └────────────┘ └─────┬──────┘ └─────┬──────┘ └────────────┘  │
│                ┌─────▼──────────────▼──────┐                  │
│                │      透传代理层           │                  │
│                │ 认证(单网关Key) → 头改写 → │                  │
│                │ 流式转发 + SSE 心跳保活    │                  │
│                └─────────────┬─────────────┘                  │
│  ┌────────────────────────▼─────────────────────────────┐    │
│  │ 存储层 SQLite(WAL)：providers / gateway_keys /        │    │
│  │ settings / model_cache / logs  +  AES-256-GCM 加解密  │    │
│  └──────────────────────────────────────────────────────┘    │
└───────────────────────────┬──────────────────────────────────┘
        /v1/chat/completions │ /v1/messages（原样透传，不转换）
   ┌───────────┬─────────────┼─────────────┬───────────┐
   ▼           ▼             ▼             ▼           ▼
供应商 A     供应商 B       供应商 C      供应商 D     …
OpenAI URL   OpenAI URL    OpenAI URL    OpenAI URL
Anthropic    Anthropic     Anthropic     Anthropic
URL          URL           URL           URL
（每供应商 1 个凭证）
```

### 2.2 分层职责

| 层 | 职责 | 不负责 |
| --- | --- | --- |
| 入口层 | 路由注册、入站认证、body 大小限制、请求 ID 生成 | 业务逻辑 |
| 应用层 | 路由决策、供应商/凭证解析、头改写、日志记录、管理 API | 网络传输细节 |
| 存储层 | SQLite 访问、加解密、配置快照与原子替换 | HTTP |
| 出站层 | `http.Transport` 复用、超时、SSE 读循环 + 心跳、上下文取消 | 格式转换 |

### 2.3 并发模型

- 每个入站请求一个 goroutine，转发不持有全局锁。
- 配置在内存中保存为**不可变快照**（`atomic.Pointer[ConfigSnapshot]`）；写操作在事务提交后整体替换快照，`O(1)` 生效，正在进行的流不受影响。
- 模型聚合由 `time.Ticker` 驱动的后台 goroutine 完成，与请求路径完全解耦；单个供应商失败被隔离，不阻塞其他供应商。
- 出站共享一个 `http.Transport`（连接池复用），每请求超时通过 `context.WithTimeout` 控制，不使用全局 `Client.Timeout`（会误杀长流）。

### 2.4 仓库结构建议

```
agora-model/
├─ cmd/agoramodel/main.go          # 入口：flag/env 解析、启动、优雅关闭
├─ internal/
│  ├─ config/                      # 启动配置 + 快照管理
│  ├─ store/                       # SQLite、迁移、DAO
│  ├─ crypto/                      # AES-256-GCM、主密钥加载、掩码
│  ├─ security/                    # SSRF URL 校验
│  ├─ platform/                    # 跨平台隔离：数据目录、主密钥来源、lock 文件、信号、服务化
│  ├─ provider/                    # 供应商模型、service、连接测试
│  ├─ models/                      # 模型聚合、缓存、/v1/models 组装
│  ├─ route/                       # 模型名路由（含 provider/model 命名空间）
│  ├─ gateway/                     # 透传代理：头改写、SSE 读循环、心跳
│  ├─ logging/                     # 请求日志与进程日志
│  └─ api/                         # 管理 REST API + 会话认证
├─ web/                            # React + Vite 前端（构建产物 embed）
├─ docs/
│  ├─ PRD.md
│  ├─ DESIGN.md
│  └─ TODO.md
└─ Makefile / Taskfile / build.ps1
```

---

## 3. 技术栈与决策记录（ADR）

| 项 | 选型 | 理由 |
| --- | --- | --- |
| 后端语言 | **Go 1.22+** | 高并发、单二进制、GC 可控；`httputil.ReverseProxy` / `http.ResponseController` 让透传与流式控制成本极低 |
| HTTP 框架 | **Gin**（或 `chi`） | 生态成熟，路由与中间件简单 |
| 存储 | **SQLite（WAL）** | 事务与原子性天然解决「文件覆盖写损坏」问题；单机足够 |
| SQLite 驱动 | **modernc.org/sqlite**（纯 Go） | 无 CGO，跨平台交叉编译简单 |
| 前端 | **React 19 + Vite + Tailwind CSS + shadcn/ui** | 开发效率高，组件齐备 |
| 前后端集成 | `embed.FS` 内嵌 `web/dist` | 单文件交付，零安装体验 |
| 实时性 | 直接透传 SSE（无需 WebSocket） | 与 LLM 流式语义天然对齐 |
| 加密 | `crypto/aes` + GCM（标准库） | 无第三方依赖 |

### ADR-001：后端语言与跨平台基线（已定案）

- **决定**：**Go 1.25+**（本机实测 go1.27.0），并把「Windows / Linux / macOS 三平台、单文件交付」作为硬约束。
- **理由**：透传网关的核心是「读 → 写 → Flush」循环，Go 标准库开箱即用；开发效率高；`CGO_ENABLED=0` 时静态链接、可直接交叉编译，从任一开发机用一条命令产出两端产物。
- **备选**（仅作记录）：Rust + Axum + Tokio（性能上限更高，但交叉编译需 `cargo-zigbuild` / `xwin` / musl target，构建链复杂度显著上升）；`ncruces/go-sqlite3`（WASM + wazero，modernc 的替代驱动，调试更直观、部分场景更快）。
- **跨平台基线（三条硬约束）**：
  1. **全量 `CGO_ENABLED=0`** —— 不引入任何 CGO 依赖（SQLite 驱动、无 C 后端的密码库等）；
  2. SQLite 使用 **`modernc.org/sqlite`**（SQLite C 源码的纯 Go 转译版，当前锁定 **v1.59.0**），`sql.Open` 驱动名为 `"sqlite"`；**`modernc.org/libc` 版本必须与驱动 `go.mod` 声明一致，不得单独升级**（不一致会编译失败）；
  3. 前端产物 `web/dist` 由 `embed.FS` 内嵌，平台无关。
- **目标矩阵**：`windows/amd64`、`windows/arm64`、`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`（驱动另支持 `windows/386`、`linux/{386,arm,loong64,ppc64le,riscv64,s390x}`、`darwin/{amd64,arm64}`、`freebsd/*`，有余力可一并产出）。
- **构建命令**（任一开发机即可完成全部产物）：

  ```bash
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/agoramodel-windows-amd64.exe ./cmd/agoramodel
  CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/agoramodel-windows-arm64.exe ./cmd/agoramodel
  CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/agoramodel-linux-amd64       ./cmd/agoramodel
  CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/agoramodel-linux-arm64       ./cmd/agoramodel
  CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/agoramodel-darwin-amd64      ./cmd/agoramodel
  CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/agoramodel-darwin-arm64      ./cmd/agoramodel
  ```

- **已知代价**：modernc 的**写入**吞吐约为 CGO 驱动的 1/3（读与并发读通常持平甚至更好）。本项目的日志为「单写协程 + 批量 flush」，写入频率低，不构成瓶颈；若未来出现写瓶颈，可按 PocketBase 模式用 `//go:build cgo` 做本地开发回退，但**发布产物一律走纯 Go 路径**。
- **环境前置（Phase 0 · T0.1，已完成）**：已安装 **go1.27.0**；`go.mod` 的 `go` 指令为 **1.25.0**（由 `modernc.org/sqlite v1.59.0` 的最低要求决定），三平台构建矩阵已验证通过。
- **平台版本下限（已确认）**：目标平台为 **Windows 10 / Windows 11** 与 Linux。Go 1.21+ 的 Windows 下限恰好是 Windows 10 1607+，因此 Go 1.25+（本项目实际使用 1.27.0）完全覆盖目标范围，无需为旧系统退回 Go 1.20，也不需要任何 Win7/8、Server 2012 兼容分支。32 位 Windows 亦不在目标内，目标架构只保留 `amd64` / `arm64`。

### ADR-002：透传 vs 协议转换

- **决定**：透传。入站协议直接决定上游协议 URL，不做任何字段级转换。
- **理由**：上游均提供双协议 URL；透传零格式风险、零转换开销、TTFT 更低、调试直观（上游返回什么就是什么）。
- **代价**：若某供应商只支持单协议，则该供应商该协议路径不可用（网关返回明确错误，见 §6.5）。

### ADR-003：存储（SQLite vs JSON 文件）

- **决定**：SQLite（WAL）。
- **理由**：事务原子性、并发读友好、可加索引做日志筛选；避免 JSON 覆盖写入被中断导致的配置损坏。
- 若未来坚持文件方案，必须「写临时文件 + `os.Rename` 原子替换」。

### ADR-004：模型寻址（`provider/model` 命名空间）

- **决定**：裸模型名按 `priority` 聚合寻址；`provider/model` 显式寻址；仅在斜杠前缀命中**已存在的 provider id** 时按命名空间解析。
- **理由**：不引入「别名表」这一新概念，配置面零增长；斜杠歧义规则保护了 `meta-llama/Llama-3-70B` 这类含斜杠的真实模型名。

### ADR-005：SSE 保活方式

- **决定**：带读超时的转发循环，空闲时注入 SSE 注释行 `: keep-alive\n\n`。
- **理由**：SSE 注释行被所有合规客户端忽略；只在空闲注入，有数据时字节流完全透明，语义无损。

### ADR-006：错误日志 vs 用量统计

- **决定**：保留「错误/审计日志」，不做「用量统计」。
- **理由**：日志是排查「Agent 发错还是上游拒了」的唯一手段；用量统计是另一回事，用户已明确暂不做。

---

## 4. 数据模型（SQLite）

### 4.1 DDL

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- 供应商
CREATE TABLE IF NOT EXISTS providers (
  id                          TEXT    PRIMARY KEY,           -- uuid/短 id
  name                        TEXT    NOT NULL,
  openai_base_url             TEXT,                          -- 如 https://api.a.com/v1
  anthropic_base_url          TEXT,                          -- 如 https://api.a.com
  openai_endpoint_override    TEXT,                          -- 完整 URL，优先于 base_url 拼接
  anthropic_endpoint_override TEXT,
  api_key_cipher              BLOB    NOT NULL,              -- AES-256-GCM: nonce||ct||tag
  api_key_hint                TEXT    NOT NULL,              -- 仅用于展示：sk-****abcd
  models_manual_json          TEXT    NOT NULL DEFAULT '[]', -- 手动添加的模型
  models_excluded_json        TEXT    NOT NULL DEFAULT '[]', -- 黑名单
  auto_fetch_models           INTEGER NOT NULL DEFAULT 1,
  priority                    INTEGER NOT NULL DEFAULT 100,
  timeout_seconds             INTEGER NOT NULL DEFAULT 120,
  extra_headers_json          TEXT    NOT NULL DEFAULT '{}',
  extra_body_json             TEXT    NOT NULL DEFAULT '{}',
  allow_internal              INTEGER NOT NULL DEFAULT 0,
  enabled                     INTEGER NOT NULL DEFAULT 1,
  created_at                  TEXT    NOT NULL,
  updated_at                  TEXT    NOT NULL
);

-- 网关 Key（v1 只暴露一条，表结构为未来多 Key 预留）
CREATE TABLE IF NOT EXISTS gateway_keys (
  id           TEXT    PRIMARY KEY,
  name         TEXT    NOT NULL DEFAULT 'default',
  key_hash     TEXT    NOT NULL UNIQUE,   -- sha256(gw-key)，比对用，不存明文
  key_hint     TEXT    NOT NULL,          -- gw-****abcd
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_at   TEXT    NOT NULL,
  last_used_at TEXT,
  revoked_at   TEXT
);

-- 聚合模型缓存
CREATE TABLE IF NOT EXISTS model_cache (
  provider_id TEXT NOT NULL,
  model_id    TEXT NOT NULL,
  source      TEXT NOT NULL DEFAULT 'auto',   -- auto | manual
  fetched_at  TEXT NOT NULL,
  PRIMARY KEY (provider_id, model_id)
);

-- 请求日志（失败必记，成功可只计数）
CREATE TABLE IF NOT EXISTS logs (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  ts               TEXT    NOT NULL,
  request_id       TEXT    NOT NULL,
  inbound_protocol TEXT    NOT NULL,          -- openai | anthropic | models
  model            TEXT,
  provider_id      TEXT,
  upstream_url     TEXT,
  status_code      INTEGER NOT NULL,
  latency_ms       INTEGER NOT NULL,
  first_byte_ms    INTEGER,
  stream           INTEGER NOT NULL DEFAULT 0,
  error_msg        TEXT,                      -- 上游错误体前 1KB，已脱敏
  client_ip        TEXT
);

-- 通用设置
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_logs_ts        ON logs(ts DESC);
CREATE INDEX IF NOT EXISTS idx_logs_status    ON logs(status_code);
CREATE INDEX IF NOT EXISTS idx_logs_model     ON logs(model);
CREATE INDEX IF NOT EXISTS idx_cache_model    ON model_cache(model_id);
```

### 4.2 关键约束与规则

| 规则 | 说明 |
| --- | --- |
| 单凭证 | 一个 `providers` 行携带一个 `api_key_cipher`；不建 credentials 表 |
| Key 不可回读 | 只存密文与掩码提示；修改采用覆盖语义 |
| 网关 Key 只存哈希 | 入站校验只需比对 `sha256`，明文仅在生成时展示一次 |
| 至少一个协议 URL | 保存时校验：`openai_base_url`、`anthropic_base_url` 至少有一个非空（含 override） |
| 默认不启用内网 | `allow_internal = 0` 时拒绝解析到 loopback/private/link-local 的 URL |
| 迁移 | 使用 `schema_migrations(version, applied_at)` 表管理；启动时自动执行缺失迁移 |

### 4.3 加密方案（AES-256-GCM）

- 密文布局：`nonce(12B) || ciphertext || tag(16B)`；每次加密随机 nonce，AAD 使用 `provider.id`（防密文跨行搬移）。
- 主密钥（32B）加载顺序：
  1. 环境变量 `GW_MASTER_KEY`（64 位 hex 或 base64，**推荐**）；
  2. 数据目录下 `master.key` 文件（首启自动生成，Linux/macOS 权限 `0600`；Windows 提示使用 ACL 或改用环境变量）。
- 主密钥缺失且无法生成 → 启动失败并给出明确提示（不允许静默降级为明文）。
- 无 `GW_MASTER_KEY` 且 `master.key` 丢失时，已存密文不可解 → 明确报错并引导重新录入 Key（不做自动抹除）。

### 4.4 配置项（启动配置，非业务配置）

| 来源 | 键 | 默认 | 说明 |
| --- | --- | --- | --- |
| flag/env | `LISTEN_ADDR` | `127.0.0.1` | 默认仅本机；改为 `0.0.0.0` 时**强制要求**设置管理密码 |
| flag/env | `PORT` | `9090` | 服务端口 |
| flag/env | `DB_PATH` | `./data/agora.db` | SQLite 路径 |
| flag/env | `GW_MASTER_KEY` | 空 | 主密钥（见 4.3） |
| flag/env | `SSE_IDLE_SECONDS` | `15` | SSE 空闲心跳阈值 |
| flag/env | `MAX_BODY_BYTES` | `16777216` | 入站 body 上限（16MB） |
| flag/env | `LOG_LEVEL` | `info` | `debug/info/warn/error` |
| flag/env | `ADMIN_PASSWORD` | 空 | Web UI 登录密码（远程访问必填） |

业务配置（供应商、网关 Key、刷新间隔）一律存 SQLite，可在 Web UI 修改即时生效。

---

## 5. 对外接口（数据面）

网关对外只暴露 **一个 Base URL + 一个 API Key**。

### 5.1 端点总表

| 方法 | 路径 | 入站协议 | 上游目标 |
| --- | --- | --- | --- |
| `POST` | `/v1/chat/completions` | OpenAI | `provider.openai_endpoint_override` 或 `openai_base_url + "/chat/completions"` |
| `POST` | `/v1/messages` | Anthropic | `provider.anthropic_endpoint_override` 或 `anthropic_base_url + "/messages"` |
| `GET` | `/v1/models` | OpenAI（同时对 Anthropic 客户端可用） | 本地聚合，不转发 |
| `GET` | `/healthz` | — | 存活探针，无需认证 |

> 路径拼接规则：若 `base_url` 以 `/v1` 结尾则追加 `/chat/completions`（OpenAI）/ `/messages`（Anthropic）；网关按「`strings.TrimRight(base, "/")` + 标准子路径」拼接，避免双斜杠。非标准路径必须用 `*_endpoint_override`。

### 5.2 认证与鉴权

| 入站协议 | 接受的凭证头 | 校验 |
| --- | --- | --- |
| OpenAI（`/v1/chat/completions`、`/v1/models`） | `Authorization: Bearer <gw-key>` | `sha256(提供的 key) == gateway_keys.key_hash AND enabled` |
| Anthropic（`/v1/messages`） | `x-api-key: <gw-key>`，兼容 `Authorization: Bearer <gw-key>` | 同上 |
| Anthropic（`/v1/models` 客户端） | 同上 | 同上 |

- 鉴权通过后记录 `last_used_at`（异步、低频批量写入，避免每请求写库）。
- 使用**常量时间比较**（`subtle.ConstantTimeCompare`）比对哈希。

### 5.3 请求处理与头部规则

**入站头 → 上游头映射表**

| 入站头 | 处理 |
| --- | --- |
| `Authorization` | **剥离**，替换为 `Authorization: Bearer <provider-key>`（OpenAI 上游） |
| `x-api-key` | **剥离**，替换为 `x-api-key: <provider-key>`（Anthropic 上游） |
| `Host` | 重写为上游 host |
| `Content-Length` | 重新计算 |
| `Accept-Encoding` | **不转发**（见下）|
| `Connection` / `Keep-Alive` / `Transfer-Encoding` / `Upgrade` / `Proxy-*` / `Te` / `Trailer` | 剥离（逐跳头） |
| `anthropic-version` / `anthropic-beta` | **原样透传**（Anthropic 上游必需） |
| `openai-*` / `User-Agent` / 以及其余业务头 | 原样透传 |
| `extra_headers` 配置 | 合并覆盖（禁止覆盖 `Host`、认证头；覆盖认证头时忽略并 warn） |

- 出站 `http.Transport` 设置 `DisableCompression = true`，并移除入站 `Accept-Encoding`，使上游返回**未压缩**内容：这样 SSE 逐块透传的字节流最干净，不会被 gzip 缓冲区改变分块节奏。网关自身不对透传响应做压缩。
- 请求体：默认直接以**流式 body**（`io.Reader`）转发，不落盘；仅当需要解析 `model` 字段时按 §7.2 的方式做「解析 + 缓存」。

### 5.4 标准错误响应

错误体风格必须与入站协议一致（否则客户端 SDK 报错难读）。

**OpenAI 风格**（`/v1/chat/completions`、`/v1/models`）：

```json
{ "error": { "message": "model 'foo' not found", "type": "invalid_request_error", "param": "model", "code": "model_not_found" } }
```

**Anthropic 风格**（`/v1/messages`）：

```json
{ "type": "error", "error": { "type": "not_found_error", "message": "model 'foo' not found" } }
```

| HTTP | `code` | 触发条件 | Anthropic `error.type` |
| --- | --- | --- | --- |
| 400 | `invalid_request_error` | JSON 无法解析、缺少 `model` 字段 | `invalid_request_error` |
| 401 | `invalid_api_key` | 网关 Key 缺失/错误/已吊销 | `authentication_error` |
| 400 | `upstream_protocol_not_configured` | 该供应商未配置对应协议的 URL | `invalid_request_error` |
| 404 | `model_not_found` | 模型不在任何启用供应商的可用模型中 | `not_found_error` |
| 404 | `not_found` | 未知路径 | `not_found_error` |
| 413 | `payload_too_large` | 请求体超 `MAX_BODY_BYTES` | `invalid_request_error` |
| 502 | `upstream_unreachable` | 连接失败、上游返回非法响应 | `api_error` |
| 503 | `no_available_provider` | 命中供应商但被停用 | `api_error` |
| 504 | `upstream_timeout` | 上游超时 | `api_error` |

- 上游自身返回的 4xx/5xx（含其错误体）**原样回传**，网关不包装——保持客户端可见的上游语义；但**记日志**。
- 客户端断开（`context.Canceled`）：不写响应，仅记日志（`status_code = 499`）。

### 5.5 `/v1/models` 契约

```json
{
  "object": "list",
  "data": [
    { "id": "gpt-4o",               "object": "model", "owned_by": "provider-a" },
    { "id": "provider-a/gpt-4o",    "object": "model", "owned_by": "provider-a" },
    { "id": "provider-b/gpt-4o",    "object": "model", "owned_by": "provider-b" },
    { "id": "claude-sonnet-4-5",    "object": "model", "owned_by": "provider-a" }
  ]
}
```

- 裸模型名仅供「默认路由」查询，`owned_by` = `priority` 最小的供应商。
- 命名空间形式列出**每一个**支持该模型的供应商，便于用户显式指定（对应 PRD FR-4.5）。
- 认证方式与 §5.2 一致；返回 `Cache-Control: no-store`。

---

## 6. 管理接口（控制面）

前缀 `/api`，返回 JSON；远程访问时必须带会话 Cookie。

### 6.1 端点清单

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/api/auth/login` | 校验管理密码，种 HttpOnly 会话 Cookie |
| `POST` | `/api/auth/logout` | 注销 |
| `GET` | `/api/auth/session` | 当前登录状态 |
| `GET` | `/api/health` | 版本、运行时长、供应商健康摘要 |
| `GET` | `/api/providers` | 供应商列表（Key 为掩码） |
| `POST` | `/api/providers` | 新增供应商（含 URL 校验 + SSRF 校验） |
| `GET` | `/api/providers/{id}` | 详情（Key 掩码） |
| `PUT` | `/api/providers/{id}` | 更新（Key 为空表示保持原值） |
| `DELETE` | `/api/providers/{id}` | 删除（级联删除其 `model_cache`） |
| `POST` | `/api/providers/{id}/test` | 连接测试，返回 `{ok, status_code, latency_ms, message, detected_protocols}` |
| `POST` | `/api/providers/{id}/fetch-models` | 立即拉取该供应商模型列表 |
| `GET` | `/api/models` | 聚合模型列表（含来源、是否默认、是否手动、抓取时间） |
| `POST` | `/api/models/refresh` | 全量刷新 |
| `POST` | `/api/models/manual` | 手动添加模型到某供应商 |
| `DELETE` | `/api/models/manual` | 移除手动模型 |
| `GET` | `/api/settings` | 读取设置（含 GateWay Key 掩码、监听地址、刷新间隔） |
| `PUT` | `/api/settings` | 更新设置（刷新间隔、日志级别等） |
| `GET` | `/api/gateway-key` | 返回掩码与提示 |
| `POST` | `/api/gateway-key/reset` | 重新生成网关 Key（明文仅在本响应中返回一次） |
| `GET` | `/api/logs` | 查询日志：`?status=&model=&provider_id=&from=&to=&limit=&offset=` |
| `GET` | `/api/export` | 导出配置（不含明文 Key） |
| `POST` | `/api/import` | 导入配置（合并/覆盖可选） |

### 6.2 关键请求/响应约定

**创建供应商（`POST /api/providers`）**

```json
{
  "name": "供应商A",
  "openai_base_url": "https://api.a.com/v1",
  "anthropic_base_url": "https://api.a.com",
  "openai_endpoint_override": null,
  "anthropic_endpoint_override": null,
  "api_key": "sk-xxx",
  "models_manual": ["gpt-4o"],
  "models_excluded": [],
  "auto_fetch_models": true,
  "priority": 10,
  "timeout_seconds": 120,
  "extra_headers": { "X-Tenant": "me" },
  "extra_body": {},
  "allow_internal": false,
  "enabled": true
}
```

响应 `201`：

```json
{
  "id": "p_7f3c1a",
  "name": "供应商A",
  "openai_base_url": "https://api.a.com/v1",
  "anthropic_base_url": "https://api.a.com",
  "api_key_hint": "sk-****abcd",
  "auto_fetch_models": true,
  "priority": 10,
  "enabled": true,
  "model_count": 12,
  "last_fetch_at": "2026-01-01T10:00:00Z",
  "last_fetch_error": null
}
```

**连接测试（`POST /api/providers/{id}/test`）**

```json
{ "ok": true,
  "openai":    { "ok": true,  "status_code": 200, "latency_ms": 132, "model_count": 12, "message": "OK" },
  "anthropic": { "ok": true,  "status_code": 200, "latency_ms": 158, "message": "OK" },
  "checked_at": "2026-01-01T10:00:00Z" }
```

- OpenAI 侧：`GET {openai_base}/models`（失败则退化为极小 `chat/completions` 探测，`max_tokens=1`）。
- Anthropic 侧：`POST {anthropic_base}/messages`，body 为 `{"model": <该供应商任一模型>, "max_tokens": 1, "messages":[{"role":"user","content":"ping"}]}`。

**更新请求的 Key 语义**：`api_key` 缺省或空字符串 = 保持原值；非空 = 覆盖并重新加密；`"api_key": null` 显式表示不支持（避免误清空）。

---

## 7. 核心流程设计

### 7.1 数据面请求主流程

```
入站请求
  │
  ├─ 1. 路径识别：/v1/chat/completions → openai；/v1/messages → anthropic；其他 → 404
  ├─ 2. body 限制：Content-Length / 读取字节数 > MAX_BODY_BYTES → 413
  ├─ 3. 入站鉴权：提取 Bearer 或 x-api-key → 比对 gateway_keys.key_hash（常量时间）
  │      失败 → 401（按入站协议风格返回错误体）
  ├─ 4. 解析 model 字段（解析 + 本地缓存 body，见 7.2）
  │      缺失/非法 → 400
  ├─ 5. 路由决策（见 7.3）→ (provider, realModel, upstreamURL)
  │      未命中 → 404 model_not_found
  │      协议未配置 → 400 upstream_protocol_not_configured
  ├─ 6. 组装上游请求：
  │      - context.WithTimeout(入站 ctx, provider.timeout_seconds)
  │      - 头改写（§5.3）+ 解密 provider key + extra_headers/body
  │      - 若 realModel != model 则重写 JSON body 的 model 字段（保持其余字段字节不变）
  ├─ 7. 发送并透传响应：
  │      - 若上游 Content-Type 为 text/event-stream → SSE 循环 + 空闲心跳（见 7.4）
  │      - 否则普通 io.Copy 透传（含状态码与响应头，剥离逐跳头）
  ├─ 8. 写日志：状态码、延迟、首字节延迟、上游 URL、错误摘要（脱敏）
  └─ 9. 客户端断开 → ctx 取消 → 上游请求取消、goroutine 退出，日志记 499
```

### 7.2 请求体解析与转发（`model` 字段处理）

要点：既要读出 `model` 判断路由，又要**完整保留原始请求体**（含未知字段、`extra_body` 合并）。

```go
// 1. 读取入站 body，同时限制大小（默认 16MB）
raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBody+1))
if err != nil { /* 400 */ }
if int64(len(raw)) > maxBody { /* 413 */ }

// 2. 仅解出 model（避免为大 body 构造完整结构体）
var probe struct{ Model string `json:"model"` }
_ = json.Unmarshal(raw, &probe)   // 失败则 400 invalid_request_error

// 3. 路由后若非命名空间形式（realModel == probe.Model）→ 原样转发 raw，零改写
//    否则用 map[string]json.RawMessage 做「键序保持的最小改写」：
var obj map[string]json.RawMessage
json.Unmarshal(raw, &obj)
obj["model"], _ = json.Marshal(realModel)   // 仅替换该键
out, _ := json.Marshal(obj)
```

- **无命名空间时零改写**（绝大多数请求），保真度最高。
- 有命名空间时仅重写 `model` 键；键序变化对上游无影响。
- `extra_body` 合并策略：解析为 `map[string]any` 后浅合并（供应商 `extra_body` 覆盖同名键），仅在该供应商配置了 `extra_body` 时才走这条路径。
- 说明（采纳审查意见但降低其权重）：大 body 解析在现代硬件上是毫秒级，先用 `json.Unmarshal` 探测 `model` 足够；上表的「只解 model」已避免全量结构体构造，属低成本的优雅实现。

### 7.3 路由算法

```go
func Resolve(snap *ConfigSnapshot, protocol, model string) (Route, error) {
    // (1) 命名空间：仅当第一段命中已存在的 provider id 才生效
    if p := strings.Index(model, "/"); p > 0 {
        if pid, rest := model[:p], model[p+1:]; snap.HasProvider(pid) {
            return explicitRoute(snap, pid, rest, protocol)
        }
    }
    // (2) 聚合寻址：按 priority 升序取第一个「可用且包含该模型」的供应商
    for _, p := range snap.ProvidersByPriority() {
        if !p.Enabled { continue }
        if !p.KnowsModel(model) { continue }     // 见下方 available models 定义
        return buildRoute(p, model, protocol)
    }
    return Route{}, ErrModelNotFound   // → 404
}
```

**`available models` 的定义**（决定 `KnowsModel` 与 `/v1/models` 的一致性）：

```
available(provider) = (手动模型 ∪ 自动拉取成功缓存) − 排除列表
                      ∪ （自动拉取从未成功时：手动模型，且不阻塞路由）
```

**`buildRoute` 校验**：

| 情况 | 处理 |
| --- | --- |
| 供应商未配置该协议的 URL | `400 upstream_protocol_not_configured` |
| 供应商 `enabled = 0` | `503 no_available_provider` |
| 模型不在该供应商 available 中（显式命名空间场景） | `404 model_not_found` |

**模型名映射（本期不做，仅预留）**：若未来出现「两协议下模型名不同」的情况，在 `providers` 增加 `model_alias_json`（`{对外名: 上游名}`），路由后改写 `model` 即可，无需改动架构。

### 7.4 SSE 透传与心跳保活

读上游是**阻塞**的，因此不能"先阻塞读、再检查 ticker"——那样 ticker 永远来不及触发。正确结构是**读协程负责收字节、主循环负责写客户端**，两者通过 channel 解耦：

```go
type chunk struct {
    data []byte
    err  error
}

// 读协程：只把上游字节搬进 channel，绝不触碰 ResponseWriter
func pumpSSE(ctx context.Context, body io.ReadCloser, out chan<- chunk) {
    defer close(out)
    buf := make([]byte, 32*1024)
    for {
        n, err := body.Read(buf)
        if n > 0 {
            b := make([]byte, n)
            copy(b, buf[:n])
            select {
            case out <- chunk{data: b}:
            case <-ctx.Done():
                return
            }
        }
        if err != nil {                 // io.EOF 也走这里
            select {                    // 主循环可能已退出，不能裸发送，否则读协程永久阻塞
            case out <- chunk{err: err}:
            case <-ctx.Done():
            }
            return
        }
    }
}

// 主循环：唯一写客户端的地方（无需再加锁），空闲时注入心跳
func streamSSE(ctx context.Context, w http.ResponseWriter, resp *http.Response, idle time.Duration) error {
    // 派生可取消 ctx + 登记清理：无论主循环因何退出，读协程都能结束
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()
    defer resp.Body.Close()

    rc := http.NewResponseController(w)
    for k, vs := range resp.Header {              // 透传响应头，剥离逐跳头与 Content-Length
        if isHopByHop(k) || strings.EqualFold(k, "Content-Length") { continue }
        for _, v := range vs { w.Header().Add(k, v) }
    }
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("X-Accel-Buffering", "no")     // 提示 Nginx 关闭缓冲
    w.WriteHeader(resp.StatusCode)
    if err := rc.Flush(); err != nil { return err }

    ch := make(chan chunk, 16)
    go pumpSSE(ctx, resp.Body, ch)                // 读协程随 ctx 取消而退出

    ticker := time.NewTicker(idle)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():                        // 客户端断开 → 上游请求同步取消
            return ctx.Err()
        case c, ok := <-ch:
            if !ok { return nil }                 // 上游流结束
            if c.err != nil { return c.err }
            if _, err := w.Write(c.data); err != nil { return err }
            if err := rc.Flush(); err != nil { return err }
            ticker.Reset(idle)                    // 有数据 → 重置空闲计时
        case <-ticker.C:                          // 空闲超阈值 → 注入 SSE 注释保活
            if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil { return err }
            if err := rc.Flush(); err != nil { return err }
        }
    }
}
```

**实现要点（必须遵守）**

1. **只在空闲时注入**：有数据时字节流与上游完全一致，客户端语义无损（SSE 注释行以 `:` 开头，被规范忽略）。
2. **必须 Flush**：写入后立即 `Flush()`，否则数据滞留在 `bufio` 中，流式失效。
3. **单写入者**：只有主循环写 `ResponseWriter`（读协程只往 channel 投递），因此天然无需写锁；`pumpSSE` 的两次发送都用 `select { case out <- …; case <-ctx.Done(): return }`，保证读协程不会因 channel 满而永久阻塞。
   **退出保障**：`streamSSE` 返程 `defer cancel()` + `defer resp.Body.Close()`——无论主循环因何退出（客户端断开、写失败、上游 EOF），读协程要么收到取消、要么因 body 关闭而读到错误，随后在发送分支走 `<-ctx.Done()` 退出，不留阻塞 goroutine。
4. **上游请求用入站 ctx 派生**：`http.NewRequestWithContext(r.Context(), ...)`，客户端断开时上游请求被取消（`Transport` 会中止连接），读协程与主循环随之退出——这是防泄漏的关键。
5. **首字节延迟单独记录**（`first_byte_ms`），用于区分「上游慢」与「网关慢」。
6. **响应头透传**：`Content-Type: text/event-stream`、`Transfer-Encoding` 由 Go 自动处理；剥离 `Content-Length`（流式不可预知）。
7. **非流式响应**：走普通 `io.Copy` 分支（或同样的小 buffer 循环 + Flush），同样透传状态码与头，不缓冲整个响应体。

### 7.5 模型聚合流程

```
启动时 + 每 N 分钟（默认 10，可配）：
  for provider in snapshot.EnabledProviders():
      go func(p){
        ctx, cancel := context.WithTimeout(ctx, 20s)
        req := GET TrimRight(p.OpenAIBaseURL,"/") + "/models"
               Authorization: Bearer <p.key>    (+ extra_headers)
        resp := do(req)
        if resp.StatusCode != 200 → 记录 last_fetch_error，保留旧缓存（标记 stale）
        parse data[].id → ids
        ids = (ids ∪ p.models_manual) − p.models_excluded
        事务内：DELETE model_cache WHERE provider_id=p AND source='auto'
                INSERT 新 ids（source='auto'，fetched_at=now）
      }(p)
```

- **并发**：每供应商独立 goroutine；整体等待 `WaitGroup` 或直接异步，不阻塞启动（启动时异步执行，首次请求前模型列表可能为空——此时路由回退为「手动模型 + 允许未列出的模型名」？**决定**：不做宽松透传，未在 available 中的模型返回 404，避免"假聚合"（审查第 4 条）。用户可在 Web UI 手动补模型。）
- **失败隔离**：单供应商失败仅记录，不影响其他供应商；Web UI 展示 `last_fetch_error`。
- **去重与 `owned_by`**：同一 `model_id` 出现在多个供应商时，裸名归 `priority` 最小者；命名空间形式全部列出。
- **一致性**：`model_cache` 的读在内存快照中进行（启动加载 + 变更后重建），请求路径不查库。
- **实现细节（Phase 3 落地）**：
  - 拉取响应兼容三种形态：`{"data":[{"id":…}]}`、`{"data":["…"]}`、顶层数组；
  - `model_cache` 的写入由聚合器内部串行化（规避 modernc 在并发写下报 `database is locked`）；
  - 失败时写 `providers.last_fetch_error` 并**保留旧缓存**，成功时清空该字段并刷新 `last_fetch_at`；
  - 刷新间隔取 `settings.model_refresh_seconds`（缺省 10 分钟），单次拉取超时 20s。

### 7.6 连接测试流程

| 协议 | 探测请求 | 判定 |
| --- | --- | --- |
| OpenAI | `GET {openai_base}/models`；若 404/405 则 `POST {openai_base}/chat/completions`，`{"model": <任一模型>, "messages":[{"role":"user","content":"ping"}], "max_tokens":1}` | 2xx = 通过；401/403 = Key 无效；连接错误/超时 = 不可达 |
| Anthropic | `POST {anthropic_base}/messages`，`{"model": <任一模型>, "max_tokens": 1, "messages":[{"role":"user","content":"ping"}]}`，带 `anthropic-version: 2023-06-01` | 同上；`max_tokens: 1` 保证计费最小 |

- 测试请求**不进入请求日志表**（避免污染），但记录到进程日志。
- 未配置某协议 URL 时，该项返回 `{ok:false, message:"未配置该协议地址"}`，不视为整体失败。

### 7.7 SSRF 校验规则（保存供应商时执行）

```go
func ValidateUpstreamURL(raw string) error {
    u, err := url.Parse(raw); if err != nil { return err }
    if u.Scheme != "https" && u.Scheme != "http" { return ErrBadScheme }
    if u.Hostname() == "" { return ErrBadHost }
    ips, err := net.LookupIP(u.Hostname()); if err != nil { return ErrDNS }
    for _, ip := range ips {
        if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
           ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
            return ErrInternalAddr
        }
    }
    return nil
}
```

- `allow_internal = true` 时跳过 IP 段检查（仅允许 `http`/`https` 与合法 host），并在保存时向 UI 提示风险 + 进程日志 `warn`。
- **校验时机**：保存时校验一次；运行期每次转发**不重复校验**（避免每请求 DNS 开销），但记录解析到的 IP 到 `debug` 日志以便排查 DNS rebinding。
- **根本防护在访问控制**：默认只监听 `127.0.0.1`；远程访问强制管理密码（对应 PRD FR-3.5、FR-8.1）。

### 7.8 凭证加解密与脱敏

```go
func Encrypt(master, plaintext []byte, aad string) ([]byte, error) {
    block, _ := aes.NewCipher(master)
    gcm, _ := cipher.NewGCM(block)
    nonce := make([]byte, gcm.NonceSize())          // 12B
    rand.Read(nonce)
    return gcm.Seal(nonce, nonce, plaintext, []byte(aad)), nil  // nonce||ct||tag
}
```

- AAD = `provider.id`：防止把 A 供应商的密文复制到 B 行后被解密使用。
- 掩码函数：`hint(s) = s[:3] + "****" + s[len(s)-4:]`（长度不足时全掩码）。
- **出口唯一性**：所有 API 响应经由序列化结构体（不含 `api_key`/`api_key_cipher` 字段），从类型层面杜绝明文泄露；禁止直接 `SELECT *` 后 `json.Marshal`。
- 日志与错误信息过滤：正则剔除 `sk-[A-Za-z0-9_\-]{8,}`、`gw-[A-Za-z0-9_\-]{8,}` 形态的字符串。

### 7.9 日志写入策略

| 场景 | 是否落库 | 说明 |
| --- | --- | --- |
| 2xx 非流式 | 可选（设置项 `log_success`，默认关） | 只累加计数，避免写放大 |
| 2xx 流式 | 可选（同上） | 记录总字节与总时长 |
| 4xx / 5xx | **必记** | 含上游错误体前 1KB（脱敏后） |
| 上游连接失败 / 超时 | **必记** | `status_code` 记 502/504 |
| 客户端断开 | **必记** | `status_code` = 499 |
| 管理 API 操作（增删改、Key 重置、登录） | **必记** | 审计用途，写入进程日志 + 可选 `logs`（`inbound_protocol = "admin"`） |

- 写入使用**异步批量**（channel + 定时 flush，如 500ms / 100 条），请求路径不等待磁盘。
- 队列满时**丢弃并计数**（丢弃数在 `/api/health` 暴露），绝不允许阻塞转发（可用性优先）。

---

## 8. 前端设计（Web UI）

### 8.1 页面与路由

| 路由 | 页面 | 数据来源 |
| --- | --- | --- |
| `/` | 概览（可选，非 P0） | `/api/health`、`/api/providers`、`/api/models` |
| `/providers` | 供应商管理（列表 + 抽屉/弹窗表单） | `/api/providers` |
| `/models` | 模型列表（聚合视图） | `/api/models` |
| `/settings` | 网关设置（Key、端口、刷新间隔、Base URL 片段） | `/api/settings`、`/api/gateway-key` |
| `/logs` | 请求日志 | `/api/logs` |
| `/login` | 登录（仅远程模式） | `/api/auth/login` |

### 8.2 供应商管理页

**列表列**：名称 · OpenAI URL · Anthropic URL · 模型数 · 优先级 · 状态（启用/停用）· 最近抓取（成功/失败 + 时间）· 操作（测试连接 / 拉取模型 / 编辑 / 删除）。

**表单字段与校验**

| 字段 | 控件 | 校验 |
| --- | --- | --- |
| 名称 | Input | 必填，1–64 字符 |
| OpenAI Base URL | Input | 可选；若填需为合法 http(s) URL |
| Anthropic Base URL | Input | 同上 |
| （至少填一个 URL） | — | 两者皆空 → 阻止保存并提示 |
| 端点覆盖（折叠高级区） | Input ×2 | 可选，需为完整 URL |
| API Key | Password Input | 编辑态显示掩码，占位符「留空表示不修改」 |
| 模型列表 | Tag 输入 | 可手工输入；「拉取模型」按钮填充候选 |
| 排除模型 | Tag 输入 | 可选 |
| 自动拉取模型 | Switch | 默认开 |
| 优先级 | Number | 整数，默认 100，越小越优先 |
| 超时（秒） | Number | 默认 120，范围 5–3600 |
| extra_headers / extra_body | KV 编辑器 | 可选，JSON 合法性校验 |
| 允许内网地址 | Switch | 默认关；开启弹出风险确认 |

**交互细节**：保存后局部刷新列表并显示 toast；删除需输入名称或二次确认；「测试连接」显示双协议结果（状态码 / 耗时 / 错误摘要）。

### 8.3 模型列表页

- 表格：模型 ID · 来源供应商 · 是否默认（priority 最小）· 来源类型（自动/手动）· 抓取时间。
- 开关「显示命名空间形式」：切换裸名视图 / `provider/model` 全量视图。
- 操作：全局刷新、手动添加（选供应商 + 填模型 ID）、移除手动模型、排除某供应商的某模型。
- 空状态引导：若聚合为空，提示「请在供应商页配置供应商并点击拉取模型」。

### 8.4 网关设置页

- 展示 **Base URL**（依据当前监听地址生成）与**网关 Key**（掩码），提供一键复制。
- 直接给出可粘贴到 Agent 的两段环境变量片段（OpenAI 协议 / Anthropic 协议），并提供「复制」按钮——对应 PRD 的 S2 场景。
- 网关 Key 重置：二次确认 + 明文仅显示一次 + 「我已保存」确认。
- 刷新间隔、成功日志开关、监听地址（修改需重启时给出明确提示）。

### 8.5 日志页

- 顶部筛选：时间范围、状态码段、模型、供应商、仅看失败。
- 表格：时间 · 协议 · 模型 · 供应商 · 上游 URL · 状态码 · 延迟 · 首字节 · 错误摘要（可展开查看截断错误体）。
- 分页（`limit`/`offset`），默认按时间倒序。

### 8.6 前端工程约束

- 类型与后端契约同源：手写 `types.ts` 或由 OpenAPI 生成；后端管理 API 提供 OpenAPI 描述（P2）。
- 请求封装统一处理 401（跳转登录）、错误 toast、加载态。
- 不引入重型状态库：`@tanstack/react-query`（或 SWR）足够。

### 8.7 实现说明（Phase 4 落地）

- **技术栈**：Vite + React 19 + TypeScript + Tailwind CSS v4（`@tailwindcss/vite`）+ `@tanstack/react-query` + `react-router-dom`；
- **组件**：按 shadcn/ui 的组织方式与工具链（`class-variance-authority` + `clsx` + `tailwind-merge`）手写所需组件，
  未运行交互式 `npx shadcn init`，因此不引入 Radix 依赖；后续可用 CLI 追加组件；
- **构建产物**：Vite 的 `build.outDir` 直接指向 `internal/webui/dist`，由该包 `//go:embed all:dist` 内嵌进单二进制；
  仓库保留 `internal/webui/dist/robots.txt` 作为占位，保证未构建前端时仍可编译（运行时提示"仅提供 API"）；
- **SPA 行为**：`/` 返回 `index.html`（`Cache-Control: no-store`），`assets/*` 带 `immutable` 长期缓存；
  `/api`、`/v1`、`/healthz` 下的未知路径仍返回 404，避免前端路由掩盖接口拼写错误；
- **开发期**：`npm --prefix web run dev`（5173）通过 Vite 代理 `/api` 到本地网关；
- **登录页**：仅在设置了 `ADMIN_PASSWORD` 时出现（`/api/auth/session` 的 `login_required` 决定）。

---

## 9. 安全设计汇总

| 威胁 | 防护 | 位置 |
| --- | --- | --- |
| Web UI 被公网暴露 | 默认 `127.0.0.1`；远程访问强制管理密码；会话 Cookie HttpOnly + SameSite=Lax；登录限速 | FR-3.5 / §7.7 |
| SSRF（添加内网上游） | 保存时 URL + IP 段校验；`allow_internal` 显式放行；转发期 debug 日志记录解析 IP | §7.7 |
| 数据库文件泄露 / 误提交 | 供应商 Key AES-256-GCM 加密；`.gitignore` 排除 `data/`、`master.key` | §4.3 / §7.8 |
| 主密钥硬编码 | 优先环境变量；文件方式权限 `0600` | §4.3 |
| Key 明文外泄 | 出口结构体不含 Key 字段；掩码回显；日志正则脱敏 | §7.8 |
| 网关 Key 泄露 | 可一键重置（旧 Key 立即失效）；只存哈希；预留多 Key 表结构 | §4.1 / §6.1 |
| 恶意/异常上游耗尽内存 | body 上限、响应体/流总时长保护、日志队列丢弃策略 | FR-9.6 / §7.9 |
| 头部注入 | 剥离逐跳头；`extra_headers` 禁止覆盖 `Host`/认证头 | §5.3 |
| 常量时间比对 | 网关 Key 哈希比对用 `subtle.ConstantTimeCompare` | §5.2 |
| 出站代理绕过 SSRF 校验 | 默认 `Transport.Proxy = nil`（企业环境常见的 `HTTP_PROXY`/`HTTPS_PROXY` 会被 `ProxyFromEnvironment` 采用，使实际连接目标与 IP 校验对象不一致）；确需代理时必须显式配置代理地址并校验 | §7.7 |

---

## 10. 边界条件与异常处理

| 场景 | 行为 |
| --- | --- |
| 入站 JSON 非法 | 400 `invalid_request_error`（按入站协议风格） |
| 缺少 `model` | 400 `invalid_request_error` |
| 模型存在但该供应商未配置该协议 URL | 400 `upstream_protocol_not_configured`，错误信息指明「供应商 X 未配置 Anthropic 地址」 |
| 模型未被任何供应商声明 | 404 `model_not_found`；错误信息附带「可用模型数」便于自查 |
| 上游 401（供应商 Key 失效） | 原样回传上游 401 + 记录日志（Web UI 供应商页显示红色健康状态） |
| 上游 429 | 原样回传（单凭证无故障转移），日志标注 `rate_limited` |
| 上游返回非 JSON / HTML（如 Cloudflare 拦截页） | 原样回传 + 日志记录前 1KB，便于识别 |
| 上游超时（含 SSE 长时间无数据但连接未断） | 连接级超时按 `timeout_seconds`；SSE 空闲由心跳保活（不主动断开，避免误杀长推理） |
| 客户端断开 | 取消上游请求，日志记 499 |
| 配置保存中并发请求 | 读写分离 + 快照替换，请求要么用旧快照要么用新快照，不会读到半更新状态 |
| 拉取模型时供应商不可用 | 保留旧缓存 + 标记陈旧 + 展示错误；不影响路由到其他供应商 |
| 数据库文件被删除 | 启动时创建新库并生成新网关 Key，控制台醒目提示 |
| 端口被占用 | 启动失败并输出明确提示 |

---

## 11. 测试策略

| 层次 | 内容 | 工具/方式 |
| --- | --- | --- |
| 单元测试 | 路由解析（含 `provider/model` 歧义、priority、404）、URL 校验、加解密与掩码、头部改写规则、模型名重写 | Go `testing`，表驱动 |
| 契约测试 | `/v1/chat/completions`、`/v1/messages`、`/v1/models` 的请求/响应与错误体风格 | `httptest.Server` 冒充上游 |
| 流式测试 | 上游分块吐出 SSE + 静默 20s，断言：客户端收到心跳注释、内容字节与上游一致、总时长不受影响 | 自建 mock 上游 + 断言首字节延迟 |
| 取消测试 | 客户端中途关闭连接，断言上游 mock 观察到 ctx 取消、网关 goroutine 数回落 | `runtime.NumGoroutine` 差值与 mock 信号 |
| 安全测试 | 内网地址被拒、`allow_internal` 放行、响应不含明文 Key、日志脱敏 | 单元 + 集成 |
| 存储测试 | 迁移、原子写、并发读、Key 覆盖语义 | 临时 SQLite 文件 |
| 前端测试 | 表单校验、Key 掩码回显、错误提示 | Vitest + Testing Library（可选） |
| 端到端验收 | 用真实 Claude Code / OpenAI SDK 指向本地网关跑通（对应 PRD §7） | 手动 + 脚本 |
| 跨平台 | 路径拼接、数据目录选择、SQLite DSN 转换、lock 文件、`--no-color`/JSON 日志输出、优雅关闭入口、服务化子命令 | 单元测试 + CI 三平台矩阵（§12 跨平台兼容性设计） |

**验收关键用例（必须自动化）**

1. 同协议透传：OpenAI 入站 → OpenAI 上游，字节级一致（除 `model` 重写场景）。
2. 双协议可用：同一供应商两条路径都返回 200。
3. SSE 心跳：上游静默 > `SSE_IDLE_SECONDS` 时客户端收到 `: keep-alive`。
4. 断连取消：半关闭客户端 → 上游 ctx 取消。
5. 命名空间路由：`a/gpt-4o` 强制走 provider-a，且上游收到的 `model` 为 `gpt-4o`。
6. 斜杠模型名保护：`meta-llama/Llama-3-70B` 不被误判为命名空间。
7. SSRF：`http://127.0.0.1:11434` 被拒；开启 `allow_internal` 后可保存。

---

## 12. 部署与运维

- **构建**：`make build`（或 `build.ps1`）：`npm --prefix web ci && npm --prefix web run build` → 按 ADR-001 的矩阵执行 `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build -trimpath -ldflags="-s -w -X main.version=<ver>"`，产出 `dist/agoramodel-<os>-<arch>[.exe]`。构建脚本必须一次产出 Windows 与 Linux 两套产物（跨平台兼容性见下文）。
- **运行**：`./agoramodel --listen 127.0.0.1 --port 9090 --db ./data/agora.db`。
- **依赖**：无外部服务依赖；单文件 + 数据库文件。
- **反向代理**（若需要 HTTPS/远程）：

```nginx
location / {
    proxy_pass http://127.0.0.1:9090;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_buffering off;              # SSE 必须关闭缓冲
    proxy_cache off;
    proxy_read_timeout 3600s;         # 长推理场景
    chunked_transfer_encoding on;
}
```

- **服务化（已定案：`kardianos/service`）**：同一二进制内建 `install / uninstall / start / stop / restart / status` 子命令，自动对接各平台服务管理器——**Windows：SCM；Linux：systemd；macOS：launchd**。
  - **实现状态**：子命令已实现并自检；install 时会把 --config/--data-dir/--db/--port 转为绝对路径写入服务配置；管理密码**不写入**服务参数（避免明文落盘），需通过环境变量注入；实机安装需管理员/root 权限。
  - 该库为**纯 Go（无 CGO）**，不影响 ADR-001 的跨平台基线。
  - **停止事件必须接入优雅关闭**：`service.Interface.Stop()` → `cancel()` → `http.Server.Shutdown(ctx)` → 停止日志批量写入并 flush → 关闭 SQLite 连接（WAL checkpoint），确保不丢日志、不损坏数据库。
  - **服务模式没有控制台**：日志必须落文件（或平台日志），不得依赖 stdout；建议 `--log-format=json` 便于采集。
  - Linux 安装通常需要 root 权限；若不想用服务子命令，也可手工用下面的 systemd unit 托管（二选一即可）。

```ini
[Unit]
Description=AgoraModel Gateway
After=network-online.target

[Service]
ExecStart=/usr/local/bin/agoramodel --listen 127.0.0.1 --port 9090 --db /var/lib/agoramodel/agora.db
Environment=GW_MASTER_KEY=<64 位 hex>
Restart=on-failure
User=agoramodel

[Install]
WantedBy=multi-user.target
```
- **备份**：备份 SQLite 文件即可（`data/agora.db` + `-wal`/`-shm`）；或用 `/api/export`。备份文件含密文，泄露风险可控（对应 §9）。
- **升级**：替换二进制 → 启动时自动执行迁移；数据库向后兼容（迁移只增不改语义）。

### 跨平台兼容性设计（Windows / Linux / macOS）

同一份代码在三端行为一致：所有平台分支集中在 `internal/platform` 包（`MasterKeySource`、`DataDir`、`LockFile`、`SignalContext`、`ServiceRunner`），**主线逻辑不出现 `runtime.GOOS` 判断**。

| # | 差异 | Windows | Linux / macOS |
| --- | --- | --- | --- |
| 1 | 文件权限 | `os.Chmod(0600)` 只影响只读位，**不能用于保护主密钥** → 优先 `GW_MASTER_KEY` 环境变量（或可选 DPAPI） | `0600` 有效（macOS 同）；Keychain 集成不在本期范围 |
| 2 | 信号 | 只有 `os.Interrupt`（Ctrl+C），`SIGTERM` 不会被投递 | `SIGINT` + `SIGTERM` 均正常 |
| 3 | 优雅关闭入口 | 信号之外还有 SCM 的「停止」事件（服务模式） | Linux：信号即可；macOS：launchd 停止时同样需要接入 `Stop()` 回调 |
| 4 | 数据目录 | `%AppData%\AgoraModel`（`os.UserConfigDir()`） | Linux `~/.config/agoramodel`；macOS `~/Library/Application Support/AgoraModel`（同一 API 得出） |
| 5 | SQLite DSN | 路径统一转 `/`（如 `file:C:/data/agora.db`），避免反斜杠转义差异 | 无差异 |
| 6 | 出站代理 | 企业环境常见 `HTTP_PROXY`/`HTTPS_PROXY` 会被 `ProxyFromEnvironment` 采用，导致 SSRF 的 IP 校验与实际连接目标不一致 → **默认 `Transport.Proxy = nil`**（同时进 §9 安全表） | 同 |
| 7 | 防火墙 | 监听 `0.0.0.0` 首次触发防火墙授权弹窗；`127.0.0.1` 不触发 | Linux：ufw/nftables 自行放行；macOS：应用防火墙首次会弹窗 |
| 8 | 单实例保护 | lock 文件（`LockFileEx`） | lock 文件（`flock`） |
| 9 | 控制台输出 | Windows 10 / 11 的控制台（Windows Terminal、新版 `cmd`）已支持 ANSI/VT 与 UTF-8，无需为旧系统降级；仍提供 `--log-format=json\|text`、`--no-color` 以便服务模式或重定向到文件 | 正常 |
| 10 | 路径大小写 | 不区分大小写 | Linux 区分大小写：静态资源名、路由、`embed` 文件名必须完全一致；macOS APFS 默认不区分但可格式化为敏感 → **一律按「区分大小写」编码** |
| 11 | 未签名二进制的首次运行 | 可能触发 SmartScreen 提示（可选代码签名） | macOS **Gatekeeper 会直接拦截未签名/未公证的二进制**，需 `xattr -dr com.apple.quarantine agoramodel`（或右键「打开」），或在构建脚本里做 `codesign -s -` ad-hoc 签名——**必须写进文档** |
| 12 | 服务化后端 | SCM（Windows 服务） | Linux：systemd；macOS：launchd。三者统一由 `kardianos/service` 封装（见「服务化」条） |

**其他跨平台约定**

- 时间统一以 UTC 存储（RFC3339），仅展示层本地化。
- 临时文件与测试目录使用 `os.MkdirTemp` / `t.TempDir()`，禁止硬编码 `/tmp`。
- 仓库加 `.gitattributes`（`* text=auto eol=lf`、`*.ps1 eol=crlf`），避免脚本行尾在两端表现不一。
- SQLite 并发：单写协程 + `busy_timeout=5000`；WAL 下读连接可多开。modernc 在**极端并发写**（100+ 同时写）下可能出现 `database is locked`，本设计的单写协程正好规避。
- 平台版本下限（已确认）：**Windows 10 / Windows 11** 与 Linux；Go 1.22+ 直接覆盖，不需要任何旧系统兼容分支（见 ADR-001）。
- CI / 本地脚本矩阵：`{windows-latest, ubuntu-latest, macos-latest}` × `{amd64, arm64}` 编译 + 冒烟（`/healthz` + 一次 mock 上游透传）；macOS 侧额外确认未签名二进制的启动方式（见上表第 11 项）。

---

## 13. 已知取舍（Explicitly Rejected）

| 被拒项 | 理由 |
| --- | --- |
| `/v1/models` 的 Anthropic 格式内容协商 | Anthropic 官方较晚才提供 `/v1/models`，且 Claude Code / Anthropic SDK 等主流客户端**不依赖它**工作（模型名写死在配置里）。为极少数严格校验 schema 的客户端做内容协商，投入产出比极低。保持 OpenAI 格式。 |
| 水平扩展 / 多实例 | 个人自用网关，SQLite 单机足够；定时任务用 goroutine + ticker，不阻塞主流程。单实例设计能省掉分布式配置同步的全部复杂度。 |
| 把「单网关 Key」当作安全漏洞 | 它本质是**可用性问题**，且用户明确要求只提供一个 Key。v1 在数据库层用 `gateway_keys` 表预留结构，未来零改表即可支持多 Key / 吊销。 |
| 模型别名表 | 用 `provider/model` 语法即可解决同名冲突，不引入新增概念与配置面。 |
| 请求体全量流式解码 | 收益被夸大（几十 KB JSON 解析是微秒级）；已用「只解 `model` + 无改写时零处理」达成同样的优雅度。 |

---

## 14. 实现顺序建议

```
透传网关 → SSE 保活 + context 取消 → SQLite + 凭证加密 → SSRF 校验 → 错误日志
        → 模型聚合与 /v1/models → provider/model 命名空间路由 → Web UI → 打包与验收文档
```

前五步完成后即得到一个可用的「配置驱动」网关（Phase 1–2）；模型聚合与 Web UI 完成后达到日常使用标准（Phase 3–4）。详细任务拆分、工时与验收判据见 `docs/TODO.md`。
