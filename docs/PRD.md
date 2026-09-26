# AgoraModel 产品需求文档（PRD）

> 产品代号：**AgoraModel**（仓库目录 `agora-model`）
> 一句话定义：**本地优先的「AI 模型接入网关 + 配置控制台」——给所有 AI Agent 一个固定入口。**
> 版本：v1.0（初稿）｜状态：待评审
> 需求来源：用户与 DeepSeek 的方案讨论（含 3 轮约束收敛 + 1 轮对抗性审查后的修正）

---

## 1. 背景与问题定义

### 1.1 现状

用户持有**多个 token plan 账号与多个 API key 账号**，同时使用**多个 AI Agent**（Claude Code、Codex、Cursor 等），目前通过 **CC Switch** 管理这些配置，并借助 **WebDAV** 在多台电脑之间复用配置。

### 1.2 痛点

| 维度 | 痛点 | 根因 |
| --- | --- | --- |
| 横向（Agent 侧） | 每当出现新的 AI Agent，就需要人工为其单独配置模型供应商；CC Switch 仅覆盖 Claude Code、Codex、Gemini CLI、OpenCode、OpenClaw、Hermes Agent 等约七款工具 | CC Switch 采用「直接写入各工具原生配置文件」的模式，每支持一个新工具都要开发专门的适配逻辑 |
| 纵向（供应商侧） | 每接入一个新模型/供应商，都要在 CC Switch 里为**每一个**已支持的 Agent 逐个添加自定义供应商 | 缺少中间解耦层，形成 **N×M** 的重复劳动（N 个 Agent × M 个供应商） |

### 1.3 解决思路

**将「供应商配置」与「Agent 配置」解耦**：不再让每个 Agent 直接对接供应商，而是在中间引入一个统一的网关层。所有 Agent 只对接网关（**一个固定的 Base URL + 一个固定的 API Key**），由网关负责按**协议**与**模型名**路由到任意供应商。

- 新增供应商 → 只在网关配置一次，所有 Agent 立即生效。
- 新增 Agent → 只要它支持自定义 Base URL（绝大多数 AI Agent 的基本能力），即可**零适配**接入。
- 跨设备 → 配置存在服务端（数据库）中，天然多端一致。

---

## 2. 目标与非目标

### 2.1 产品目标

| 编号 | 目标 | 说明 |
| --- | --- | --- |
| G1 | **单一入口** | 对外只暴露一个 Base URL 和一个 API Key，所有 Agent 共用 |
| G2 | **零模板动态接入** | 不预设任何供应商类型/模板，新增供应商只填双 URL + Key |
| G3 | **双协议原生透传** | 同时提供 OpenAI `/v1/chat/completions` 与 Anthropic `/v1/messages`，按入站协议选择上游对应协议 URL，**不做格式转换** |
| G4 | **模型聚合可查询** | 对外 `GET /v1/models` 返回所有供应商模型的聚合列表 |
| G5 | **跨设备可用** | 配置集中在服务端，换机器无需重新配置各 Agent 的供应商 |
| G6 | **轻量可维护** | 单二进制部署，内嵌前端静态资源，个人可长期维护 |

### 2.2 明确不做（Out of Scope · v1）

| 不做的事 | 理由 |
| --- | --- |
| 协议双向转换（OpenAI ⇄ Anthropic 的请求/响应/SSE 事件映射） | 用户澄清：所用供应商**都同时提供两种协议的 URL**，直接透传即可 |
| 多凭证 / 凭证池 / 轮询 / 故障转移 | 用户澄清：**一个供应商只配置一个凭证** |
| 用量统计与计费 | 用户澄清：暂不考虑 |
| WebDAV 同步 | 用户澄清：暂不考虑（配置在服务端已天然多端可用） |
| Anthropic 格式的 `/v1/models` 内容协商 | 主流 Anthropic 客户端（Claude Code、Anthropic SDK）不依赖该接口，投入产出比低 |
| 多实例 / 水平扩展 / 分布式 | 个人自用工具，SQLite 单机足够；定时任务是 goroutine + ticker，不阻塞主流程 |
| 多租户、团队权限、RBAC | 无此需求 |
| MCP 服务器代理、语义缓存、Prompt 重写 | 属于后续扩展方向，非本期 |

