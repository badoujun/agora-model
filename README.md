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
   ├─ /v1/models            ← 已启用模型列表（只返回 供应商名/模型名）
   └─ Web 控制台            ← 供应商 / 模型 / 设置 / 日志 / 数据位置
        │
        ▼
   供应商 A（OpenAI 兼容 URL + 多个 Key，轮询/故障转移）
   供应商 B（…）  供应商 C（…）
```

## 特性

| 能力 | 说明 |
| --- | --- |
| **零模板接入** | 不预设供应商类型：填「名称 + OpenAI 兼容地址 + API Key」即可，无需选择模板或改代码 |
| **多凭证轮询** | 一个供应商可配置任意多个 API Key（数量不设上限），请求按顺序轮询以分散配额；401/402/403/429 立即自动切换并**停用失败的 Key**（最后一个可用 Key 仅冷却，避免供应商被彻底打死） |
| **协议原生透传** | 对外提供 OpenAI 兼容接口，**不做格式转换**，流式响应逐字节透传 |
| **模型选择** | 在供应商编辑页一键拉取上游 `/models`，勾选要启用的模型，并可为每个模型设置别称（保存前即可先拉取） |
| **显式路由** | 请求必须使用 `供应商名/模型名`：网关按前缀选择供应商并剥离前缀；裸模型名返回 400，不再有默认供应商 |
| **配置迁移** | 供应商支持导入/导出 JSON（含全部 API Key 明文；同时保留单 Key 兼容字段，旧版本也能导入），换机时导出→导入即可 |
| **单文件交付** | 前端内嵌，无运行时依赖（无 JVM/Node/libc 要求），Windows / Linux / macOS 各一份 |
| **安全默认** | 仅监听本机回环；供应商凭证与网关 Key 均 AES-256-GCM 加密落库；SSRF 校验；日志脱敏 |
| **多网关 Key** | 可同时启用任意多个网关 API Key，每个带别名与备注（用途 / 使用人），可单独启停、软删除、重置明文；每个 Key 还能限定**可调用的供应商 × 模型范围**（留空即不限制），越界请求返回 403 `model_not_allowed` |
| **可排查** | 请求日志页可按状态码/模型（模糊匹配，忽略大小写）/供应商/**网关 Key**筛选，并显示该请求用的是哪个网关 Key，明确区分「Agent 发错了」与「上游拒了」 |
| **可计量** | 使用统计页可选「全部」或单个网关 Key，模型占比、活跃热力图、使用趋势与 KPI 随之切换 |
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
3. **打印一次默认网关 Key**（`gateway_key=gw-…`）——这是默认 Agent 要填的 Key；
   之后可随时在 Web 控制台「网关设置」页查看或复制，也可以再新建多个 Key 分发给不同 Agent。

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

## 多 API Key：轮询与自动停用

一个供应商可以配置任意多个 API Key（UI 里「添加 Key」，可设备注名与启用开关）：

- **轮询**：每次请求按配置顺序取下一个启用的 Key（内存中的轮询计数，无锁、不查库）。
- **故障转移**：上游返回 `401 / 402 / 403 / 429` 时，网关立即换下一个 Key 重试（单个请求最多尝试 3 个 Key，
  且不会在响应已开始返回给客户端之后重试）。所有 Key 都失败时，客户端拿到的仍是最后一次上游响应原文。
- **自动停用**：`401 / 402 / 403 / 429` 的 Key 会被**自动停用**并落库（列表里显示「已自动停用」与原因，
  可在编辑页勾回「启用」或点「全部启用」后保存；正在进行的请求不受影响）。
  **保护**：如果失败的是该供应商**最后一个可用**的 Key，则只冷却 60 秒而不停用——避免一次上游
  401/429 就把整家供应商打死（此时界面会显示失败次数与最近错误，便于排查）。
- **429 与 402 同等处理**：上游限速/额度耗尽（`429`）不再是"临时冷却"，而是与欠费（`402`）一样停用该 Key，
  后续请求不会再白白打上游；被停用的 Key 在供应商页可随时重新启用。
- **失败可追溯**：每一次凭证失败（哪怕后续 Key 成功、请求最终 200）都会把「掩码 Key + 状态码/原因 +
  网关动作」写入请求日志的错误摘要，日志页可直接定位是哪个 Key 因什么被停用。
- **不重试的情况**：`5xx`、`400/404` 等业务错误、客户端断开、响应已开始流式输出。
- **内存热路径**：请求只读内存中的配置快照与 Key 运行时状态（轮询计数、冷却、停用标记），
  配置变更或自动停用后才异步重建快照；数据库不可用时数据面仍可正常转发。
- **每个 Key 的观测**：供应商页展示每个 Key 的近 30 天 token 用量与失败次数；日志页新增「凭证」列，
  能看到具体是哪个 Key 服务的请求，错误摘要里还会记录每次故障转移的失败原因与处理动作。数据库中 Key 密文与明文掠影分离，接口永不回传明文。

## 数据、安全与备份

- **数据 = 两个文件**：`agora.db`（SQLite，WAL 模式）与 `master.key`。备份这两个即可迁移。
  控制台的「数据位置」页会按当前系统列出这两个文件的绝对路径。
- **凭证加密**：供应商 API Key（每个 Key 独立一行密文）与网关 Key 明文都以 AES-256-GCM 密文存储
  （供应商 Key 的 AAD 绑定 `provider.id`，网关 Key 绑定自身 id），数据库泄露也拿不到可直接使用的明文；
  供应商列表只回显掩码与失败次数，网关 Key 仅同源控制台可查看。
- **主密钥丢失 = 凭证不可解密**：此时启动会直接失败并提示，**不会静默降级为明文**。
  若不想依赖文件，用 `GW_MASTER_KEY` 环境变量（例如放在 systemd 的 `Environment=`）。
- **库比程序新 = 拒绝启动**：`agora.db` 的 schema 版本高于当前程序已知的最高版本时，
  启动会明确提示「数据库由更新版本的 agoramodel 创建」并退出，而不是在半途抛出缺列错误。
  迁移只增不减，**降级运行不受支持**：请把程序升级到与库同级或更新的版本。
- **网关 Key**：以 sha256 校验，比较使用常量时间；**可同时启用多个**，分别发给不同的
  Agent / 使用人，每个 Key 可单独启停、软删除与重置明文（只换明文，别名/备注/范围不变）。
  「网关设置」页可随时查看/复制每个 Key 的明文；旧版本只存哈希的 Key 无法还原，页面会提示重置。
  最后一个启用的 Key 不允许停用或删除（避免把所有 Agent 锁死）。
- **网关 Key 的模型范围**：每个 Key 可勾选「供应商 × 模型」白名单，不勾选 = 不限制。
  范围参与路由选择：只有显式 `供应商名/模型名` 且落在范围内才会放行，越界请求返回 403
  `model_not_allowed`（与「模型不存在」的 404 区分开）；`/v1/models` 也只列出范围内的模型。
  范围存的是**上游模型名**，改别名不会让范围失效。
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

## Docker 部署

仓库自带 `Dockerfile` 与 `docker-compose.yml`，基础镜像默认使用
[docker.aityp.com](https://docker.aityp.com/)（渡渡鸟镜像站）收录的华为云 SWR 地址：

```bash
cp .env.example .env
# 编辑 .env：设置 ADMIN_PASSWORD（必填），建议同时设置 GW_MASTER_KEY=$(openssl rand -hex 32)
docker compose up -d --build
docker compose logs agoramodel | grep gateway_key   # 首次启动打印一次的网关 Key
```

默认只绑定宿主机 `127.0.0.1:9090`；要让其他机器上的 Agent 访问，把 `.env` 里的
`AGORAMODEL_BIND` 改成 `0.0.0.0`。数据保存在命名卷 `/data`（`agora.db` + `master.key`）。

> `docker.aityp.com` 是镜像**索引站**，不是 registry，不能配置为 `registry-mirrors`；
> 详情页给出的实际拉取地址形如 `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.25-alpine`。

构建参数、数据备份/恢复、arm64 与升级说明见 **[docs/DOCKER.md](docs/DOCKER.md)**。

## Web 控制台

| 页面 | 用途 |
| --- | --- |
| 供应商管理 | 列表 / 新增 / 编辑 / 删除、**从国内常用预设填充**、**导入 / 导出 JSON**、**连接测试**、**拉取模型并勾选（可设别称，保存前也能拉）**、官网地址、启用停用 |
| 模型列表 | 已启用模型的 `供应商名/模型名` 标识、上游名对照、来源供应商、别称标记、搜索 |
| 网关设置 | **多个网关 Key**（别名 / 备注 / 启停 / 软删除 / 模型范围 / 明文查看与复制 / **行内累计 token 用量**）、Agent 环境变量片段一键复制（可选使用哪个 Key）、成功日志开关、**WebDAV 同步配置（推送 / 拉取 / 状态）** |
| 请求日志 | 按状态码 / 模型（模糊匹配，忽略大小写）/ 供应商 / **网关 Key** / 仅失败筛选，分页与错误展开，可 5 秒自动刷新 |
| 使用统计 | 顶部可选「全部」或**单个网关 Key**；各模型输入 / 输出 / 缓存命中 token、按日 / 周 / 月聚合、活跃热力图（半年），每个模块独立时间窗口（含自定义日期范围） |
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
自动填入名称、官网地址与 OpenAI Base URL；接下来仍是原有流程：添加一个或多个 API Key →
拉取并勾选要启用的模型 → 保存。预设只是把固定的字段先填好，校验、保存、加密、
拉取模型等行为与手动新增完全一致。

> 自定义模板：把编辑好的 JSON 放到 `docs/provider-presets.json`，再用
> `--presets docs/provider-presets.json` 启动二进制即可；不指定则使用编译期内置版本。

## WebDAV 配置同步（多机/备份）

把全部供应商配置（含全部 API Key 明文）同步到自有 WebDAV 服务器，便于多机部署、备份与
回滚（多 Key 的完整同步要求两端版本一致；旧版本只会导入第一个 Key）。在「网关设置」底部填写服务器地址、用户名、密码（与供应商 API Key、网关 Key
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
请求必须写成 `供应商名/模型名`（`GET /v1/models` 返回的就是这个形式），网关按前缀选择供应商，
转发前把前缀剥离。裸模型名会返回 400 `invalid_request_error`，不再按名称升序猜默认供应商。
为模型配置了别称后，别称就是模型名部分（原名不再对外暴露），网关转发前会自动换回上游真实模型名。
含斜杠的真实模型名（如 `meta-llama/Llama-3-70B`）需要先配置别称，再用 `供应商名/别称` 调用。

**Q：换了一台机器要重新配置吗？**
不需要。复制 `agora.db` + `master.key`（或在新机器上用 `GW_MASTER_KEY`），
各 Agent 的环境变量指向新地址即可。
也可以只迁移供应商：在「供应商管理」页点「导出」得到 JSON（**含全部 API Key 明文**，请妥善保管），
到新实例点「导入」即可，凭证会用新机器的密钥重新加密。导出文件同时包含 `keys` 数组与兼容字段
`api_key`（首个启用的 Key），因此 **旧版本实例也能导入**（只会采用第一个 Key）。

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

**Q：启动报 `no such column: api_key_cipher`，或者提示「数据库由更新版本的 agoramodel 创建」？**
数据库文件比你正在跑的二进制**新**。典型场景：本地用源码构建的版本已经把库迁移到更高 schema
（例如 v6 删掉了 `providers.api_key_cipher`），随后又执行了 `npm install -g @bakeroot/agoramodel`，
装到的却是更早发布的版本，于是它拿旧 SQL 去查已被删掉的列。

迁移只增不减，**新版库不能回退给旧版程序使用**。按提示升级程序即可：

```bash
npm install -g @bakeroot/agoramodel@latest   # 或用与库同级/更新的源码重新 make dist
agoramodel --version                          # 确认版本号
```

> `agora.db` + `master.key` 是唯一数据，升级只涉及这两个文件之外的二进制；
> 库本身不会被降级，也不需要手工改表。

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
- CI 发布（`.github/workflows/release.yml`）走 GitHub Actions OIDC（npm trusted publishing），
  **不需要任何 token**：需在 npmjs.com 上为 7 个包分别配置 Trusted Publisher（workflow 填
  `release.yml`）。OIDC 下 `npm whoami` 必然失败，`publish.mjs` 会自动跳过这道预检。
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
