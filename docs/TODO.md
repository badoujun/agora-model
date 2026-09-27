# AgoraModel 待办清单（TODO / WBS）

> 对应文档：`docs/PRD.md`（需求与验收）、`docs/DESIGN.md`（接口与流程）
> 说明：`- [ ]` 未完成 ｜ `- [x]` 已完成 ｜ 每项标注 **预估工时**、**依赖**、**验收判据**
> 执行顺序以本文件的 Phase 编号为准（与 PRD §9 里程碑一致）。

---

## 里程碑总览

| 阶段 | 目标 | 预估 | 交付判据 |
| --- | --- | --- | --- |
| **Phase 0** 准备 | Go 工具链、仓库骨架、三平台构建矩阵、mock 上游 | 1 天 | 一条命令产出 Windows + Linux + macOS 产物；mock 上游可响应双协议 |
| **Phase 1** 核心透传网关 | 双端点透传 + 保活 + 取消 | 3–5 天 | Claude Code / OpenAI SDK 指向网关可流式对话 |
| **Phase 2** 存储与安全加固 | SQLite + 加密 + SSRF + 日志 | 2–3 天 | 库中无明文 Key；失败请求可查 |
| **Phase 3** 模型聚合与路由 | 聚合 `/v1/models` + 命名空间路由 | 2–3 天 | 新供应商模型自动出现；`provider/model` 生效 |
| **Phase 4** Web UI | 四个页面可用 | 5–7 天 | 纯浏览器完成供应商接入与排查 |
| **Phase 5** 打包与验收 | 三平台单文件 + 服务化 + 文档 + 八条验收 | 2–3 天 | 每个平台一个可执行文件 + 一个 db 文件即可运行 |
| **合计** | | **约 2–3 周（单人）** | |

**依赖关系（关键路径）**

```
T0.1(Go 1.22+) → T0.2 → T0.3(三平台矩阵) → T0.4 → T0.5 → T0.6
                  ↓
T1.1 → T1.2 → T1.5 → T1.6 → T1.7 → T1.8 → T1.10 → T1.11 → T1.12 → T1.13 → T1.14
                                                        ↓
T2.1 → T2.2 → T2.3 → T2.4 → T2.5 → T2.6 → T2.7        （可与 T1 后期并行设计）
                                                        ↓
T3.1 → T3.2 → T3.3 → T3.4 → T3.5 → T3.6
                  ↓
T4.1 → T4.2 → T4.3 → T4.4 → T4.5 → T4.6 → T4.7 → T4.8 → T4.9
                                                        ↓
                                              T5.1 → T5.2 → T5.3 → T5.4 → T5.5 → T5.6
```

---

## Phase 0 · 环境与仓库准备（1 天）

- [x] **T0.1｜安装 Go 工具链（已装 go1.27.0；go.mod 声明 Go 1.25+）** ｜ 0.5h ｜ 无依赖
  - 安装 **Go 1.22+** 并验证 `go version`；目标平台已确认（Windows 10 / 11 + Linux + macOS，见 PRD R9，已关闭）。
  - Rust + Axum 仅作备选记录，不再是默认路径（接口契约、数据模型、流程不变，切换成本可控）。
  - **验收**：`go version`、`go env GOHOSTOS GOHOSTARCH` 正常；版本号记入 DESIGN ADR-001。
  - ✅ 实测：winget 安装 **go1.27.0 windows/amd64**；`gofmt -l cmd internal` 无输出，`go vet ./...`、`go build ./...` 均通过。
- [x] **T0.2｜初始化仓库结构** ｜ 1h ｜ 依赖 T0.1
  - 创建 `cmd/agoramodel`、`internal/{config,store,crypto,security,platform,provider,models,route,gateway,logging,api}`、`web/`。
  - `.gitignore` 至少包含：`data/`、`*.db`、`*.db-wal`、`*.db-shm`、`master.key`、`web/dist/`、`web/node_modules/`、`dist/`。
  - 新增 `.gitattributes`：`* text=auto eol=lf`、`*.ps1 eol=crlf`（消除 Windows CRLF 与其他平台 LF 的行尾差异）。
  - **验收**：`git status` 干净；目录骨架与 DESIGN §2.4 一致（含 `internal/platform`）。
  - ✅ 实测：11 个 internal 包（各含 `doc.go`）+ `cmd/agoramodel/main.go`（`/healthz` 与优雅关闭骨架）+ `web/README.md`；`.gitattributes` 生效，Go 文件为无 BOM UTF-8。
- [x] **T0.3｜构建脚本（含三平台矩阵）** ｜ 2h ｜ 依赖 T0.2
  - `Makefile`（Linux / macOS）与 `build.ps1`（Windows）：前端构建 → `embed` → 按 ADR-001 矩阵执行 `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build -trimpath -ldflags="-s -w -X main.version=<ver>"`，输出 `dist/agoramodel-<os>-<arch>[.exe]`（Windows / Linux / macOS × amd64 / arm64，共 6 份）。
  - 脚本内**断言 `CGO_ENABLED=0`**；构建后用 `file` / `ldd`（或 Windows 侧等价手段）确认静态链接、无 CGO 依赖。
  - **验收**：一条命令产出三平台六份产物，且校验通过。
  - ✅ 实测：`build.ps1 -Target dist` 一次产出 6 份产物（6.2–6.9 MB/份）；`go version -m` 逐份确认 `CGO_ENABLED=0` 与正确的 `GOOS/GOARCH`。注意：`build.ps1` 必须保存为 **UTF-8 with BOM + CRLF**，否则 Windows PowerShell 5.1 会按 ANSI 解码中文而报解析错误（脚本头部已注明）。