---

## 3. 术语表

| 术语 | 定义 |
| --- | --- |
| **Provider（供应商）** | 一个上游模型服务的接入配置，包含 OpenAI 协议基地址、Anthropic 协议基地址、凭证、模型列表、优先级 |
| **凭证（Credential）** | 供应商的 API Key / Token Plan Key。v1 中每个供应商**有且仅有一个** |
| **Gateway Key（网关 Key）** | 本产品对外颁发的唯一 API Key，所有 Agent 用它访问网关 |
| **Inbound Protocol（入站协议）** | Agent 请求网关时使用的协议：`openai`（`/v1/chat/completions`）或 `anthropic`（`/v1/messages`） |
| **Upstream Protocol（上游协议）** | 网关转发给供应商时使用的协议，由入站协议直接决定，不做转换 |
| **Model Namespace（`provider/model`）** | 显式指定供应商的模型书写形式，如 `provider-a/gpt-4o` |
| **聚合模型列表** | 所有供应商模型 ID 的去重并集，由 `GET /v1/models` 对外暴露 |
| **控制台（Web UI）** | 网关自带的管理界面：供应商管理、模型列表、网关设置、日志 |

---

## 4. 用户与使用场景

### 4.1 用户画像

- **主用户（P0）**：本人，个人开发者，一人维护、多台机器使用，追求「配置一次、处处生效」。
- **次要用户（P2）**：小团队共享一台网关（同一 Key 或后续的多 Key 增强）。

### 4.2 核心场景

| 编号 | 场景 | 期望体验 |
| --- | --- | --- |
| S1 | **接入新供应商** | 打开 Web UI → 填名称、OpenAI Base URL、Anthropic Base URL、API Key → 保存 → 所有 Agent 立刻可用，全程 ≤ 1 分钟，**不选择任何供应商类型模板** |
| S2 | **接入新 Agent** | 把它的 Base URL 指向网关、Key 填网关 Key 即可；无需等网关发版适配 |
| S3 | **换一台电脑使用** | 访问已部署的网关，或在本地启动网关并导入配置；各 Agent 的环境变量指向同一个网关地址 |
| S4 | **排查问题** | 在「日志」页按时间/状态码/模型筛选，看清是 Agent 发错了还是上游拒了 |
| S5 | **同名模型精确路由** | 在模型名中写 `provider-a/gpt-4o` 强制走指定供应商 |
| S6 | **查看可用模型** | 在 Web UI「模型列表」页或调用 `GET /v1/models` 看到聚合结果与来源供应商 |

---

## 5. 功能需求（FR）

> 优先级：**P0** = v1 必须交付；**P1** = 低成本高收益增强；**P2** = 可选/后续。

### FR-1 动态供应商注册（P0）

- **FR-1.1** 供应商数据结构**完全开放、无类型枚举**：不依赖任何预置模板，字段为「名称 + 两个协议基地址 + 凭证 + 模型 + 可选高级项」。
- **FR-1.2** 支持 **双 URL**：`openai_base_url` 与 `anthropic_base_url`，分别对应两种协议的上游基地址。
- **FR-1.3** 支持 **端点覆盖**：`openai_endpoint_override` / `anthropic_endpoint_override`，当上游路径非标准（不是 `/chat/completions`、`/messages`）时直接写完整 URL，优先级高于 base_url 拼接。
- **FR-1.4** 支持 **参数透传**：`extra_headers`（追加/覆盖请求头，如特殊认证头）、`extra_body`（合并进请求体的固定字段）。用于适配需要额外认证头或特殊参数的供应商。
- **FR-1.5** 支持 `timeout`（上游请求超时，默认 120s）。
- **FR-1.6** 支持 `priority`（整数，越小越优先），用于同名模型的默认选择。
- **FR-1.7** 支持 `auto_fetch_models` 开关与手动模型列表。
- **FR-1.8** 支持 `allow_internal`（默认 `false`）显式放行内网地址（如本地 Ollama / vLLM）。
- **FR-1.9** 供应商的增、删、改、查、启用/停用、**连接测试**。

