# AgoraModel

> **一个本地优先的 AI 模型接入网关 + 配置控制台**：给所有 AI Agent 一个固定的入口，
> 新增供应商只配置一次，所有 Agent 立即可用。

- 痛点 A（横向）：每出现一个新的 AI Agent，就要在 CC Switch 之类的工具里为它单独配置供应商。
- 痛点 B（纵向）：每接入一个新供应商，就要给**每一个** Agent 重复配置一遍（N×M 重复劳动）。
- AgoraModel 的做法：**把供应商配置与 Agent 配置解耦**。所有 Agent 只对接网关
  （一个 Base URL + 一个 Key），由网关按协议与模型名路由到任意供应商。

```
Agent（Claude Code / Codex / Cursor / 任意支持自定义 Base URL 的工具）
        │  ANTHROPIC_BASE_URL / OPENAI_BASE_URL 指向同一个网关
        ▼
   AgoraModel 网关（单可执行文件）
   ├─ /v1/messages          ← Anthropic 协议，原样透传
   ├─ /v1/chat/completions  ← OpenAI 协议，原样透传
   ├─ /v1/models            ← 聚合后的模型列表（含 provider/model 命名空间）
   └─ Web 控制台            ← 供应商 / 模型 / 设置 / 日志
        │
        ▼
   供应商 A（OpenAI URL + Anthropic URL + 1 个 Key）
   供应商 B（…）  供应商 C（…）
```

## 特性

| 能力 | 说明 |
| --- | --- |
| **零模板接入** | 不预设供应商类型：填「名称 + 两个协议地址 + API Key」即可，无需选择模板或改代码 |
| **双协议原生透传** | 同时对外提供 OpenAI 与 Anthropic 两套接口，**不做格式转换**，流式响应逐字节透传 |
| **模型聚合** | 定时从各供应商 `/models` 拉取并聚合，`/v1/models` 同时给出裸名与 `provider/model` |
| **显式路由** | 裸模型名按优先级选默认供应商；`provider/model` 可强制指定供应商 |
| **单文件交付** | 前端内嵌，无运行时依赖（无 JVM/Node/libc 要求），Windows / Linux / macOS 各一份 |
| **安全默认** | 仅监听本机回环；供应商凭证 AES-256-GCM 加密落库；SSRF 校验；日志脱敏 |
| **可排查** | 请求日志页可按状态码/模型/供应商筛选，明确区分「Agent 发错了」与「上游拒了」 |

## 快速开始

### 1. 构建

```bash
# 需要 Go 1.25+ 与 Node 20+（前者见 go.mod，后者仅用于构建前端）
make dist                     # Linux / macOS
pwsh -File build.ps1 -Target dist   # Windows
```

产物在 `dist/`：`agoramodel-{windows,linux,darwin}-{amd64,arm64}`（共 6 份）。

> 只构建本机平台：`go build -o agoramodel ./cmd/agoramodel`（前端需先 `npm --prefix web ci && npm --prefix web run build`）。

### 2. 首次启动

```bash
./dist/agoramodel-linux-amd64 --config config.example.json
# 或 Windows
.\dist\agoramodel-windows-amd64.exe --config config.example.json
```

首次启动会：

1. 在数据目录生成主密钥 `master.key`（请备份）；
2. 从 `config.example.json` 导入示例供应商（**仅在数据库为空时导入一次**）；
3. **打印一次网关 Key**（`gateway_key=gw-…`）——这就是所有 Agent 要填的 Key，请立即保存。

随后访问 <http://127.0.0.1:9090> 打开 Web 控制台，在「供应商管理」里改成你自己的供应商即可。

### 3. 让 Agent 接入

```bash
# Anthropic 协议（Claude Code 等）
export ANTHROPIC_BASE_URL=http://127.0.0.1:9090
export ANTHROPIC_API_KEY=gw-你的网关Key

# OpenAI 协议（Codex / Cursor / 任意 OpenAI 兼容工具）
export OPENAI_BASE_URL=http://127.0.0.1:9090/v1
export OPENAI_API_KEY=gw-你的网关Key
```