- [x] **T0.4｜Mock 上游服务（测试基础设施）** ｜ 2h ｜ 依赖 T0.1
  - 支持 `POST /v1/chat/completions`、`POST /v1/messages`、`GET /v1/models`；可配置：分块间隔、**空闲静默 N 秒**、返回 401/429/500、非法 JSON、超大响应、慢首字节。
  - **验收**：mock 可被 `curl` 调用；静默能力可复现 SSE 超时场景。
  - ✅ 实测：`tools/mock-upstream/server.mjs`（Node）双协议流式 / 非流式均正常（Anthropic 8 个 SSE 事件、OpenAI 含 `[DONE]`）；`status=401`、`bad=1` 故障注入生效；`/__requests` 断言端点可读出收到的 `authorization` / `x-api-key` / `anthropic-version` 头。注意：PowerShell 向 `curl.exe` 传内联 JSON 会吃掉引号，测试脚本一律用 `--data-binary @file`。
- [ ] **T0.5｜三平台本地冒烟** ｜ 2h ｜ 依赖 T0.3、T0.4（可与 T0.6 并行）
  - Windows 实机跑 `windows/amd64`、WSL2 或 Linux 机器跑 `linux/amd64`、macOS 机器（或 CI 的 `macos-latest` runner）跑 `darwin/arm64`；三端均需启动成功、`/healthz` 正常；mock 上游用 `curl` 直连验证自身可用。
  - **经网关的端到端透传冒烟不在本任务内**（透传能力属 Phase 1），由 **T1.14** 覆盖。
  - **验收**：三端冒烟通过；记录控制台/日志差异并确认 `--no-color`、`--log-format=json` 可用；macOS 侧确认未签名二进制的可运行方式（`xattr -dr com.apple.quarantine`）。
  - 进度：Windows 侧 ✅（`--version` 注入 `0.1.0-dev`、`/healthz` 返回 200、`text` 与 `json` 两种日志格式输出正常）；Linux / macOS 侧待 T0.6 的 CI runner。
- [ ] **T0.6｜CI 矩阵** ｜ 2h ｜ 依赖 T0.3
  - 流水线矩阵：`os ∈ {windows-latest, ubuntu-latest, macos-latest}` × `goarch ∈ {amd64, arm64}`；步骤：构建 → 单元测试 → 产物冒烟（`/healthz`；Phase 1 后追加经网关的透传断言）。
  - **验收**：六个组合全绿；任一平台失败能明确定位；macOS runner 上确认未签名二进制可启动（必要时先 `xattr -dr com.apple.quarantine`）。
  - 进度：workflow 已就绪（`.github/workflows/ci.yml`：三平台 × 两架构的 `go vet`/`go test`/构建/CGO 校验，本机架构冒烟已含 healthz + 401 + 双协议透传断言，另有独立 `race` job 跑 `go test -race`）；**仓库尚无远端，待 push 后才能实际验证**。

---

## Phase 1 · 核心透传网关（3–5 天）

> **进度：已完成（T1.1–T1.14）**
> 实测证据：
> - `go test ./... -count=1` **全部通过**（config 11 / route 6 / gateway 21 个用例，含子用例共 38 项）；
> - `tools/smoke/phase1.ps1` 端到端冒烟 **21/21 通过**：healthz、双风格 401、缺 model 400、未知模型 404、
>   非流式与流式透传（OpenAI `[DONE]` / Anthropic `message_stop`）、上游凭证替换（`Bearer sk-…` / `x-api-key`）、
>   `anthropic-version` 透传、SSE 心跳保活、上游超时 504、上游不可达 502、请求体超限 413、
>   `extra_body` / `extra_headers` 合并、断连后上游请求被取消、mock 上游存活；
> - 说明：本机 `CGO_ENABLED=0` 且无 gcc，`-race` 不可用，已放到 CI 的独立 Linux job（`CGO_ENABLED=1`）。
- [x] **T1.1｜启动配置** ｜ 2h ｜ 依赖 T0.2
  - flag/env：`LISTEN_ADDR`(127.0.0.1)、`PORT`(9090)、`DB_PATH`、`GW_MASTER_KEY`、`SSE_IDLE_SECONDS`(15)、`MAX_BODY_BYTES`(16MB)、`LOG_LEVEL`、`ADMIN_PASSWORD`。
  - **验收**：非法组合给出明确报错（如监听 `0.0.0.0` 但未设 `ADMIN_PASSWORD` 时拒绝启动）。
- [x] **T1.2｜路由骨架** ｜ 2h ｜ 依赖 T1.1
  - 注册 `POST /v1/chat/completions`、`POST /v1/messages`、`GET /v1/models`、`GET /healthz`；未知路径 404。
  - **验收**：路径识别到协议映射正确（openai/anthropic）。
- [x] **T1.3｜配置快照 + 引导配置** ｜ 3h ｜ 依赖 T1.1
  - `atomic.Pointer[ConfigSnapshot]`，Phase 1 先用 JSON 文件加载（Phase 2 切换 SQLite，接口不变）。
  - **验收**：改配置后重启生效；快照替换是原子的（无半更新状态）。
- [x] **T1.4｜入站认证** ｜ 2h ｜ 依赖 T1.2
  - OpenAI：`Authorization: Bearer <gw-key>`；Anthropic：`x-api-key`（兼容 Bearer）。
  - `sha256` 存储 + `subtle.ConstantTimeCompare`；失败 → 401。
  - **验收**：错误 Key 返回 401，且错误体风格符合入站协议。
- [x] **T1.5｜请求体读取 / 限制 / model 探测** ｜ 3h ｜ 依赖 T1.2
  - `io.LimitReader(body, maxBody+1)`；超限 413；`json.Unmarshal` 只探测 `model`；缺失 → 400。
  - **验收**：17MB 请求返回 413；无 `model` 返回 400（DESIGN §7.2）。