**验收**：新增一个此前从未见过的供应商，仅填写双 URL + Key 即可成功发起 `/v1/chat/completions` 与 `/v1/messages` 请求，且**未修改任何代码、未选择任何模板**。

### FR-2 双协议透传网关（P0）

- **FR-2.1** 提供 `POST /v1/chat/completions`（OpenAI 协议入站）与 `POST /v1/messages`（Anthropic 协议入站）。
- **FR-2.2** 入站 `/v1/chat/completions` → 转发到 `provider.openai_base_url + /chat/completions`（或 `openai_endpoint_override`）。
- **FR-2.3** 入站 `/v1/messages` → 转发到 `provider.anthropic_base_url + /messages`（或 `anthropic_endpoint_override`）。
- **FR-2.4** **请求与响应均不做格式转换**，仅做：鉴权校验 → 模型路由 → 认证头替换 → 请求转发 → 响应（含 SSE 流）逐字节透传。
- **FR-2.5** 头部处理规则：剥离网关自身的入站认证头，注入供应商真实凭证；`anthropic-version`、`anthropic-beta`、`openai-*` 等业务头原样透传；移除 `Host`、`Content-Length`、`Connection` 等逐跳头。
- **FR-2.6** 上游返回的状态码、响应头（在安全范围内）、响应体原样回传。

**验收**：同一份 messages 通过 OpenAI 路径与 Anthropic 路径请求同一供应商，二者均正常返回；网关无任何 JSON 字段级改写（除 `model` 字段的命名空间剥离）。

### FR-3 认证与网关 Key（P0）

- **FR-3.1** 全网关**对外只使用一个 API Key**（Gateway Key），可在 Web UI 生成、重置、复制。
- **FR-3.2** 兼容两种入站认证头：OpenAI 入站接受 `Authorization: Bearer <gw-key>`；Anthropic 入站接受 `x-api-key: <gw-key>`（同时兼容 `Authorization: Bearer`）。
- **FR-3.3** 鉴权失败返回 401，且**错误响应体按入站协议风格**返回（OpenAI 风格 `{"error":{...}}`；Anthropic 风格 `{"type":"error","error":{...}}`）。
- **FR-3.4** 数据库层使用 `gateway_keys` **表**存储（而非单字段配置），为未来「多 Key、按 Agent 发放、单独吊销」预留结构；v1 UI 仅暴露一个 Key。
- **FR-3.5** Web UI 自身访问控制：默认仅监听 `127.0.0.1`；若需远程访问，必须启用管理密码登录（会话 Token），禁止在公网裸奔。

### FR-4 模型聚合（P0）

- **FR-4.1** 启动时对所有 `auto_fetch_models = true` 的供应商请求 `{openai_base_url}/models`（`Authorization: Bearer <provider-key>`），解析 `data[].id`。
- **FR-4.2** 定时刷新（默认 10 分钟一次；支持手动「刷新」按钮与单供应商「拉取模型」）。
- **FR-4.3** 聚合结果 = 所有供应商模型 ID 的去重并集；记录每个模型的来源供应商与优先级。
- **FR-4.4** 对外 `GET /v1/models` 返回 **OpenAI 格式**：`{"object":"list","data":[{"id":"gpt-4o","object":"model","owned_by":"provider-a"}, ...]}`。
- **FR-4.5** 同名模型在列表中同时提供裸名 `gpt-4o` 与命名空间形式 `provider-a/gpt-4o`，使用户一眼看清来源。
- **FR-4.6** 支持手动覆盖：手动添加模型、手动排除（黑名单）模型。
- **FR-4.7** 拉取失败（网络错误、401、超时）不得影响其他供应商，也不得导致网关不可用；失败原因在 Web UI 与日志中可见。

