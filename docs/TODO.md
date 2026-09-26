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

- [ ] **T3.1｜聚合器** ｜ 4h ｜ 依赖 T2.2
  - 启动 + ticker（默认 10min，可配）遍历 `auto_fetch_models` 供应商；每供应商独立 goroutine + 20s 超时；请求 `{openai_base}/models`。
  - **验收**：单供应商失败不影响其他供应商；失败写入 `last_fetch_error`。
- [ ] **T3.2｜model_cache 与 available models** ｜ 3h ｜ 依赖 T3.1
  - 计算 `available = (auto ∪ manual) − excluded`；事务内替换自动项；内存视图随快照更新。
  - **验收**：manual/excluded 单测；拉取失败时保留旧缓存并标记陈旧。
- [ ] **T3.3｜`GET /v1/models`** ｜ 3h ｜ 依赖 T3.2
  - 裸名（`owned_by` = priority 最小）+ `provider/model` 全量；`Cache-Control: no-store`；认证同 §5.2。
  - **验收**：新增/停用供应商后列表随之变化；命名空间项数量 = 支持该模型的供应商数。
- [ ] **T3.4｜`provider/model` 命名空间路由** ｜ 3h ｜ 依赖 T3.2、T1.6
  - 仅当第一段命中已存在 provider id 才解析；转发前**最小改写** `model` 键（`map[string]json.RawMessage`）。
  - **验收**：`p/gpt-4o` 走 p 且上游收到 `gpt-4o`；`meta-llama/Llama-3-70B` 不被误判（对应 DESIGN §7.2/§7.3）。
- [ ] **T3.5｜extra_body 合并** ｜ 2h ｜ 依赖 T3.4
  - 仅当配置了 `extra_body` 时走 `map[string]any` 浅合并路径（供应商值覆盖同名键）。
  - **验收**：合并结果符合预期，未配置时零改写。
- [ ] **T3.6｜连接测试（双协议）** ｜ 4h ｜ 依赖 T2.3、T3.1
  - OpenAI：`GET /models` → 失败退化为 `max_tokens=1` 探测；Anthropic：`POST /messages` with `max_tokens=1`。
  - 返回 `{ok, status_code, latency_ms, message}`；未配置的协议单独返回原因；测试请求不入 `logs` 表。
  - **验收**：错误 Key → `ok:false` + 401 提示；正确配置 → 双协议均 `ok:true`。
- [ ] **T3.7｜实现顺序复核 / 回归** ｜ 2h ｜ 依赖 T3.1–T3.6
  - 回归 Phase 1 全部验收用例，确认聚合引入后透传仍字节保真。

---

## Phase 4 · Web UI（5–7 天）

- [ ] **T4.1｜前端脚手架** ｜ 3h ｜ 依赖 T0.3
  - Vite + React 19 + Tailwind + shadcn/ui + `@tanstack/react-query` + 路由。
  - **验收**：`npm run dev` 可跑，构建产物可被 `embed`。
- [ ] **T4.2｜API 客户端与统一错误处理** ｜ 3h ｜ 依赖 T4.1
  - 封装 `fetch`：401 跳转登录、错误 toast、加载态、类型定义（`types.ts`）。
  - **验收**：后端停掉时页面给出可读错误而非白屏。
- [ ] **T4.3｜供应商管理页** ｜ 6h ｜ 依赖 T4.2
  - 列表列：名称/双 URL/模型数/优先级/状态/最近抓取/操作；表单字段与校验按 DESIGN §8.2（含「Key 留空表示不修改」）。
  - **验收**：新建供应商（仅填名称+双URL+Key）成功；两者 URL 皆空时阻止保存。
- [ ] **T4.4｜测试连接 / 拉取模型交互** ｜ 3h ｜ 依赖 T4.3、T3.6
  - 展示双协议结果（状态码/耗时/错误摘要）；「拉取模型」后模型数更新。
  - **验收**：错误 Key 场景有明确可操作提示。
- [ ] **T4.5｜模型列表页** ｜ 4h ｜ 依赖 T4.2、T3.3
  - 表格 + 「显示命名空间形式」开关 + 手动添加/排除 + 全局刷新 + 空状态引导。
  - **验收**：手动添加的模型立刻可被路由到（对应 DESIGN §8.3）。
- [ ] **T4.6｜网关设置页** ｜ 3h ｜ 依赖 T4.2、T2.4
  - Base URL 与 Key 掩码展示、一键复制 OpenAI/Anthropic 环境变量片段；Key 重置（明文仅显示一次 + 「我已保存」确认）；端口/刷新间隔修改（需重启时明确提示）。
  - **验收**：复制片段可直接粘贴进 Agent 配置并生效（PRD 场景 S2）。
