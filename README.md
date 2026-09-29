# AgoraModel

> **一个本地优先的 AI 模型接入网关 + 配置控制台**：给所有 AI Agent 一个固定的入口，
> 新增供应商只配置一次，所有 Agent 立即可用。

- 痛点 A（横向）：每出现一个新的 AI Agent，就要在 CC Switch 之类的工具里为它单独配置供应商。
- 痛点 B（纵向）：每接入一个新供应商，就要给**每一个** Agent 重复配置一遍（N×M 重复劳动）。
- AgoraModel 的做法：**把供应商配置与 Agent 配置解耦**。所有 Agent 只对接网关
  （一个 Base URL + 一个 Key），由网关按模型名路由到任意供应商。

```
Agent（Codex / Cursor / 任意支持自定义 Base URL 的工具）
        │  OPENAI_BASE_URL 指向同一个网关
        ▼
   AgoraModel 网关（单可执行文件）
   ├─ /v1/chat/completions  ← OpenAI 兼容协议，原样透传
   ├─ /v1/models            ← 已启用模型列表（含 供应商名/模型名 命名空间）
   └─ Web 控制台            ← 供应商 / 模型 / 设置 / 日志 / 数据位置
        │
        ▼
   供应商 A（OpenAI 兼容 URL + 1 个 Key）
   供应商 B（…）  供应商 C（…）
```

## 特性

| 能力 | 说明 |
| --- | --- |
| **零模板接入** | 不预设供应商类型：填「名称 + OpenAI 兼容地址 + API Key」即可，无需选择模板或改代码 |
| **协议原生透传** | 对外提供 OpenAI 兼容接口，**不做格式转换**，流式响应逐字节透传 |
| **模型选择** | 在供应商编辑页一键拉取上游 `/models`，勾选要启用的模型，并可为每个模型设置别称（保存前即可先拉取） |
| **显式路由** | 裸模型名按供应商名称升序选默认供应商；`供应商名/模型名` 可强制指定供应商 |
| **配置迁移** | 供应商支持导入/导出 JSON（含 API Key 明文），换机时导出→导入即可 |
| **单文件交付** | 前端内嵌，无运行时依赖（无 JVM/Node/libc 要求），Windows / Linux / macOS 各一份 |
| **安全默认** | 仅监听本机回环；供应商凭证 AES-256-GCM 加密落库；SSRF 校验；日志脱敏 |
| **可排查** | 请求日志页可按状态码/模型（模糊匹配，忽略大小写）/供应商筛选，明确区分「Agent 发错了」与「上游拒了」 |
| **数据透明** | 「数据位置」页按当前系统列出数据库、主密钥、日志等落盘路径，支持一键复制路径 |

## 快速开始

### 1. 构建

```bash
# 需要 Go 1.25+ 与 Node 20+（前者见 go.mod，后者仅用于构建前端与跑冒烟脚本）
make dist                     # Linux / macOS
pwsh -File build.ps1 -Target dist   # Windows
```

产物在 `dist/`：`agoramodel-{windows,linux,darwin}-{amd64,arm64}`（共 6 份）。

> 首次在 Linux / macOS 上开发，先跑 `./tools/dev/preflight.sh`：它按 `go.mod` 声明比对
> Go 版本，并给出 Node / make / git 缺失时的安装命令。
>
> 该脚本用 bash 写（靠 `${BASH_SOURCE[0]}` 定位仓库根、并开了 `pipefail`），请**直接执行
> 或用 `bash` 调用**：Debian / Ubuntu 上 `/bin/sh` 是 dash，`sh ./tools/dev/preflight.sh`
> 会直接报错退出（脚本专门拦了这种情况，以免对着文件系统根目录跑完还报「环境就绪」）。

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
3. **打印一次网关 Key**（`gateway_key=gw-…`）——这就是所有 Agent 要填的 Key；
   之后可随时在 Web 控制台「网关设置」页查看或复制。

随后访问 <http://127.0.0.1:9090> 打开 Web 控制台，在「供应商管理」里改成你自己的供应商即可。

### 3. 让 Agent 接入

```bash
# OpenAI 协议（Codex / Cursor / 任意 OpenAI 兼容工具）
export OPENAI_BASE_URL=http://127.0.0.1:9090/v1
export OPENAI_API_KEY=gw-你的网关Key
```