**验收**：在 Web UI 新增供应商后，`GET /v1/models` 在刷新后包含其模型；关闭某供应商后，其模型从聚合列表消失。

### FR-5 路由（P0）

- **FR-5.1** **按模型名路由**：解析请求体 `model` 字段 → 在供应商模型表中查找支持该模型的供应商。
- **FR-5.2** **优先级决策**：多个供应商支持同名模型时，取 `priority` 最小者。
- **FR-5.3** **显式命名空间路由（P1）**：`model` 形如 `provider-a/gpt-4o` 时强制走 `provider-a`，转发前把 `model` 改写为 `gpt-4o`。
- **FR-5.4** **歧义处理**：仅当斜杠前的第一段**恰好命中已存在的 provider id** 时才按命名空间解析，否则视为完整模型名（兼容 `meta-llama/Llama-3-70B` 这类含斜杠的模型名）。
- **FR-5.5** 未匹配到任何供应商 → 返回 404 `model_not_found`；无模型列表可用的供应商 → 明确报错，绝不静默转发到错误上游。
- **FR-5.6** 上游网络错误 → 502；上游超时 → 504；均带结构化错误信息。

### FR-6 Web UI（P0）

四个页面（保留三个核心页 + 审查后新增日志页）：

| 页面 | 内容 |
| --- | --- |
| **供应商管理** | 列表（名称、双 URL、模型数、启用状态、连接状态）；新增/编辑表单（名称、OpenAI Base URL、Anthropic Base URL、API Key、模型列表、`priority`、`allow_internal`、高级项）；操作：连接测试、拉取模型、启用/停用、删除（二次确认） |
| **模型列表** | 聚合模型表格（模型 ID、来源供应商、是否默认、是否手动）、手动添加/排除、全局刷新 |
| **网关设置** | 展示/重置/复制网关 API Key；展示并修改监听地址与端口；展示 Base URL 与 Key 的一键复制片段（OpenAI 版与 Anthropic 版环境变量） |
| **日志** | 请求级日志表格，按时间/状态码/模型/供应商筛选；展示延迟、上游状态码、错误摘要（敏感信息已脱敏） |

- **FR-6.1** 首屏可用的**零安装**体验：静态资源内嵌进后端二进制，单进程启动即可访问。
- **FR-6.2** 表单校验：URL 协议与可达性、必填项、`priority` 数字、Key 掩码回显（**永不回传明文**）。
- **FR-6.3** 所有写操作给出明确成功/失败反馈；错误信息包含可操作提示。
- **FR-6.4** 「连接测试」：向该供应商发一个最小请求（OpenAI 用 `/models` 或极小的 `chat/completions`；Anthropic 用 `/messages`），返回耗时、状态码与错误摘要。

### FR-7 日志与可观测（P0）

- **FR-7.1** 请求日志表：`id, ts, request_id, inbound_protocol, model, provider_id, upstream_url, status_code, latency_ms, stream, error_msg, client_ip`。
- **FR-7.2** 写入策略：失败请求（4xx/5xx）与上游连接失败/超时**必记**；成功请求只记计数或按开关记录，避免写放大。
- **FR-7.3** 错误体截断保存（前 1KB）；不落 `Authorization` / `x-api-key` 等敏感头，不落完整请求体。
- **FR-7.4** 进程日志（stdout）分级输出：启动配置、路由决策、上游错误、SSE 心跳异常。
- **FR-7.5（P2）** 日志保留策略（按天/条数清理），防止 SQLite 无限增长。

### FR-8 安全（P0）