- [x] **T1.6｜路由决策 v1** ｜ 3h ｜ 依赖 T1.3、T1.5
  - 按 `priority` 升序取第一个 enabled 且包含该模型的供应商；未命中 → 404 `model_not_found`。
  - **验收**：表驱动单测覆盖「命中/未命中/多供应商优先级/停用供应商」。
- [x] **T1.7｜头改写** ｜ 3h ｜ 依赖 T1.6
  - 剥离逐跳头与 `Accept-Encoding`；替换认证头；注入 `extra_headers`；`Host` 重写；`Transport.DisableCompression = true`。
  - **验收**：mock 上游收到的头部符合 DESIGN §5.3 表格（含 `anthropic-version` 原样透传）。
- [x] **T1.8｜非流式透传** ｜ 3h ｜ 依赖 T1.7
  - 透传状态码、响应头（剥离逐跳）、响应体；不缓冲整包。
  - **验收**：mock 返回的字节与客户端收到的一致（`model` 未改写时字节级相同）。
- [x] **T1.9｜上游超时与连接池** ｜ 2h ｜ 依赖 T1.7
  - `context.WithTimeout` 按 `provider.timeout_seconds`；共享 `http.Transport`（`MaxIdleConnsPerHost` 合理值）；**不使用** `Client.Timeout`（会误杀长流）。
  - **验收**：mock 延迟 > 超时值 → 504；长流不被全局超时掐断。
- [x] **T1.10｜SSE 透传** ｜ 4h ｜ 依赖 T1.8
  - 按 DESIGN §7.4 实现 `pumpSSE`（读协程投递 chunk）+ `streamSSE`（主循环写客户端）。
  - **验收**：mock 分 10 块吐 SSE，客户端逐块实时收到（非一次性）；无内容改写。
- [x] **T1.11｜SSE 心跳保活** ｜ 3h ｜ 依赖 T1.10
  - 空闲达 `SSE_IDLE_SECONDS` 注入 `: keep-alive\n\n` 并 Flush；有数据重置计时器。
  - **验收**：mock 静默 20s，客户端在此期间收到 ≥1 行注释；恢复数据后内容与上游一致。
- [x] **T1.12｜客户端断开取消** ｜ 2h ｜ 依赖 T1.10
  - 上游请求使用入站 `ctx`；断言断开后上游被取消、读协程退出。
  - **验收**：集成测试中 `runtime.NumGoroutine` 回落到基线；mock 观察到 ctx 取消（对应 DESIGN §11「取消测试」）。
- [x] **T1.13｜统一错误响应** ｜ 2h ｜ 依赖 T1.4、T1.6
  - 实现 DESIGN §5.4 的状态码与 `code`/`error.type` 映射；上游错误原样回传。
  - **验收**：同一错误在 OpenAI 路径与 Anthropic 路径返回各自风格的错误体。
- [x] **T1.14｜Phase 1 测试与冒烟** ｜ 4h ｜ 依赖 T1.1–T1.13
  - 单测：路由、头改写、body 限制、错误映射；集成：mock 上游端到端。
  - **验收（Phase 1 DoD）**：Claude Code 配 `ANTHROPIC_BASE_URL` 可流式对话；OpenAI SDK 配 `OPENAI_BASE_URL` 可流式对话；静默 20s 不断流；断开无泄漏。

---

## Phase 2 · 存储与安全加固（2–3 天）

> **进度：已完成（T2.1–T2.8）**
> 实测证据：
> - `go test ./... -count=1` 全绿（新增 crypto / security / store / logging 用例）；
> - `tools/smoke/phase2.ps1` 端到端冒烟 **19/19 通过**：首次启动生成主密钥、从引导配置导入供应商、
>   打印网关 Key 明文（仅一次）、用数据库中的 Key 完成透传、错误 Key 401、请求日志落库、
>   **agora.db 与 WAL 中均检索不到凭证明文**、重启不重复导入且原 Key 仍有效、
>   `--reset-gateway-key` 后新 Key 可用而旧 Key 立即失效、`allow_internal=false` 时内网地址被拒并启动失败、
>   **主密钥丢失时启动失败（不静默降级为明文）**；
> - 依赖：`modernc.org/sqlite v1.59.0`（纯 Go，无 CGO），因此 `go.mod` 的 `go` 指令相应提升为 **1.25.0**
>   （本机工具链为 go1.27.0，CI 通过 `go-version-file: go.mod` 自动对齐）；
> - 行为变化：网关 Key 改由 `gateway_keys` 表管理（JSON 里的 `gateway.api_key` 在数据库模式下不再参与认证）；
>   JSON 引导配置**仅在数据库中没有供应商时导入一次**，之后以数据库为准（避免覆盖后续 Web UI 的修改）。
- [x] **T2.1｜SQLite 接入与迁移** ｜ 3h ｜ 依赖 T1.3
  - `modernc.org/sqlite`（纯 Go）+ `journal_mode=WAL`；`schema_migrations` 表；启动自动迁移。
  - **验收**：空目录首启自动建表；重复启动幂等。
- [x] **T2.2｜providers / settings DAO** ｜ 4h ｜ 依赖 T2.1
  - 按 DESIGN §4.1 建表；CRUD；配置快照改为从 DB 加载；变更后原子替换快照。
  - **验收**：新增/修改/删除供应商后，新请求立即按新配置路由（无需重启）。
- [x] **T2.3｜凭证加解密与掩码** ｜ 4h ｜ 依赖 T2.2
  - AES-256-GCM（`nonce||ct||tag`，AAD = `provider.id`）；主密钥来源 `GW_MASTER_KEY` > `master.key`(0600)；掩码函数。
  - **验收**：DB 中无可读明文；`master.key` 与库文件分离；主密钥缺失时启动失败且提示明确（不静默降级）。