新增供应商后无需重启：所有 Agent 的后续请求立即按新配置路由。

## 启动参数与环境变量

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `--config` | — | `config.json` | 引导配置（**仅在数据库空时**用于首次导入供应商） |
| `--data-dir` | — | 见下 | 数据目录（放 `agora.db`、`master.key`） |
| `--db` | — | `<数据目录>/agora.db` | SQLite 路径 |
| `--listen` | — | `127.0.0.1` | 监听地址；**改为非回环时必须设置管理密码** |
| `--port` | — | `9090` | 监听端口 |
| `--admin-password` | `ADMIN_PASSWORD` | 空 | Web UI 管理密码；设置后 /api 需要登录 |
| `--log-format` | — | `text` | `text` 或 `json` |
| `--log-level` | — | `info` | `debug` / `info` / `warn` / `error` |
| — | `GW_MASTER_KEY` | 空 | 主密钥（64 位 hex）；**推荐用它替代 master.key 文件** |

数据目录默认位置：Windows `%AppData%\AgoraModel`、Linux `~/.config/agoramodel`、
macOS `~/Library/Application Support/AgoraModel`。

## 数据、安全与备份

- **数据 = 两个文件**：`agora.db`（SQLite，WAL 模式）与 `master.key`。备份这两个即可迁移。
- **凭证加密**：供应商 API Key 以 AES-256-GCM 密文存储（AAD 绑定供应商 id），
  数据库或备份泄露也拿不到明文；Web UI 与 API 只回显掩码。
- **主密钥丢失 = 凭证不可解密**：此时启动会直接失败并提示，**不会静默降级为明文**。
  若不想依赖文件，用 `GW_MASTER_KEY` 环境变量（例如放在 systemd 的 `Environment=`）。
- **网关 Key**：以 sha256 存储，重置后旧 Key 立即失效；明文只在生成/重置时显示一次。
- **访问控制**：默认只监听 `127.0.0.1`（本机免登录）；一旦监听非回环地址，
  必须设置 `ADMIN_PASSWORD`，否则启动直接拒绝。

## 服务化（开机自启）

网关自带服务管理子命令，自动对接各平台服务管理器：

```bash
./agoramodel install      # Windows: SCM / Linux: systemd / macOS: launchd
./agoramodel start
./agoramodel status
./agoramodel stop
./agoramodel uninstall
```

> 安装/卸载需要管理员（Windows）或 root（Linux）权限。
> 服务启动参数由 `install` 时的 `--config/--data-dir/--port` 决定（路径已转绝对）。
> **服务模式没有控制台**，日志请写文件或交给平台日志；`ADMIN_PASSWORD` 建议通过服务配置注入，不要写在命令行里。

<details>
<summary>手工托管（systemd 示例）</summary>

```ini
[Unit]
Description=AgoraModel Gateway
After=network-online.target

[Service]
ExecStart=/usr/local/bin/agoramodel --listen 127.0.0.1 --port 9090 --data-dir /var/lib/agoramodel
Environment=GW_MASTER_KEY=<64 位 hex>
Restart=on-failure
User=agoramodel

[Install]
WantedBy=multi-user.target
```

</details>

## Web 控制台

| 页面 | 用途 |
| --- | --- |
| 供应商管理 | 列表 / 新增 / 编辑 / 删除、**连接测试**（双协议）、**拉取模型**、启用停用、优先级 |
| 模型列表 | 聚合结果（裸名与 `provider/model` 视图切换）、来源标注、手动增删、全量刷新 |
| 网关设置 | 网关 Key（掩码 / 重置）、Agent 环境变量片段一键复制、模型刷新间隔、成功日志开关 |
| 请求日志 | 按状态码 / 模型 / 供应商 / 仅失败筛选，分页与错误展开，可 5 秒自动刷新 |
| 登录页 | 仅当设置了 `ADMIN_PASSWORD` 时出现 |

## 反向代理（如需 HTTPS 或远程访问）

流式响应必须关闭代理缓冲，否则首字节会被攒住：