- **FR-8.1 SSRF 防护**：保存供应商时校验 URL —— 仅允许 `http`/`https`；解析主机名得到的 IP 若为 loopback / private / link-local / 非全局单播则拒绝；`allow_internal = true` 时显式放行并留记录。真实风险场景是「自己配置时的笔误」与「Web UI 意外暴露到公网」。
- **FR-8.2 凭证加密存储**：供应商 `api_key` 以 **AES-256-GCM** 密文落库，nonce 随机、密文内含认证标签。主密钥来源优先级：环境变量 `GW_MASTER_KEY` > 首次启动自动生成的 `master.key` 文件（权限 `0600`）。
- **FR-8.3 脱敏**：Web UI 与 API 回显 Key 一律为 `sk-****abcd` 形式；修改 Key 采用「覆盖」语义，不支持回读。
- **FR-8.4 会话安全**：Web UI 若启用登录，使用 HttpOnly + SameSite 的会话 Cookie；登录失败有速率限制。
- **FR-8.5 不信任上游**：遵循上游响应的大小上限限制（如响应体/SSE 总时长与字节上限保护），避免恶意/异常上游耗尽内存。

### FR-9 流式与稳定性（P0）

- **FR-9.1 SSE 直接透传**：上游 `text/event-stream` 逐块读取、立即 Flush 写出，不解析、不聚合。
- **FR-9.2 SSE 心跳保活**：采用**带读超时的转发循环**——空闲达到阈值（默认 15s）时注入一行 SSE 注释 `: keep-alive\n\n` 并 Flush，收到数据后重置计时器。只在真正空闲时注入，字节流对客户端语义透明。
- **FR-9.3 上下文取消**：使用 `http.NewRequestWithContext(ctx, ...)`，`ctx` 取自入站请求；客户端断开时上游请求立即取消，转发 goroutine 退出，无泄漏。
- **FR-9.4 超时**：上游连接/整体超时可配置；客户端写入超时可选。
- **FR-9.5 并发**：不使用全局锁串行化转发；配置读取采用快照（读多写少），配置变更后新请求立刻生效，不中断进行中的流。
- **FR-9.6 请求体限制**：限制入站 body 最大值（默认 16MB，覆盖 base64 图片场景），超限返回 413。

### FR-10 配置存储与生效（P0）

- **FR-10.1** 使用 **SQLite**（WAL 模式）持久化：`providers`、`gateway_keys`、`settings`、`logs`、`models`（缓存）。
- **FR-10.2** 事务保证原子性；不使用「JSON 文件覆盖写入」方式（易因中断损坏）。
- **FR-10.3** 配置变更立即对**新请求**生效（内存快照 + 原子替换），不要求重启。
- **FR-10.4** 提供导入/导出（JSON）用于备份与迁移；导出**不含明文 Key**（可选：加密导出）。

### FR-11 部署与运行（P0）

- **FR-11.1** 单二进制：Go 编译产物内嵌前端静态资源；SQLite 使用纯 Go 驱动（免 CGO），便于跨平台交叉编译。
- **FR-11.2** 启动参数/环境变量：监听地址、端口、数据库路径、主密钥、日志级别。
- **FR-11.3** 首次启动自动生成网关 Key 并打印到控制台 + 写入数据库。
- **FR-11.4** 提供 Windows / Linux / macOS 的运行说明；**内建服务化子命令**：`install / uninstall / start / stop / restart / status`（基于 `kardianos/service`，分别对接 Windows SCM、Linux systemd、macOS launchd）；服务停止事件走优雅关闭（关闭 HTTP 服务、flush 日志批次、checkpoint 并关闭 SQLite）；**服务模式下日志写文件而非 stdout**。
- **FR-11.5** 文档化反代注意事项（Nginx：`proxy_buffering off`、`proxy_read_timeout` 调大、`chunked_transfer_encoding` 保持），因为 SSE 经反代易被缓冲/掐断。

---