新增供应商后无需重启：所有 Agent 的后续请求立即按新配置路由（在供应商编辑页拉取并勾选模型后即可使用）。

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
| `--log-file` | — | 空（写 stdout） | 日志文件路径（追加写入，自动建目录）；**服务模式没有控制台，必须落文件**，`install` 时缺省为 `<数据目录>/logs/agoramodel.log` |
| `--presets` | — | 编译期内置 | 国内常用供应商预设 JSON；为空时使用内置的 DeepSeek / MiniMax / SCNet / Agnes |
| `--service-env` | — | 空 | 仅 `install`：注入服务进程的环境变量，可重复（如 `--service-env ADMIN_PASSWORD=…`）；**明文会写入服务配置** |
| `--service-exec` | — | 空 | 仅 `install`：服务要运行的可执行文件；缺省把当前二进制复制到 `<数据目录>/bin/` 后运行该副本 |
| — | `GW_MASTER_KEY` | 空 | 主密钥（64 位 hex）；**推荐用它替代 master.key 文件** |

数据目录默认位置：Windows `%AppData%\AgoraModel`、Linux `~/.config/agoramodel`、
macOS `~/Library/Application Support/AgoraModel`。

## 数据、安全与备份

- **数据 = 两个文件**：`agora.db`（SQLite，WAL 模式）与 `master.key`。备份这两个即可迁移。
  控制台的「数据位置」页会按当前系统列出这两个文件的绝对路径。
- **凭证加密**：供应商 API Key 与网关 Key 明文都以 AES-256-GCM 密文存储（AAD 绑定记录 id），
  数据库泄露也拿不到可直接使用的明文；供应商列表只回显掩码，网关 Key 仅同源控制台可查看。
- **主密钥丢失 = 凭证不可解密**：此时启动会直接失败并提示，**不会静默降级为明文**。
  若不想依赖文件，用 `GW_MASTER_KEY` 环境变量（例如放在 systemd 的 `Environment=`）。
- **网关 Key**：以 sha256 校验，重置后旧 Key 立即失效；「网关设置」页可随时查看/复制当前 Key
  （旧版本只存哈希的 Key 无法还原，页面会提示重置）。
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

> 安装/卸载需要管理员（Windows）或 root（Linux）权限；权限不足时会给出提示而不是原始错误码。

`install` 会做三件事，避免服务启动后出现「数据是空的」「日志丢了」「二进制不见了」：

1. **固化数据目录**：`--data-dir` 缺省时取系统数据目录并转成绝对路径写进服务配置。
   服务账户与前台运行往往不是同一个（Windows SCM 默认 `LocalSystem`），
   不固化的话 `%AppData%` 会解析到 `…\systemprofile\…`，看起来就是「服务起来了但供应商是空的」。
2. **复制二进制到稳定位置**：缺省把当前二进制复制到 `<数据目录>/bin/agoramodel[.exe]` 并运行该副本。
   通过 npm / npx 运行时，原路径位于 `node_modules` 或 `_npx` 缓存，升级或清理缓存都会整体删除该目录，
   已注册的服务随即指向一个不存在的文件。用 `--service-exec` 可指定其它路径。
3. **日志落文件**：`--log-file` 缺省为 `<数据目录>/logs/agoramodel.log`。
   服务模式没有控制台（Windows SCM 尤其如此），不落文件就无处排查。

`install` 是幂等的：同名服务已存在时会先停止并卸载再重新安装，因此**升级二进制后重跑一次 `install` 即可**。

环境变量（`ADMIN_PASSWORD`、`GW_MASTER_KEY` 等）通过 `--service-env` 注入：

```bash
# Windows（管理员）
.\agoramodel.exe install --data-dir "D:\AgoraModel" --service-env ADMIN_PASSWORD=你的密码

# Linux（root）
sudo ./agoramodel install --data-dir /var/lib/agoramodel \
  --service-env ADMIN_PASSWORD=你的密码 --service-env GW_MASTER_KEY=<64 位 hex>
```

> `--service-env` 的明文会写入服务配置（Linux systemd unit / Windows 注册表），请限制该文件权限。
> 不注入 `GW_MASTER_KEY` 时服务会依赖数据目录下的 `master.key` 文件，务必一起备份。

参数两种写法都支持（`install --data-dir X` 与 `--data-dir X install`）。

在 Linux 上服务由 systemd 托管（Windows 为 SCM、macOS 为 launchd），排查时的常用命令：

```bash
systemctl status agoramodel                        # 等价于 ./agoramodel status
systemctl cat agoramodel                           # 看 install 写入的 unit（含 --log-file 与 Environment=）
tail -f <数据目录>/logs/agoramodel.log              # install 缺省注入的日志文件
sudo journalctl -u agoramodel -f                   # unit 尚未配置 --log-file 时的输出
```

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

## 通过 npm 安装（可选）

除了下载单文件二进制，也可以把 npm 当作分发通道：

