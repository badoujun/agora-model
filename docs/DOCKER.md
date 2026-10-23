# Docker 部署

本文说明如何把 AgoraModel 构建成镜像并运行。镜像内的进程是**单文件二进制**：
Node 只用于构建前端，运行镜像里没有 Node / Go，也不需要系统库（`CGO_ENABLED=0`）。

## 1. 关于 docker.aityp.com

<https://docker.aityp.com/>（渡渡鸟镜像同步站）是**镜像索引站**，不是 Docker registry：

- 它**不能**写进 `/etc/docker/daemon.json` 的 `registry-mirrors`，`/v2/` 也不是可用的 registry 端点。
- 每个镜像详情页会给出实际可拉取的地址，形如：

  ```bash
  docker pull swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.25-alpine
  docker tag  swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.25-alpine \
              docker.io/library/golang:1.25-alpine
  ```

  实际存储在北京四的华为云 SWR 上，国内可直接拉取。

本项目已经把 Dockerfile / compose 的默认基础镜像指向这些地址：

| 用途 | 默认镜像 |
| --- | --- |
| 前端构建 | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/node:22-alpine` |
| 后端构建 | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.25-alpine` |
| 运行时 | `swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.22` |

找不到所需 tag 时，可在该站搜索镜像名，进详情页复制命令；也可以用它首页的
「添加镜像」提交同步（通常 24 小时内完成）。

> **arm64 注意**：该站的 arm64 副本常带 `-linuxarm64` 后缀（如
> `.../alpine:3.22-linuxarm64`）。在详情页切到 `linux/arm64` 后复制实际命令，
> 再通过 `--build-arg` 或 `.env` 覆盖 `NODE_IMAGE` / `GO_IMAGE` / `RUNTIME_IMAGE`。
> 默认的三个地址是 amd64 镜像。
>
> 如果构建机能直连 Docker Hub，可把这三个变量分别改成 `node:22-alpine`、
> `golang:1.25-alpine`、`alpine:3.22`，或使用公司内网 registry。

## 2. 构建

依赖清单里的 tarball 默认指向官方 npm，构建时会用 `NPM_REGISTRY`（默认
`https://registry.npmmirror.com`）重写主机名；Go 模块走 `GOPROXY`（默认
`https://goproxy.cn,direct`）。

### 方式 A：docker compose（推荐）

```bash
cp .env.example .env
# 编辑 .env：至少设置 ADMIN_PASSWORD；建议同时设置 GW_MASTER_KEY
chmod 600 .env
docker compose build
```

### 方式 B：docker build

```bash
docker build \
  --build-arg VERSION="$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//')" \
  -t agoramodel:local .
```

构建完成后也可以不用 compose，直接运行（数据卷名为 `agoramodel-data`）：

```bash
docker run -d --name agoramodel \
  --restart unless-stopped \
  -p 127.0.0.1:9090:9090 \
  -e ADMIN_PASSWORD='换成强密码' \
  -v agoramodel-data:/data \
  agoramodel:local
```

> 想用环境变量保存主密钥时加 `-e GW_MASTER_KEY=<固定的 64 位 hex>`。
> **不要在每次 `docker run` 时重新生成**，否则旧数据无法解密。

常用构建参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `VERSION` | 空 | 注入控制台显示的版本号；空则用程序内置版本 |
| `NODE_IMAGE` | SWR 镜像 | 前端构建基础镜像 |
| `GO_IMAGE` | SWR 镜像 | 后端构建基础镜像 |
| `RUNTIME_IMAGE` | SWR 镜像 | 运行时基础镜像 |
| `NPM_REGISTRY` | `https://registry.npmmirror.com` | 前端依赖源 |
| `GOPROXY` | `https://goproxy.cn,direct` | Go 模块代理 |
| `APK_MIRROR` | `mirrors.aliyun.com` | Alpine apk 源（安装 ca-certificates / tzdata） |

## 3. 配置

`.env` 的关键项：

```dotenv
# Web 控制台登录密码。容器内监听 0.0.0.0，这一项必填。
ADMIN_PASSWORD=换成一个强密码

# 建议使用环境变量保存主密钥：openssl rand -hex 32
# 留空则首次启动在 /data/master.key 生成文件。已有数据后不要更换/丢失。
GW_MASTER_KEY=

# 宿主机绑定地址：127.0.0.1 仅本机；要让其他机器上的 Agent 访问改成 0.0.0.0。
AGORAMODEL_BIND=127.0.0.1

TZ=Asia/Shanghai
```