## 6. 非功能需求（NFR）

| 维度 | 要求 |
| --- | --- |
| 性能 | 透传路径无 JSON 重新序列化开销；单请求额外延迟 < 5ms（本地回环，不含上游）；单机支持数十并发流式请求 |
| 可靠性 | 上游异常不影响网关本身；单供应商失败不影响其他供应商；配置写入原子 |
| 兼容性 | 与 OpenAI 官方 SDK、Anthropic 官方 SDK、Claude Code、Codex、Cursor 等客户端兼容；模型名含 `/` 的供应商不被破坏 |
| 安全 | 见 FR-8；默认仅本机可访问；Key 加密且不可回读 |
| 可维护性 | 单二进制、单进程、单数据库文件；核心逻辑模块化（路由 / 透传 / 聚合 / 存储 / 安全），便于单测 |
| 可移植性 | 目标平台 **Windows 10 / 11**、**Linux**、**macOS**（均为 amd64 / arm64）；全量 `CGO_ENABLED=0`，静态链接、无运行时依赖（无 libc / Node / JVM），**每平台单文件交付**；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`（见 DESIGN ADR-001 与「跨平台兼容性设计」） |
| 跨平台一致性 | 同一份代码在三端行为一致：文件权限、信号与优雅关闭、服务化方式、数据目录、出站代理、控制台输出等差异集中隔离在 `internal/platform`；CI 三平台编译 + 冒烟 |
| 可观测 | 结构化错误日志 + 请求日志页；关键路径可定位到「Agent 请求 → 路由决策 → 上游响应」 |
| 易用性 | 新供应商接入零模板；表单内有 Base URL/Key 的一键复制片段，便于贴进 Agent 配置 |

---

## 7. 整体验收标准

1. **零模板接入**：新增任意「支持 OpenAI + Anthropic 双协议」的供应商，仅通过 Web UI 填 4 个字段（名称、双 URL、Key）即可用。
2. **双协议可用**：Claude Code 配 `ANTHROPIC_BASE_URL=http://127.0.0.1:9090` + `ANTHROPIC_API_KEY=gw-xxx` 可正常对话；OpenAI 兼容 Agent 配 `OPENAI_BASE_URL=http://127.0.0.1:9090/v1` + `OPENAI_API_KEY=gw-xxx` 可正常对话。
3. **模型聚合**：`GET /v1/models` 返回全部供应商模型的去重并集，且 `owned_by` 正确。
4. **长任务不中断**：上游静默 90s 以上的推理请求，在经反向代理后仍能完成（SSE 心跳生效）。
5. **断连无泄漏**：客户端中途断开后，网关 goroutine 数回落，上游请求被取消。
6. **安全基线**：数据库中无明文 Key；`/api` 与 Web UI 不能在内网地址上被配置为上游（除非显式 `allow_internal`）；对外 Key 回显为掩码。
7. **可排查**：任一失败请求可在日志页按时间查到状态码、上游 URL、错误摘要（脱敏）。
8. **单文件交付**：**每个平台**一个可执行文件 + 一个数据库文件即可运行，包含 Web UI。

---

## 8. 与现有方案的对比

| 方案 | 协议支持 | 动态供应商 | Agent 管理 | Web UI | 部署复杂度 |
| --- | --- | --- | --- | --- | --- |
| CC Switch | ❌ | ❌（需逐 Agent 配置） | ✅ | 桌面端 | 低 |
| LiteLLM | ✅（统一为 OpenAI 格式，Anthropic 入站需转换） | ❌（需为供应商写适配） | ❌ | 基础 | 中 |
| one-api / new-api | ✅（统一为 OpenAI 格式） | ⚠️（预设类型 + 自定义） | ❌ | ✅ | 中 |
| simple-one-api | ✅（三协议） | ⚠️（灵活但需手写配置） | ❌ | ✅ | 低 |
| QueQiao-Router | ✅ | ⚠️ | ❌ | ✅（Rust + Axum） | 中 |
| **AgoraModel（本方案）** | ✅ **双协议原生入站 + 原生透传** | ✅ **完全动态（零模板）** | ✅ **无需适配（Agent 只认网关）** | ✅ | 中（单二进制） |