```bash
npm install -g @bakeroot/agoramodel
agoramodel --version
```

包结构与 esbuild / biome 相同：主包 `@bakeroot/agoramodel` 只含一个 Node 转发器，真正的二进制由
6 个平台包（`@bakeroot/agoramodel-<os>-<cpu>`）通过 `optionalDependencies` 提供——npm 在**安装期**
按 `os` / `cpu` 自动选装，**不执行任何安装脚本**（兼容 npm 11+ 默认禁用依赖脚本的策略）。

> 注册开机自启服务请先 `npm install -g`，**不要用 `npx`**：
> npx 的临时缓存目录随时会被清理，注册好的服务会随即失效。
>
> `install` 会把二进制复制到 `<数据目录>/bin/` 再注册服务，因此后续
> `npm install -g @bakeroot/agoramodel@新版本` 不会影响正在运行的服务；升级后重跑一次 `install` 即可。

卸载：

```bash
agoramodel uninstall         # 先注销系统服务（数据目录会保留）
npm uninstall -g @bakeroot/agoramodel  # 再卸载包
```

## Web 控制台

| 页面 | 用途 |
| --- | --- |
| 供应商管理 | 列表 / 新增 / 编辑 / 删除、**从国内常用预设填充**、**导入 / 导出 JSON**、**连接测试**、**拉取模型并勾选（可设别称，保存前也能拉）**、官网地址、启用停用 |
| 模型列表 | 已启用模型的对外名与上游名对照、来源供应商、默认路由标注、搜索 |
| 网关设置 | 网关 Key（明文，可一键复制 / 重置）、Agent 环境变量片段一键复制、成功日志开关、**WebDAV 同步配置（推送 / 拉取 / 状态）** |
| 请求日志 | 按状态码 / 模型（模糊匹配，忽略大小写）/ 供应商 / 仅失败筛选，分页与错误展开，可 5 秒自动刷新 |
| 数据位置 | 按当前系统列出数据目录 / 数据库 / 主密钥 / 日志等落盘路径、占用大小，一键复制路径 |
| 登录页 | 仅当设置了 `ADMIN_PASSWORD` 时出现 |

## 国内常用供应商预设

内置 4 个国内常用 OpenAI 兼容供应商模板（`internal/presets/builtin.json` 与
`docs/provider-presets.json` 同步维护）：

| ID | 名称 | Base URL |
| --- | --- | --- |
| `deepseek` | DeepSeek | `https://api.deepseek.com/v1` |
| `minimax` | MiniMax | `https://api.minimaxi.com/v1`（中国站） |
| `scnet` | 国家超算互联网 | `https://api.scnet.cn/api/llm/v1` |
| `agnes` | Agnes AI | `https://api.agnes-ai.cn/v1`（中国站） |

在「供应商管理」页右上角点 **「从预设…」**，选模板后会在 **「新增供应商」** 对话框里
自动填入名称、官网地址与 OpenAI Base URL；接下来仍是原有流程：填入 API Key →
拉取并勾选要启用的模型 → 保存。预设只是把固定的字段先填好，校验、保存、加密、
拉取模型等行为与手动新增完全一致。

> 自定义模板：把编辑好的 JSON 放到 `docs/provider-presets.json`，再用
> `--presets docs/provider-presets.json` 启动二进制即可；不指定则使用编译期内置版本。

## WebDAV 配置同步（多机/备份）

把全部供应商配置（含 API Key 明文）同步到自有 WebDAV 服务器，便于多机部署、备份与
回滚。在「网关设置」底部填写服务器地址、用户名、密码（与供应商 API Key、网关 Key
同样以 AES-256-GCM 加密落库），即可使用：

- **推送本地 → 远端**：在 `provider` 标签页上方新增/删除/调整供应商后推一把。
- **拉取远端 → 本地**：在新机器上跑通后只拉一次即可拿到完整配置。
- **状态**：每 15 秒自动刷新，显示「本地/远端」SHA256 指纹与「是否一致」徽标。

冲突保护（避免误覆盖）：

- 推送前会先 `GET` 远端并比对 SHA256；远端被别人改过时返回 409，控制台会弹
  「确认覆盖远端？」二次确认。
- 拉取同理：本地与远端不一致时先确认再覆盖。
- 凭证密文不会出现在 `GET /api/settings/webdav` 响应里，仅配置项 `has_password` 字段。

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
在供应商编辑页点「拉取模型」获取候选；若上游没有该接口，可在「OpenAI 端点覆盖」里填完整地址（此时无法拉取，需要上游提供 `/models` 才能勾选模型）。