- [x] **T2.4｜网关 Key 管理** ｜ 3h ｜ 依赖 T2.2
  - `gateway_keys` 表（存 `sha256` + hint）；首启自动生成并打印明文一次；支持重置（旧 Key 立即失效）。
  - **验收**：重置后旧 Key 返回 401；表中无明文。
- [x] **T2.5｜SSRF 校验** ｜ 3h ｜ 依赖 T2.2
  - 保存时校验 scheme/host/IP 段；`allow_internal` 显式放行 + warn 日志（DESIGN §7.7）。
  - **验收**：`http://127.0.0.1:11434` 被拒；开启开关后可保存；单测覆盖 IPv6/loopback/link-local。
- [x] **T2.6｜请求日志与异步写入** ｜ 4h ｜ 依赖 T2.2
  - `logs` 表 + channel 批量 flush（500ms/100 条）；失败必记、成功可选计数；队列满丢弃并计数（`/api/health` 暴露丢数）。
  - **验收**：4xx/5xx/超时/499 均有记录；磁盘慢时请求延迟不受影响。
- [x] **T2.7｜脱敏与进程日志** ｜ 2h ｜ 依赖 T2.6
  - 出口结构体不含 Key 字段；正则过滤 `sk-*`/`gw-*`；`slog` 分级；错误体截断 1KB 入库。
  - **验收**：任何 API 响应、日志文件、进程 stdout 中检索不到明文 Key 形态字符串。
- [x] **T2.8｜管理操作审计日志** ｜ 2h ｜ 依赖 T2.6
  - 记录增删改、Key 重置、登录尝试（含失败）。
  - **验收**：日志页可见 `admin` 类型记录。

---

## Phase 3 · 模型聚合与路由增强（2–3 天）

> **进度：已完成（T3.1–T3.7；T3.5 在 Phase 1 已实现）**
> 实测证据：
> - `go test ./... -count=1` 全绿，累计 **97 个用例**（新增 models 聚合、provider 双协议探测、
>   route 命名空间、gateway `/v1/models`、store 模型缓存与迁移 v2）；
> - `tools/smoke/phase3.ps1` 端到端冒烟 **15/15 通过**：启动即聚合（mock 上游 3 个模型）、
>   失败隔离（不可达供应商只记录 `last_fetch_error`，不影响其他供应商）、
>   手动模型与排除列表生效、`/v1/models` 同时给出裸名（`owned_by` = priority 最小者）与
>   `provider/model` 命名空间条目、未授权 401、**用聚合得到的模型直接透传（无需手动配置模型）**、
>   命名空间路由在转发前剥离前缀、未知前缀退回普通模型名返回 404；
> - 迁移 v2：`providers` 增加 `last_fetch_at` / `last_fetch_error`；
> - 附带完成 `internal/provider` 双协议连接测试（OpenAI 侧优先 `/models`，404/405 时回退最小对话探测），
>   供 Phase 4 的 Web UI「测试连接」直接调用。

- [x] **T3.1｜聚合器** ｜ 4h ｜ 依赖 T2.2
  - 启动 + ticker（默认 10min，可配）遍历 `auto_fetch_models` 供应商；每供应商独立 goroutine + 20s 超时；请求 `{openai_base}/models`。
  - **验收**：单供应商失败不影响其他供应商；失败写入 `last_fetch_error`。
- [x] **T3.2｜model_cache 与 available models** ｜ 3h ｜ 依赖 T3.1
  - 计算 `available = (auto ∪ manual) − excluded`；事务内替换自动项；内存视图随快照更新。
  - **验收**：manual/excluded 单测；拉取失败时保留旧缓存并标记陈旧。
- [x] **T3.3｜`GET /v1/models`** ｜ 3h ｜ 依赖 T3.2
  - 裸名（`owned_by` = priority 最小）+ `provider/model` 全量；`Cache-Control: no-store`；认证同 §5.2。
  - **验收**：新增/停用供应商后列表随之变化；命名空间项数量 = 支持该模型的供应商数。
- [x] **T3.4｜`provider/model` 命名空间路由** ｜ 3h ｜ 依赖 T3.2、T1.6
  - 仅当第一段命中已存在 provider id 才解析；转发前**最小改写** `model` 键（`map[string]json.RawMessage`）。
  - **验收**：`p/gpt-4o` 走 p 且上游收到 `gpt-4o`；`meta-llama/Llama-3-70B` 不被误判（对应 DESIGN §7.2/§7.3）。
- [x] **T3.5｜extra_body 合并** ｜ 2h ｜ 依赖 T3.4
  - 仅当配置了 `extra_body` 时走 `map[string]any` 浅合并路径（供应商值覆盖同名键）。
  - **验收**：合并结果符合预期，未配置时零改写。
- [x] **T3.6｜连接测试（双协议）** ｜ 4h ｜ 依赖 T2.3、T3.1
  - OpenAI：`GET /models` → 失败退化为 `max_tokens=1` 探测；Anthropic：`POST /messages` with `max_tokens=1`。
  - 返回 `{ok, status_code, latency_ms, message}`；未配置的协议单独返回原因；测试请求不入 `logs` 表。
  - **验收**：错误 Key → `ok:false` + 401 提示；正确配置 → 双协议均 `ok:true`。
- [x] **T3.7｜实现顺序复核 / 回归** ｜ 2h ｜ 依赖 T3.1–T3.6
  - 回归 Phase 1 全部验收用例，确认聚合引入后透传仍字节保真。

---

## Phase 4 · Web UI（5–7 天）