**与 LiteLLM 的核心差异**：LiteLLM 把一切统一成 OpenAI 格式，原生 Anthropic 客户端（Claude Code）接入时存在额外转换成本与 SSE 事件映射风险；本方案**同时原生支持两种入站协议并原样透传**，无转换、无格式兼容风险、TTFT 更低。

---

## 9. 里程碑与路线

| 阶段 | 内容 | 预估 |
| --- | --- | --- |
| **P0 基础** | Go 1.22+ 安装、仓库骨架、三平台构建矩阵与 CI、mock 上游 | 1 天 |
| **Phase 1 核心透传网关** | 双端点、单网关 Key 鉴权、配置加载、按模型路由、SSE 直接透传、**SSE 心跳保活、context 取消** | 3–5 天 |
| **Phase 2 存储与安全加固** | SQLite + 原子性、AES-256-GCM 凭证加密、SSRF 校验、错误日志表 | 2–3 天 |
| **Phase 3 模型聚合与路由** | 自动拉取、去重、`/v1/models`、`provider/model` 命名空间路由 | 2–3 天 |
| **Phase 4 Web UI** | 供应商管理、模型列表、网关设置、日志页 | 5–7 天 |
| **Phase 5 打包与文档** | 三平台（Windows / Linux / macOS × amd64 / arm64）发布产物、服务化集成、内嵌前端、使用文档、端到端验收 | 2–3 天 |

**总计约 2–3 周（单人）**。前四步（透传 → 保活/取消 → SQLite+加密 → 日志）完成即可日常使用。

---

## 10. 风险与开放问题

| 编号 | 类型 | 内容 | 处置 |
| --- | --- | --- | --- |
| R1 | 假设风险 | 「所有供应商都提供双协议 URL」是用户前提；若某供应商只有单协议，将无法通过对应入站路径访问 | 设计上允许只配置其中一个 URL：未配置的协议路径对该供应商返回明确的 4xx 错误 |
| R2 | 兼容性 | 两种协议下同名模型的 `model` 名称可能不一致（如 OpenAI 端点叫 `gpt-4o`、Anthropic 端点叫别的） | 本期不做映射；通过命名空间路由 + 手动模型维护规避，后续按需加映射表 |
| R3 | 兼容性 | 部分供应商的 OpenAI 兼容路径不是 `/v1/chat/completions` | 提供 `*_endpoint_override` 完整 URL 覆盖 |
| R4 | 运维 | 反向代理默认缓冲 SSE，导致流式失效或被掐断 | 文档明确反代配置；网关侧 SSE 心跳兜底 |
| R5 | 安全 | 网关集中持有全部上游 Key | AES-256-GCM 加密 + 掩码回显 + 主密钥不落库；定位为「防误传/防备份泄露」而非「防主机被攻陷」 |
| R6 | 环境 | 技术栈已定案 **Go 1.22+**（跨平台基线：`CGO_ENABLED=0` + `modernc.org/sqlite`），但开发机（Windows）尚未安装 Go 工具链 | 列为 Phase 0 · T0.1 前置任务：安装 Go 1.22+ 并验证三平台（Windows / Linux / macOS × amd64 / arm64）交叉编译矩阵；Rust + Axum 仅作备选记录（见 DESIGN ADR-001） |
| R7 | 需求 | 「对外只提供一个 Key」与「多 Agent 分权/吊销」存在张力 | v1 单 Key；数据库用 `gateway_keys` 表预留结构，未来零改表升级 |
| R8 | 数据 | 模型列表准确性依赖上游 `/models` 的可靠性 | 支持手动覆盖与排除；拉取失败保留上次结果并标注陈旧 |
| R9 | 兼容性 | 目标 Windows 版本下限已确定为 **Windows 10 / 11**（明确不含 Win7/8 与 Server 2012，也不出 32 位产物） | 已关闭：Go 1.22+ 覆盖 Win10 1607+，无需旧系统兼容分支或退回 Go 1.20 |
| R10 | 代理 | 企业环境常见的 `HTTP_PROXY` 会使出站请求走代理，导致 SSRF 的 IP 校验与实际连接目标不一致 | 默认 `Transport.Proxy = nil`；需要代理时必须显式配置并校验代理地址（DESIGN §9、§12） |
| R11 | 配置 | 误把内网地址（本机 Ollama / 公司内网服务）配为上游，或 Web UI 暴露公网后被他人利用 | SSRF 校验默认拒绝内网段；`allow_internal = true` 显式放行并留痕；Web UI 默认仅监听 `127.0.0.1` |