**Q：添加供应商时报“解析到内网或回环地址”？**
这是 SSRF 防护。确实要接入本机 Ollama / 公司内网服务时，打开该供应商的「允许内网地址」。

**Q：模型不出现 / 拉取失败？**
供应商的 `openai_base_url` 需要指向能提供 `/models` 的端点（自动补 `/models`）。
失败原因会写在供应商列表的「最近拉取」与 Web UI 中，且**不会清空上一次的候选列表**。

**Q：多个供应商有同名模型，请求会走哪一个？**
裸模型名按供应商名称升序选第一个提供该模型的供应商；需要指定时用 `供应商名/模型名`。
为模型配置了别称后，别称就是对外模型名（原名不再对外暴露），网关转发前会自动换回上游真实模型名。

**Q：换了一台机器要重新配置吗？**
不需要。复制 `agora.db` + `master.key`（或在新机器上用 `GW_MASTER_KEY`），
各 Agent 的环境变量指向新地址即可。
也可以只迁移供应商：在「供应商管理」页点「导出」得到 JSON（**含 API Key 明文**，请妥善保管），
到新实例点「导入」即可，凭证会用新机器的密钥重新加密。

**Q：macOS 上提示「无法验证开发者」？**
二进制未签名：`xattr -dr com.apple.quarantine agoramodel-darwin-arm64` 后再运行。

**Q：`build.ps1` 报「无法将 go 项识别为 cmdlet / 函数 / 脚本文件」？**
已修复：脚本会自行合并系统与用户 PATH，并探测常见安装目录（`C:\Program Files\Go\bin`、
`%LOCALAPPDATA%\Programs\Go\bin`、winget 包目录等）。原因是通过 winget 等方式安装工具时，
只修改了系统 PATH，而**已经打开的终端仍持有旧 PATH**。
若仍报错则说明 Go 确实未安装：`winget install GoLang.Go` 后重试（无需重开终端）。
同类 PATH 探测保留在 `build.ps1` 内部；Linux / macOS 侧对应的预检是 `./tools/dev/preflight.sh`
（多一项版本比对：发行版仓库自带的 Go 常常低于 `go.mod` 要求）。
端到端冒烟脚本已改为 Node（`tools/smoke/*.mjs`），不再依赖 shell 的 PATH 探测机制。

**Q：构建时 npm 提示 `allow-scripts ... esbuild`？**
这是 npm 11+ 的安全策略（默认不执行依赖的安装脚本）。esbuild 的平台二进制由可选依赖提供，
**不影响构建**；如需消除提示可执行 `npm approve-scripts esbuild`。

**Q：`npm ci` 报 `EALLOWREMOTE: Fetching packages of type "remote" have been disabled`？**
`web/package-lock.json` 里记录的 tarball 地址若与你当前配置的 registry 不同源，npm 12+
会按 `allow-remote` 策略拒绝（默认 `none`）。本仓库的 lock 统一指向官方
`registry.npmjs.org`——npm 会按你配置的 registry（含国内镜像）重写 tarball 的 host，
所以正常情况下不会触发。若你本地改过 lock 或用了自定义源，可临时放开：
`npm --prefix web ci --allow-remote=all`。

**Q：`make dist` 报 `dial tcp ...: i/o timeout`（拉不到 Go 依赖）？**
受限网络（内网 / 大陆直连）连不上 `proxy.golang.org`。`Makefile` 与 `build.ps1` 都已在
未显式设置时回落到 `GOPROXY=https://goproxy.cn,direct`；也可以用环境变量覆盖，或依赖
已在本机缓存时用 `GOPROXY=off make dist` 完全离线构建。

## 开发

三个平台共用同一套源码与脚本，差别只在入口命令：Windows 用 PowerShell，Linux / macOS 用 Makefile。

### Linux / macOS

```bash
./tools/dev/preflight.sh   # 环境预检：Go 1.25+ / Node 20+ / make / git（bash 脚本，勿用 sh 调用）

make lint      # go vet + gofmt（提交前跑这个）
make test      # 单元测试（CGO_ENABLED=0，与发布基线一致）
make dist      # 三平台六份产物 + CGO / 静态链接校验
make smoke     # 端到端冒烟 phase1–5
make smoke-phase3   # 只跑某一阶段
```

### Windows

```powershell
pwsh -File build.ps1 -Target dist
go test ./... -count=1
node tools/smoke/phase1.mjs     # 冒烟脚本是 Node，两个平台跑的是同一份
```

### 端到端冒烟（Node，Windows / Linux / macOS 共用）

