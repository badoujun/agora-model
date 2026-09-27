# @bakeroot/agoramodel

> 本地优先的 AI 模型接入网关 + 配置控制台：给所有 AI Agent 一个固定的入口，
> 新增供应商只配置一次，所有 Agent 立即可用。

- **痛点 A**：每出现一个新的 AI Agent，就要为它单独配置一遍供应商。
- **痛点 B**：每接入一个新供应商，就要给**每一个** Agent 重复配置一遍。
- AgoraModel 的做法：把供应商配置与 Agent 配置解耦——所有 Agent 只对接网关
  （一个 Base URL + 一个 Key），由网关按模型名路由到任意供应商。

```
Agent（Codex / Cursor / 任意支持自定义 Base URL 的工具）
      │  OPENAI_BASE_URL 指向同一个网关
      ▼
 AgoraModel 网关（单可执行文件）
 ├─ /v1/chat/completions  ← OpenAI 兼容协议，原样透传
 ├─ /v1/models            ← 已启用模型列表
 └─ Web 控制台            ← 供应商 / 模型 / 设置 / 日志
```

## 安装

```bash
npm install -g @bakeroot/agoramodel
```

也可以用 `npx` 直接跑（**但注册开机自启服务必须先全局安装**，见下文）：

```bash
npx @bakeroot/agoramodel --version
```

安装的是本机平台的预编译二进制，**零运行时依赖**——不需要 JVM、Python 或系统库。
支持 Windows / Linux / macOS 的 x64 与 arm64，共 6 个平台。

## 快速开始

```bash
# 1) 前台启动（首次会打印一次网关 Key，请立即保存）
agoramodel

# 2) 打开 Web 控制台配置供应商
#    http://127.0.0.1:9090

# 3) 让 Agent 接入
export OPENAI_BASE_URL=http://127.0.0.1:9090/v1
export OPENAI_API_KEY=gw-你的网关Key
```

新增供应商后**无需重启**：所有 Agent 的后续请求立即按新配置路由。

## 后台常驻 / 开机自启

自带服务管理子命令，自动对接各平台服务管理器
（Windows：SCM；Linux：systemd；macOS：launchd）：

```bash
sudo agoramodel install     # Windows 用管理员身份的终端；Linux / macOS 需要 sudo
agoramodel start
agoramodel status
```

> **不要用 `npx` 注册服务**：npx 会把包放进临时缓存目录，该目录随时可能被清理，
> 注册好的服务会随即失效。请务必先 `npm install -g @bakeroot/agoramodel`。
>
> `install` 会把二进制复制到稳定位置（`<数据目录>/bin/`），
> 因此后续 `npm install -g @bakeroot/agoramodel@新版本` **不会**影响已在运行的服务；
> 升级后重新执行一次 `agoramodel install` 即可。

服务模式没有控制台，日志默认写入 `<数据目录>/logs/agoramodel.log`。

需要注入环境变量（例如管理密码、主密钥）：

```bash
sudo agoramodel install \
  --service-env ADMIN_PASSWORD=你的密码 \
  --service-env GW_MASTER_KEY=<64 位 hex>
```

## 卸载

```bash
agoramodel stop          # 停止
sudo agoramodel uninstall # 注销系统服务（数据目录会保留）
npm uninstall -g @bakeroot/agoramodel
```

> `npm uninstall` 不会自动注销系统服务，请先执行 `agoramodel uninstall`。

## 常用参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--config` | `config.json` | 引导配置（仅在数据库为空时用于首次导入供应商） |
| `--data-dir` | 系统用户配置目录 | 存放 `agora.db`、`master.key`、日志 |
| `--listen` / `--port` | `127.0.0.1` / `9090` | 监听地址与端口；改为非回环时**必须**设置管理密码 |
| `--admin-password` | 空 | Web UI 管理密码（也可用环境变量 `ADMIN_PASSWORD`） |
| `--log-file` | 空（stdout） | 日志文件路径 |
| `--log-format` / `--log-level` | `text` / `info` | `text\|json` / `debug\|info\|warn\|error` |

完整文档、设计说明与问题排查：<https://github.com/badoujun/agora-model>

## 数据与安全

- **数据 = 两个文件**：`agora.db`（SQLite）与 `master.key`，备份这两个即可迁移。
- 供应商 API Key 以 AES-256-GCM 密文存储，数据库泄露也拿不到明文。
- 默认只监听本机回环；一旦监听非回环地址，必须设置 `ADMIN_PASSWORD`，否则拒绝启动。

## 平台支持

本包通过 `optionalDependencies` 自动选装对应平台的二进制，
不执行任何安装脚本（兼容 npm 11+ 默认禁用依赖脚本的策略）。
如果你的环境禁用了可选依赖（`--no-optional` / `--ignore-optional`），
请改用发布页的单文件二进制：<https://github.com/badoujun/agora-model/releases>

## 许可证

[MIT](https://github.com/badoujun/agora-model/blob/main/LICENSE) © 2026 badoujun