> 网关默认只监听回环地址；容器里必须监听 `0.0.0.0` 才能端口映射，所以程序会强制要求
> `ADMIN_PASSWORD`。没有密码时容器会直接退出并打印原因，这是预期行为。
>
> 如果任意 `docker compose` 子命令报 `required variable ADMIN_PASSWORD is missing a value`，
> 说明还没有按上面的步骤创建 `.env`——compose 在解析配置阶段就会检查该变量。
>
> 项目根目录的 `config.json` 只用于**空数据库的首次导入**。需要导入时把 compose 里的
> `./config.json:/app/config.json:ro` 取消注释；之后以数据库为准，不会再导入。
> 不挂载也能启动：控制台里可以直接添加供应商。

## 4. 启动与首次使用

```bash
docker compose up -d
docker compose ps                 # STATUS 里应看到 healthy
docker compose logs -f agoramodel
```

首次启动会生成默认网关 Key，并在日志里**只打印一次**：

```bash
docker compose logs agoramodel | grep gateway_key
# level=WARN msg="已生成网关 Key（可在 Web UI「网关设置」查看或复制）" gateway_key=gw-...
```

之后可以：

1. 浏览器打开 `http://<宿主机IP>:9090`，用 `ADMIN_PASSWORD` 登录；
2. 在「供应商管理」添加供应商（或从内置预设填充），拉取并勾选模型；
3. 在「网关设置」查看/新建网关 Key，复制给 Agent。

Agent 侧配置：

```bash
export OPENAI_BASE_URL=http://<宿主机IP>:9090/v1
export OPENAI_API_KEY=gw-你的网关Key
```

> 如果 Agent 也跑在容器里，`127.0.0.1` 指的是 Agent 自己的容器，请改用宿主机 IP、
> Docker 网络里的服务名（同一 compose 网络内可用 `agoramodel`）或 `host.docker.internal`。

## 5. 数据与备份

数据只有两个文件，都在 `/data` 卷里：

- `/data/agora.db`（SQLite，WAL 模式）
- `/data/master.key`（主密钥；使用 `GW_MASTER_KEY` 环境变量时不需要它）

compose 默认使用命名卷 `agoramodel_agoramodel-data`（直接用 `docker run` 时是
`agoramodel-data`）。备份：

```bash
docker compose stop
docker run --rm \
  -v agoramodel_agoramodel-data:/data:ro \
  -v "$PWD":/backup \
  swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.22 \
  tar czf /backup/agoramodel-data-$(date +%F).tgz -C /data .
docker compose start
```

恢复：

```bash
docker compose stop
docker run --rm \
  -v agoramodel_agoramodel-data:/data \
  -v "$PWD":/backup \
  swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.22 \
  sh -c 'rm -f /data/agora.db* /data/master.key && tar xzf /backup/agoramodel-data-YYYY-MM-DD.tgz -C /data'
docker compose start
```

> 备份文件包含加密后的供应商凭证与主密钥，拿到它等于拿到全部配置，请妥善保管。
> `GW_MASTER_KEY` 不随卷备份；用了它就必须单独保存该值。

如果更习惯宿主机目录，可把 compose 的卷改成 `./data:/data`。容器以 UID `10001` 运行，
需要先授权：

```bash
mkdir -p data && sudo chown 10001:10001 data
```

## 6. 升级

```bash
# 先备份数据（见上）
docker compose build --pull
docker compose up -d
docker compose logs --tail=50 agoramodel
```

镜像只替换二进制，`/data` 卷保留。数据库 schema 只增不减：**新版库不能回退给旧版程序**，
升级前建议先备份。需要固定版本时设置 `AGORAMODEL_VERSION`，镜像 tag 会随之变化。

## 7. 对外暴露与 HTTPS

- 仅本机使用：保持 `AGORAMODEL_BIND=127.0.0.1`。
- 局域网 / 公网：改 `AGORAMODEL_BIND=0.0.0.0`，并在前面套 Nginx / Caddy 做 TLS。
- 反向代理必须关闭响应缓冲，否则 SSE 流式输出会被攒住；配置示例见
  [README「反向代理」](../README.md#反向代理如需-https-或远程访问)。

查看健康状态：

```bash
curl http://127.0.0.1:9090/healthz
# {"status":"ok","version":"0.7.0"}
```