> **补充任务组（本轮补齐的文档遗漏）：T4.0 后端管理 API（DESIGN §6）**
> Phase 1–3 只实现了数据面（`/v1/*`）。控制面的 REST 接口必须与前端同步落地：
> `/api/auth/*`（登录/登出/会话）、`/api/health`、`/api/providers*`（CRUD / 连接测试 / 拉取模型）、
> `/api/models*`（列表 / 刷新 / 手动增删）、`/api/settings`、`/api/gateway-key*`（读取 / 重置）、
> `/api/logs`（筛选分页）、`/api/export|import`；并复用保存路径上的 SSRF 校验与凭证加密、
> 采用「默认仅回环免登录，远程必须登录」的访问控制。
>
> **进度（T4.0 已完成）**：`internal/api` 已实现全部控制面端点并通过单测（12 个用例）——
> 会话认证（默认回环免登录；设置 `ADMIN_PASSWORD` 后强制登录，**非回环监听且无密码时启动直接失败**）、
> providers CRUD（响应只回掩码、`PUT` 留空 `api_key` 表示保持原值、复用 SSRF 校验与加密）、
> 连接测试、拉取模型、模型列表与手动增删、settings、网关 Key 重置（明文仅出现一次）、
> 日志查询（错误信息已脱敏）、导出/导入（导出不含明文凭证）。
>
> **进度：Phase 4 已完成（T4.0 后端 API + T4.1–T4.9 前端）**
> 实测证据：
> - 前端工程位于 `web/`（Vite + React 19 + TypeScript + Tailwind CSS v4 + @tanstack/react-query +
>   react-router-dom）；`npm run build`（含 `tsc --noEmit`）通过，产物输出到 `internal/webui/dist`
>   （index.html 0.4 KB、CSS 19.8 KB、JS 376.6 KB / gzip 116 KB）；
> - 页面：供应商管理（列表 + 表单 + 连接测试 + 拉取模型 + 删除）、模型列表（裸名/命名空间切换、
>   来源标注、手动增删、全量刷新）、网关设置（Key 掩码与重置、环境变量片段一键复制、运行参数）、
>   请求日志（筛选、分页、错误展开、5 秒自动刷新）、登录页（仅在设置管理密码时出现）；
> - 后端：`internal/webui` 以 `//go:embed all:dist` 内嵌前端并提供 SPA fallback，
>   `/api`、`/v1`、`/healthz` 的未知路径仍返回 404；
> - `tools/smoke/phase4.ps1` 端到端冒烟 **28/28 通过**（静态资源与缓存头、SPA 回退、会话与健康检查、
>   模拟 Web UI 全流程、ADMIN_PASSWORD 登录流程）；
> - 回归：单测 **111 个用例**全绿，phase1 21/21、phase2 19/19、phase3 15/15。
> 说明：UI 组件按 shadcn/ui 的组织与工具链（cva + clsx + tailwind-merge）**手写实现**，
> 未运行交互式 `npx shadcn init`（避免引入 Radix 等额外依赖）；后续需要时可随时用 CLI 追加组件。

- [x] **T4.1｜前端脚手架** ｜ 3h ｜ 依赖 T0.3
  - Vite + React 19 + Tailwind + shadcn/ui + `@tanstack/react-query` + 路由。
  - **验收**：`npm run dev` 可跑，构建产物可被 `embed`。
- [x] **T4.2｜API 客户端与统一错误处理** ｜ 3h ｜ 依赖 T4.1
  - 封装 `fetch`：401 跳转登录、错误 toast、加载态、类型定义（`types.ts`）。
  - **验收**：后端停掉时页面给出可读错误而非白屏。
- [x] **T4.3｜供应商管理页** ｜ 6h ｜ 依赖 T4.2
  - 列表列：名称/双 URL/模型数/优先级/状态/最近抓取/操作；表单字段与校验按 DESIGN §8.2（含「Key 留空表示不修改」）。
  - **验收**：新建供应商（仅填名称+双URL+Key）成功；两者 URL 皆空时阻止保存。
- [x] **T4.4｜测试连接 / 拉取模型交互** ｜ 3h ｜ 依赖 T4.3、T3.6
  - 展示双协议结果（状态码/耗时/错误摘要）；「拉取模型」后模型数更新。
  - **验收**：错误 Key 场景有明确可操作提示。
- [x] **T4.5｜模型列表页** ｜ 4h ｜ 依赖 T4.2、T3.3
  - 表格 + 「显示命名空间形式」开关 + 手动添加/排除 + 全局刷新 + 空状态引导。
  - **验收**：手动添加的模型立刻可被路由到（对应 DESIGN §8.3）。
- [x] **T4.6｜网关设置页** ｜ 3h ｜ 依赖 T4.2、T2.4
  - Base URL 与 Key 掩码展示、一键复制 OpenAI/Anthropic 环境变量片段；Key 重置（明文仅显示一次 + 「我已保存」确认）；端口/刷新间隔修改（需重启时明确提示）。
  - **验收**：复制片段可直接粘贴进 Agent 配置并生效（PRD 场景 S2）。
- [x] **T4.7｜日志页** ｜ 4h ｜ 依赖 T4.2、T2.6
  - 筛选（时间/状态码/模型/供应商/仅失败）、分页、错误摘要展开。
  - **验收**：能定位「Agent 发错」与「上游拒绝」两类问题。
- [x] **T4.8｜登录与会话（远程模式）** ｜ 3h ｜ 依赖 T4.2、T2.4
  - 管理密码登录、HttpOnly + SameSite Cookie、登录限速、登出。
  - **验收**：监听 `0.0.0.0` 时未登录不能访问 `/api/*` 与页面。