- [ ] **T4.7｜日志页** ｜ 4h ｜ 依赖 T4.2、T2.6
  - 筛选（时间/状态码/模型/供应商/仅失败）、分页、错误摘要展开。
  - **验收**：能定位「Agent 发错」与「上游拒绝」两类问题。
- [ ] **T4.8｜登录与会话（远程模式）** ｜ 3h ｜ 依赖 T4.2、T2.4
  - 管理密码登录、HttpOnly + SameSite Cookie、登录限速、登出。
  - **验收**：监听 `0.0.0.0` 时未登录不能访问 `/api/*` 与页面。
- [ ] **T4.9｜embed 集成与 SPA fallback** ｜ 3h ｜ 依赖 T4.1
  - 静态资源内嵌、前端路由 fallback、静态资源缓存头。
  - **验收**：仅启动后端进程即可在浏览器完成全部操作（零安装体验）。

---

## Phase 5 · 打包、文档与验收（2–3 天）

- [ ] **T5.1｜三平台发布产物** ｜ 3h ｜ 依赖 T4.9
  - 产出 6 份单文件：`windows/{amd64,arm64}` + `linux/{amd64,arm64}` + `darwin/{amd64,arm64}`（`CGO_ENABLED=0`、静态链接、版本号注入）；核对 `modernc.org/libc` 与驱动 `go.mod` 声明的版本一致。
  - **验收**：每个产物在对应平台（实机 / VM / WSL2 / CI runner）启动、返回 `/healthz`、完成一次 mock 透传；服务模式验证见 T5.7。
- [ ] **T5.2｜文档** ｜ 4h ｜ 依赖 T5.1
  - `README.md`：快速开始、环境变量表、Agent 配置示例、Nginx 反代片段（`proxy_buffering off`、`proxy_read_timeout 3600s`）、**三平台服务化说明**（Windows SCM / Linux systemd / macOS launchd，含 `install / start / stop` 子命令与手工 systemd unit 备选）、**macOS 首次运行说明**（Gatekeeper 拦截 → `xattr -dr com.apple.quarantine`）、备份与升级说明。
  - **验收**：照文档从零可跑通（干净机器或新目录验证）；macOS 按文档首次运行成功。
- [ ] **T5.3｜端到端验收（PRD §7 八条）** ｜ 4h ｜ 依赖 T5.1
  - 1 零模板接入 → 2 双协议可用 → 3 模型聚合 → 4 长任务不中断 → 5 断连无泄漏 → 6 安全基线 → 7 可排查 → 8 单文件交付。
  - **验收**：逐条留证（命令输出 / 截图 / 日志片段），全部勾选。
- [ ] **T5.4｜性能冒烟** ｜ 2h ｜ 依赖 T5.1
  - 20 并发流式请求；本地回环额外延迟 < 5ms；内存稳定（无 goroutine/buffer 增长）。
  - **验收**：压测后 goroutine 数与 RSS 回落至基线附近。
- [ ] **T5.5｜安全复核** ｜ 2h ｜ 依赖 T5.3
  - 复核 DESIGN §9 表格逐项：默认监听、远程鉴权、SSRF、密文存储、掩码、常量时间比对、逐跳头剥离、限速。
  - **验收**：检查清单全绿；发现项记录为 Backlog。
- [ ] **T5.6｜导出/导入与迁移演练** ｜ 2h ｜ 依赖 T5.1
  - `/api/export`、`/api/import`；演练换机：导出 → 新机导入 → Agent 连接成功。
  - **验收**：导出文件不含明文 Key（对应 PRD 场景 S3）。
- [ ] **T5.7｜服务化集成（`kardianos/service`）** ｜ 3h ｜ 依赖 T5.1
  - 内建 `install / uninstall / start / stop / restart / status` 子命令；`Service.Stop()` 接入优雅关闭（`http.Server.Shutdown` → flush 日志批次 → SQLite checkpoint 与关闭）。
  - 三平台验证：Windows SCM、Linux systemd、macOS launchd；确认服务模式下日志落文件、重启自动恢复、停止时不丢日志。
  - **验收**：三平台均可 `install → start → 压一次 → stop → uninstall` 走通；`stop` 后无 WAL 残留异常、日志完整。

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
- [ ] Windows 托盘/开机自启一键安装包。
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