```nginx
location / {
    proxy_pass http://127.0.0.1:9090;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_buffering off;            # SSE 必需
    proxy_cache off;
    proxy_read_timeout 3600s;       # 长推理场景
    chunked_transfer_encoding on;
}
```

网关自身也做了 SSE 空闲心跳（默认 15s 注入 `: keep-alive`）作为兜底。

## 常见问题

**Q：上游 `/models` 不可用，模型列表是空的？**
在供应商的「手动模型」里填写模型名；或在高级选项里设置端点覆盖。手动模型始终生效，并可配置「排除模型」。

**Q：添加供应商时报“解析到内网或回环地址”？**
这是 SSRF 防护。确实要接入本机 Ollama / 公司内网服务时，打开该供应商的「允许内网地址」。

**Q：模型不出现 / 拉取失败？**
供应商的 `openai_base_url` 需要指向能提供 `/models` 的端点（自动补 `/models`）。
失败原因会写在供应商列表的「最近拉取」与 Web UI 中，且**不会清空上一次的缓存**。

**Q：换了一台机器要重新配置吗？**
不需要。复制 `agora.db` + `master.key`（或在新机器上用 `GW_MASTER_KEY`），
各 Agent 的环境变量指向新地址即可。（导出功能故意不导出明文凭证。）

**Q：macOS 上提示「无法验证开发者」？**
二进制未签名：`xattr -dr com.apple.quarantine agoramodel-darwin-arm64` 后再运行。

**Q：`build.ps1` 报「无法将 go 项识别为 cmdlet / 函数 / 脚本文件」？**
已修复：脚本会自行合并系统与用户 PATH，并探测常见安装目录（`C:\Program Files\Go\bin`、
`%LOCALAPPDATA%\Programs\Go\bin`、winget 包目录等）。原因是通过 winget 等方式安装工具时，
只修改了系统 PATH，而**已经打开的终端仍持有旧 PATH**。
若仍报错则说明 Go 确实未安装：`winget install GoLang.Go` 后重试（无需重开终端）。
同样的探测也已接入 `tools/smoke/*.ps1`。

**Q：构建时 npm 提示 `allow-scripts ... esbuild`？**
这是 npm 11+ 的安全策略（默认不执行依赖的安装脚本）。esbuild 的平台二进制由可选依赖提供，
**不影响构建**；如需消除提示可执行 `npm approve-scripts esbuild`。

## 开发

```bash
go test ./... -count=1                    # 单元测试
go vet ./... && gofmt -l cmd internal      # 静态检查

# 端到端冒烟（会自行构建前端与二进制）
pwsh -File tools/smoke/phase1.ps1   # 双协议透传、SSE 心跳、取消、错误码
pwsh -File tools/smoke/phase2.ps1   # 持久化、加密、网关 Key 生命周期、SSRF
pwsh -File tools/smoke/phase3.ps1   # 模型聚合、命名空间路由
pwsh -File tools/smoke/phase4.ps1   # Web UI 与 API 全流程、登录模式

# 本地 mock 上游（冒烟脚本会自动启动；也可单独用于调试）
node tools/mock-upstream/server.mjs
```

目录结构：

```
cmd/agoramodel      入口（含服务化子命令）
internal/api        控制面 REST（/api/*）
internal/config     配置模型与不可变快照
internal/crypto     AES-256-GCM 与主密钥
internal/gateway    数据面透传（/v1/*，SSE 保活与取消）
internal/logging    请求日志异步批量写入与脱敏
internal/models     模型聚合
internal/platform   跨平台数据目录
internal/provider   供应商连接测试
internal/route      模型名路由（含命名空间）
internal/security   SSRF 校验
internal/store      SQLite 持久化与迁移
internal/webui      内嵌前端与 SPA 路由
web                 前端工程（Vite + React）
docs                PRD / 功能明细设计说明书 / 待办清单
```

更多设计细节见：
[`docs/PRD.md`](docs/PRD.md)（需求与验收）、[`docs/DESIGN.md`](docs/DESIGN.md)（接口契约与实现）、
[`docs/TODO.md`](docs/TODO.md)（分阶段任务与实测证据）。