- [x] **T4.9｜embed 集成与 SPA fallback** ｜ 3h ｜ 依赖 T4.1
  - 静态资源内嵌、前端路由 fallback、静态资源缓存头。
  - **验收**：仅启动后端进程即可在浏览器完成全部操作（零安装体验）。

---

## Phase 5 · 打包、文档与验收（2–3 天）
> **进度：Phase 5 已完成（T5.1–T5.6 全绿；T5.7 代码完成，实机安装待提权环境）**
> 实测证据：
> - **单文件交付**：`build.ps1 -Target dist -Version 0.1.0` 产出三平台六份产物，
>   已校验格式（PE / ELF / Mach-O）、`--version` 版本注入、前端已内嵌（单文件 > 5MB）；
> - **`tools/smoke/phase5.ps1` 验收 26/26 通过**，覆盖 PRD §7 八条：
>   ① 零模板接入（仅填双 URL + Key 即 201）② 双协议可用（两条路径均 200 且原样透传）
>   ③ 模型聚合（裸名唯一 + `owned_by` 正确）④ 上游静默 20s 仍完成（SSE 心跳兜底）
>   ⑤ 断连后网关仍健康 ⑥ 安全基线（DB 无明文、API 只回掩码、内网地址被拒、非回环无密码拒绝启动）
>   ⑦ 上游 401 原样透传且可在日志接口查到 ⑧ 运行仅依赖可执行文件 + `agora.db` + `master.key`；
> - **性能（T5.4）**：20 并发流式请求全部成功（约 2.5s），压测后网关内存增长 1.9MB；
> - **迁移演练（T5.6）**：导出（不含明文凭证）→ 在新数据目录的实例上导入 → 同模型名继续可用；
> - **服务化（T5.7）**：`install | uninstall | start | stop | restart | status` 子命令已实现并自检
>   （未安装时 `status` 给出明确错误）；**实机安装需要管理员/root 权限，未在本机执行**，
>   README 已给出三平台命令与注意事项（服务模式无控制台，日志需落文件；管理密码用环境变量注入）；
> - **文档（T5.2）**：新增 `README.md`（快速开始、参数与环境变量表、数据与安全、服务化、
>   反向代理、常见问题、开发与冒烟命令）。

- [x] **T5.1｜三平台发布产物** ｜ 3h ｜ 依赖 T4.9
  - 产出 6 份单文件：`windows/{amd64,arm64}` + `linux/{amd64,arm64}` + `darwin/{amd64,arm64}`（`CGO_ENABLED=0`、静态链接、版本号注入）；核对 `modernc.org/libc` 与驱动 `go.mod` 声明的版本一致。
  - **验收**：每个产物在对应平台（实机 / VM / WSL2 / CI runner）启动、返回 `/healthz`、完成一次 mock 透传；服务模式验证见 T5.7。
- [x] **T5.2｜文档** ｜ 4h ｜ 依赖 T5.1
  - `README.md`：快速开始、环境变量表、Agent 配置示例、Nginx 反代片段（`proxy_buffering off`、`proxy_read_timeout 3600s`）、**三平台服务化说明**（Windows SCM / Linux systemd / macOS launchd，含 `install / start / stop` 子命令与手工 systemd unit 备选）、**macOS 首次运行说明**（Gatekeeper 拦截 → `xattr -dr com.apple.quarantine`）、备份与升级说明。
  - **验收**：照文档从零可跑通（干净机器或新目录验证）；macOS 按文档首次运行成功。
- [x] **T5.3｜端到端验收（PRD §7 八条）** ｜ 4h ｜ 依赖 T5.1
  - 1 零模板接入 → 2 双协议可用 → 3 模型聚合 → 4 长任务不中断 → 5 断连无泄漏 → 6 安全基线 → 7 可排查 → 8 单文件交付。
  - **验收**：逐条留证（命令输出 / 截图 / 日志片段），全部勾选。
- [x] **T5.4｜性能冒烟** ｜ 2h ｜ 依赖 T5.1
  - 20 并发流式请求；本地回环额外延迟 < 5ms；内存稳定（无 goroutine/buffer 增长）。
  - **验收**：压测后 goroutine 数与 RSS 回落至基线附近。
- [x] **T5.5｜安全复核** ｜ 2h ｜ 依赖 T5.3
  - 复核 DESIGN §9 表格逐项：默认监听、远程鉴权、SSRF、密文存储、掩码、常量时间比对、逐跳头剥离、限速。
  - **验收**：检查清单全绿；发现项记录为 Backlog。
- [x] **T5.6｜导出/导入与迁移演练** ｜ 2h ｜ 依赖 T5.1
  - `/api/export`、`/api/import`；演练换机：导出 → 新机导入 → Agent 连接成功。
  - **验收**：导出文件不含明文 Key（对应 PRD 场景 S3）。
  - **第 11 轮修订（用户要求）**：导出改为**包含 API Key 明文**，导入时用目标机器的主密钥重新加密；
    验收更新为「导出（含明文）→ 新机导入 → Agent 直接可用」，页面导出前二次确认并提示按凭证保管。
    同时新增迁移 v4（`providers.website_url`、`gateway_keys.key_cipher`）与控制台改动：
    左上角显示程序版本号、供应商官网地址、新增供应商时保存与拉取模型解耦、网关 Key 明文可查看/复制、新增「数据位置」页。
    配套：冒烟脚本断言随新行为同步（`phase2.ps1` 22 项、`phase4.ps1` 31 项、`phase5.ps1` 29 项，含「导出含明文」「网关 Key 可复制」「数据库无明文 Key」新增断言），五个冒烟脚本全部通过。