---

## 附录 A：需求约束的演进（对话收敛记录）

| 轮次 | 用户输入 | 对方案的影响 |
| --- | --- | --- |
| 初始 | 三个硬性要求：不预设供应商模板；兼容 OpenAI 与 Anthropic 两套接口；要有 Web UI | 确立「动态供应商 + 双协议 + Web UI」三大支柱 |
| 第 2 轮 | **不考虑协议的双向转换，我提供的模型供应商都有支持两种协议的 URL** | 删除协议转换层，网关退化为「协议感知的透传路由层」；供应商配置改为双 URL；SSE 直接透传；工期缩短约 40% |
| 第 3 轮 | **1) 一个供应商只设一个凭证，不要轮询/故障转移；2) 对外只提供一个 Base URL 和一个 API Key，可查询聚合后的模型；3) 不做用量统计和 WebDAV 同步** | 删除凭证池/轮询/故障转移；确立单 Key + `/v1/models` 聚合；裁剪 Web UI 为三个核心页 |
| 第 4 轮 | 对抗性审查报告（11 项） | 采纳 P0 修正：SSRF 校验、Key 加密、SSE 心跳、context 取消、错误日志、SQLite 原子写；采纳 P1：`gateway_keys` 表结构预留、`provider/model` 命名空间路由、请求体流式解析；明确拒绝：`/v1/models` 的 Anthropic 内容协商、水平扩展、把单 Key 当安全漏洞 |
| 第 5 轮 | 明确要求「Windows 与 Linux 都能使用」 | 锁定跨平台基线：全量 `CGO_ENABLED=0` + 纯 Go SQLite 驱动 `modernc.org/sqlite` + 前端 `embed` 内嵌；新增「跨平台兼容性设计」（权限/信号/服务化/数据目录/代理/控制台等差异）与构建矩阵；技术栈定案 Go 1.22+ |
| 第 6 轮 | 目标 Windows 下限收窄为 Win10 / Win11；服务化选定 `kardianos/service`；要求顺带产出 macOS 产物 | 目标矩阵扩为 Windows / Linux / macOS × amd64 / arm64（6 份产物）；服务化统一为 `kardianos/service`（Windows SCM / Linux systemd / macOS launchd）；跨平台清单补齐 macOS 特有项（Gatekeeper、launchd、数据目录、大小写敏感性） |

## 附录 B：最终用户使用方式（验收片段）

```bash
# OpenAI 协议 Agent
OPENAI_BASE_URL=http://127.0.0.1:9090/v1
OPENAI_API_KEY=gw-xxxxxxxx

# Anthropic 协议 Agent（Claude Code 等）
ANTHROPIC_BASE_URL=http://127.0.0.1:9090
ANTHROPIC_API_KEY=gw-xxxxxxxx
```

> 新增供应商时，只需在 Web UI 里填一次双 URL 与 Key，所有 Agent 立即生效；新出现的 Agent 只要支持自定义 Base URL，无需任何适配即可接入。