```bash
node tools/smoke/phase1.mjs   # OpenAI 协议透传、SSE 心跳、取消、错误码
node tools/smoke/phase2.mjs   # 持久化、加密、网关 Key 生命周期、SSRF
node tools/smoke/phase3.mjs   # 模型拉取与勾选、供应商名命名空间路由、模型别名
node tools/smoke/phase4.mjs   # Web UI 与 API 全流程、登录模式
node tools/smoke/phase5.mjs   # PRD 八条验收、性能冒烟、迁移演练
```

每个阶段自建临时数据目录、自动拉起 mock 上游，结束时打印 `N/N 通过`，失败返回非零退出码。
默认端口从 19090 起（phase1 → 19090、phase2 → 19091、…、mock 统一 9999），
可用 `--port=` / `--mock-port=` 覆盖。

`--exe=<路径>` 可跳过 `go build`，直接对一份已构建产物跑全部断言——CI 就是这样验证**发布产物本身**
（而不是另起一次 go build 的开发二进制）：

```bash
node tools/smoke/phase1.mjs --exe=dist/agoramodel-linux-amd64
```

> 冒烟脚本原先只有 PowerShell 版本（`tools/smoke/*.ps1`），只能在 Windows 上跑，且用了
> `curl.exe`、`Start-Process -WindowStyle` 等 Windows 专属调用。现已统一改写成
> `tools/smoke/*.mjs` + 共享库 `tools/smoke/lib/harness.mjs`，三个平台一份实现。

```bash
# 本地 mock 上游（冒烟脚本会自动启动；也可单独用于调试）
node tools/mock-upstream/server.mjs

# npm 分发包
node --test npm/test/npm-package.test.mjs npm/test/tarball.test.mjs
```

### npm 包构建与发布

```bash
# 1) 先产出六份 Go 产物（版本号建议与 npm 版本保持一致）
pwsh -File build.ps1 -Target dist -Version 0.2.0   # 或 make dist

# 2) 打成 7 个包：主包 agoramodel + 6 个平台包 agoramodel-<os>-<cpu>
node npm/scripts/build.mjs --version=0.2.0

# 3) 预演（不发布） / 正式发布
node npm/scripts/publish.mjs --dry-run
node npm/scripts/publish.mjs
```

- 产物在 `npm/dist/tarballs/`，共 7 个 `.tgz`（每个约 5 MB）。
- 发布顺序固定在 `publish.mjs` 里：**先平台包、后主包**——主包的 `optionalDependencies`
  指向平台包，反过来发会让先安装的人拿到一个拉不到二进制的版本。
- `publish.mjs` 默认发到 `https://registry.npmjs.org`，**不沿用** `npm config get registry`
  （国内环境常把它设成只读镜像如 `registry.npmmirror.com`，发布必然失败且报错晦涩）；
  需要私有源时显式传 `--registry=`。
- 发布前先 `npm login`；账号开了 2FA 时传 `--otp=123456`。
- `build.mjs` **自己写 tar** 而不调用 `npm pack`：Windows 文件系统无法表达 Unix 权限位
  （`chmod` 是空操作，实测 mode 恒为 666），npm pack 出来的 Linux / macOS 平台包里
  二进制会是 644，装到 Linux 上无法执行。

目录结构：

```
cmd/agoramodel      入口（含服务化子命令）
internal/api        控制面 REST（/api/*）
internal/config     配置模型与不可变快照
internal/crypto     AES-256-GCM 与主密钥
internal/gateway    数据面透传（/v1/*，SSE 保活与取消）
internal/logging    请求日志异步批量写入与脱敏
internal/models     模型候选列表拉取（/models）
internal/platform   跨平台数据目录、日志落文件、服务化
internal/provider   供应商连接测试
internal/route      模型名路由（含命名空间）
internal/security   SSRF 校验
internal/store      SQLite 持久化与迁移
internal/webui      内嵌前端与 SPA 路由
npm                 npm 分发：主包转发器 + 平台包构建/发布脚本
web                 前端工程（Vite + React）
tools/mock-upstream 双协议 mock 上游（Node）
tools/smoke         端到端冒烟 phase1–5（Node，Windows / Linux / macOS 共用同一份）
tools/dev           Linux / macOS 开发环境预检
docs                PRD / 功能明细设计说明书 / 待办清单
```

更多设计细节见：
[`docs/PRD.md`](docs/PRD.md)（需求与验收）、[`docs/DESIGN.md`](docs/DESIGN.md)（接口契约与实现）、
[`docs/TODO.md`](docs/TODO.md)（分阶段任务与实测证据）。

## 许可证

[MIT](LICENSE) © 2026 badoujun