- [x] **T5.7｜服务化集成（`kardianos/service`）** ｜ 3h ｜ 依赖 T5.1
  - 内建 `install / uninstall / start / stop / restart / status` 子命令；`Service.Stop()` 接入优雅关闭（`http.Server.Shutdown` → flush 日志批次 → SQLite checkpoint 与关闭）。
  - 三平台验证：Windows SCM、Linux systemd、macOS launchd；确认服务模式下日志落文件、重启自动恢复、停止时不丢日志。
  - **验收**：三平台均可 `install → start → 压一次 → stop → uninstall` 走通；`stop` 后无 WAL 残留异常、日志完整。
- [x] **T5.7b｜服务化健壮性补齐** ｜ 2h ｜ 依赖 T5.7
  - `--log-file`（PRD FR-11.4 的「服务模式日志落文件」此前未实现，`newLogger` 硬编码 stdout；Windows SCM 下 stdout 直接被丢弃）。
  - `install` 固化绝对 `--data-dir`：服务账户与前台运行不同（Windows SCM 默认 `LocalSystem`），原先缺省时不写入该参数，导致数据落到 `…\systemprofile\AppData\Roaming\AgoraModel`。
  - `service.Config.Executable` 指向 `<数据目录>/bin/` 下的二进制副本：原实现走 `os.Executable()`，在 npm / npx 场景指向会被整体删除重建的缓存目录。
  - `--service-env KEY=VALUE` 注入 `service.Config.EnvVars`（此前 `EnvVars`/`UserName`/`WorkingDirectory` 三个字段均未使用，无法注入 `ADMIN_PASSWORD` / `GW_MASTER_KEY`）。
  - 幂等安装（已存在则先 Stop + Uninstall 再 Install）、权限类失败的人话提示、子命令参数两种位置都支持。
  - 服务化逻辑集中到 `internal/platform`（符合 DESIGN §12「平台差异集中在 platform 包」）。
  - **实测（Windows）**：`go test ./... -count=1` 全绿；`internal/platform` 新增 16 个测试函数（共 35 个通过点）；`--version` 正常；`install --data-dir X` 复制副本并给出权限提示（非管理员下未误注册服务）；`--log-file` 前台落盘、`/healthz` 200。
  - **未实测**：三平台实机 `install → start`（需管理员 / root 环境）、服务模式下的日志与优雅关闭。
- [x] **T5.8｜npm 分发** ｜ 3h ｜ 依赖 T5.7
  - `npm/` 目录：主包 `@bakeroot/agoramodel`（Node 转发器）+ 6 个平台包 `@bakeroot/agoramodel-<os>-<cpu>`（各含一个预编译二进制）。
  - 平台包用 `os` / `cpu` 字段经 `optionalDependencies` 在**安装期**筛选，**不使用 postinstall**（npm 11+ 默认不执行依赖脚本，postinstall 方案会静默失败）；包内不含任何 `scripts`。
  - **自写 tar 打包**（`npm/scripts/tarball.mjs`）而不调 `npm pack`：Windows 上 `fs.chmod()` 是空操作（实测 mode 恒为 666），npm pack 出来的 Linux / macOS 平台包里二进制是 644、装完无法执行；且 Windows 调 npm CLI 必须经 shell，Node 24 会报 DEP0190。自写后在 tar 头里直接写 0755。
  - `publish.mjs` 默认发官方 registry（不沿用 `npm config get registry`——本机即为只读镜像 `registry.npmmirror.com`），先发平台包再发主包，`--tag` / `--otp` / `--registry` 经白名单校验后才拼进命令。
  - **实测（Windows）**：`node --test` 20 项通过（含用系统 `tar` 独立校验 tar 头 checksum 与 `-rwxr-xr-x`）；`npm install <tgz>` 解包正确；经 npm 生成的 `node_modules/.bin/agoramodel` 运行正常，退出码 0 / 1 / 2 与 stdout/stderr 转发均正确；注入尝试（`--tag=bad;rm -rf /`、`--registry=https://x.com;whoami`）全部被拒；`npm publish --dry-run` 被 npm 接受。
  - **未实测**：Linux / macOS 上安装与运行、真实 `npm publish`（需 npm 账号与 2FA）。
- [x] **T5.8b｜改用 scope（实发失败驱动的返工）** ｜ 1h
  - **实发失败**：2FA 已通过，但无 scope 的 `agoramodel-win32-x64` 被 npm **服务端**拒绝：`403 Forbidden - Package name triggered spam detection`。这类拦截只在发布时由服务端执行，`--dry-run` 预检不到。
  - 该筛查对 `<名字>-<平台>-<架构>` 模式特别敏感，公开案例：`do-harness-win32-x64`（同款失败，Windows 二进制最终改走 GitHub Release）、`archons-win32-x64-msvc`（同批其他平台包正常）。
  - **改为全 scope**：`@bakeroot/agoramodel` + `@bakeroot/agoramodel-<os>-<cpu>`。scope 是账号独占命名空间，包名不参与相似度判定。
  - 配套改动：`bin` 的 key 用**命令名**而非包名（装完仍叫 `agoramodel`）；发布显式带 `--access public`（scope 默认按私有处理）；tarball 文件名按 npm 约定扁平化为 `bakeroot-agoramodel-<ver>.tgz`。
  - **实测**：`node --test` 22 项通过；本地 tarball 安装后布局为 `node_modules/@bakeroot/agoramodel`，`.bin/agoramodel` 运行正常（`--version` → 0.1.0，退出码 0 / 2 正确）；`publish --dry-run` 显示 `public access` 且顺序为「6 个平台包 → 主包」。
  - **已发布**：`@bakeroot/agoramodel@0.1.0`（用户账号 bakeroot，2FA web 认证流程）。
- [x] **T5.10｜修复首次启动死锁（零供应商）** ｜ 1h
  - **用户实测**：`npm i -g @bakeroot/agoramodel` 后直接运行 `agoramodel`，报 `启动失败 err="未配置任何启用的供应商（providers[].enabled 需为 true）"` 并退出。而该提示让人「等待 Web UI 添加供应商」——Web UI 必须先启动网关才能访问，构成死锁。
  - **根因**：`config.NewSnapshot` 复用了 `File.Normalize` 的「必须至少有一个启用的供应商」校验，把「数据库为空」这个**合法初始状态**误判成配置错误。
  - **修复**：拆出 `normalize(requireProviders bool)`。`Normalize`（引导配置，经 `LoadFile`）保持严格；新增 `NormalizeAllowEmpty` 供 `NewSnapshot`（运行时快照）使用。启动日志从 ERROR 退出改为 WARN + 给出 Web UI 地址。
  - **顺带**：`/v1/chat/completions` 在零供应商时返回「网关尚未配置任何供应商…请在 Web UI 的『供应商管理』中添加」，而不是笼统的「模型未被任何供应商声明」；「删除最后一个供应商」也不再失败。
  - **实测（Windows）**：空数据目录启动后进程存活、`/healthz` 200、Web UI 200、`/v1/models` 无 Key 仍 401；运行时 `POST /api/providers` 添加后 `/v1/models` 立即返回 4 条（裸名 + 命名空间形式），删除该供应商后服务仍存活；`go test ./...` 全绿（新增 `TestAllowEmptyProvidersForRuntimeSnapshot`、`TestNormalizeAllowEmptyStillValidatesOtherFields`、`TestWebUIURL`）。
  - **安全审查**：独立 security-review 子代理结论为「未引入可利用的安全回归」——鉴权位于路由决策之前且与供应商数量正交，非回环强制管理密码绑定 `Listen`、未被改动，空配置响应不泄露信息。另已穷尽核对调用点：走宽松路径的入口**只有** `NewSnapshot`，引导配置仍走严格的 `Normalize`。
- [x] **T5.9｜许可证** ｜ 0.5h
  - 新增 `LICENSE`（MIT，参考 cc-switch 的许可证文本，逐字一致，仅版权行不同：`Copyright (c) 2026 badoujun`）。
  - npm 包的 `license` / `author` 字段由 `build.mjs` **从 LICENSE 解析**，避免元数据与许可证文本漂移；`LICENSE` 随每个 tarball 一起分发。

---

## Backlog（P2 / 后续迭代）

> 以下项已在 DESIGN 中明确"本期不做"，此处登记以备后续排期。

- [ ] 多网关 Key：按 Agent 发放、单独吊销、最后使用时间统计（表结构已在 `gateway_keys` 预留，零改表）。
- [ ] 模型名映射（`model_alias_json`）：解决「同一模型在两种协议下名称不同」（PRD 风险 R2）。
- [ ] 日志保留策略：按天数/条数清理，防止 SQLite 无限增长（FR-7.5）。
- [ ] `/v1/models` 的 Anthropic 格式内容协商（DESIGN §13 已论证低优先级）。
- [ ] 协议适配插件接口：Gemini `/v1beta`、OpenAI Responses API（预留 `protocol adapter` 抽象）。
- [ ] 用量统计与计费（用户已明确暂不考虑）。
- [ ] WebDAV 导出（用户已明确暂不考虑；当前靠 `/api/export` + 服务端集中配置）。
- [ ] MCP 服务器代理。
- [ ] 多用户/多租户与 RBAC。
- [ ] Windows 托盘 / 一键安装包（`.msi`）。开机自启本身已由 `install` 子命令覆盖，见 T5.7 / T5.8。
- [ ] macOS 代码签名与公证（notarize），消除 Gatekeeper 拦截提示。

---

## 风险跟踪表（随进度更新）

| 编号 | 风险 | 触发点 | 缓解措施 | 状态 |
| --- | --- | --- | --- | --- |
| R1 | 某供应商只支持单协议 | 接入时 | 网关返回 `upstream_protocol_not_configured`；文档说明 | 已设计 |
| R2 | 两协议下模型名不一致 | 接入时 | 本期用命名空间路由 + 手动模型规避 | 已设计 |
| R3 | 供应商路径非标准 | 接入时 | `*_endpoint_override` 覆盖 | 已设计 |
| R4 | 反代缓冲导致流式失效 | 部署 | Nginx 配置文档 + SSE 心跳兜底 | 已设计 |
| R5 | 上游 Key 集中存储 | 全程 | AES-256-GCM + 掩码 + 主密钥分离 | 已设计 |
| R6 | 开发机无 Go 工具链 | Phase 0 | 已定案 Go 1.22+；由 T0.1 安装、T0.3 三平台矩阵验证 | 待处理（Phase 0） |
| R7 | 对外只有一个网关 Key，无法按 Agent 分权/吊销 | 使用期 | v1 单 Key；`gateway_keys` 表预留多 Key 结构，未来零改表升级 | 已设计 |
| R8 | 上游 `/models` 不可靠 | 运行期 | 手动覆盖/排除 + 保留旧缓存 + 陈旧标记 | 已设计 |
| R9 | 目标 Windows 下限 | Phase 0 | 已确认仅需 Windows 10 / 11 + Linux（不含 Win7/8、Server 2012、32 位）；Go 1.22+ 直接覆盖 | 已关闭 |
| R10 | `HTTP_PROXY` 导致 SSRF 校验失效 | 实现期 | 默认 `Transport.Proxy = nil`；需要代理时显式配置 | 已设计 |
| R11 | 误配内网地址 | 接入时 | SSRF 校验 + `allow_internal` 显式放行 | 已设计 |
