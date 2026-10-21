# AgoraModel 多阶段构建：Node 构建前端 → Go 构建单文件二进制 → Alpine 运行。
#
# 基础镜像默认取 https://docker.aityp.com/ 收录的地址（实际托管在华为云 SWR）。
# 该站是镜像索引站而不是 registry，不能写进 daemon.json 的 registry-mirrors；
# 在镜像详情页复制 `docker pull swr.cn-north-4.myhuaweicloud.com/ddn-k8s/...` 即可。
# 外网可直连 Docker Hub 时用 --build-arg 覆盖下面三个 BASE 镜像即可。
#
# 详细部署说明见 docs/DOCKER.md。

ARG NODE_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/node:22-alpine
ARG GO_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/golang:1.25-alpine
ARG RUNTIME_IMAGE=swr.cn-north-4.myhuaweicloud.com/ddn-k8s/docker.io/library/alpine:3.22

# ---------------------------------------------------------------- 前端
FROM ${NODE_IMAGE} AS web

WORKDIR /src

# 国内 npm 镜像；lock 里的 registry.npmjs.org 会被 npm 按此地址重写主机名。
ARG NPM_REGISTRY=https://registry.npmmirror.com

# 先只拷贝依赖清单，让依赖层可以复用缓存。
COPY web/package.json web/package-lock.json ./web/
RUN npm --prefix web ci --registry="${NPM_REGISTRY}" --no-audit --no-fund

COPY web ./web
# vite 的 outDir 指向 ../internal/webui/dist，产物随后交给 Go embed。
RUN npm --prefix web run build

# ---------------------------------------------------------------- 后端
FROM ${GO_IMAGE} AS build

WORKDIR /src

# 受限网络下的模块代理；与 Makefile 的兜底保持一致。
ARG GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0 \
    GOPROXY=${GOPROXY}

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
# 用前端阶段的构建产物覆盖仓库里的占位 dist（robots.txt），否则内嵌的 Web UI 是空的。
COPY --from=web /src/internal/webui/dist ./internal/webui/dist

# 版本号由 CI / 本地构建注入；留空时使用 cmd/agoramodel 内置的语义化版本号。
ARG VERSION=""
RUN set -eux; \
    ldflags='-s -w'; \
    if [ -n "${VERSION}" ]; then \
        ldflags="${ldflags} -X main.version=${VERSION}"; \
    fi; \
    go build -trimpath -buildvcs=false -ldflags="${ldflags}" -o /out/agoramodel ./cmd/agoramodel

# ---------------------------------------------------------------- 运行
FROM ${RUNTIME_IMAGE}

# ca-certificates：访问 HTTPS 上游；tzdata：让 TZ 环境变量生效。
# Alpine 官方 apk 源在国内常被限速/拒绝，默认换成阿里云；外网可覆盖 APK_MIRROR。
ARG APK_MIRROR=mirrors.aliyun.com
RUN sed -i "s#dl-cdn.alpinelinux.org#${APK_MIRROR}#g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 agora \
    && adduser -S -D -H -u 10001 -G agora -h /app -s /sbin/nologin agora \
    && mkdir -p /data \
    && chown 10001:10001 /data

WORKDIR /app

COPY --from=build --chown=10001:10001 /out/agoramodel /app/agoramodel
# 示例配置只作参考；首次导入时由用户把 config.json 挂载到 /app/config.json。
COPY --chown=10001:10001 config.example.json /app/config.example.json

# 数据卷：agora.db + master.key，备份这两个文件即可迁移。
VOLUME ["/data"]

EXPOSE 9090

# 容器内必须监听 0.0.0.0 才能被端口映射访问；因此 ADMIN_PASSWORD 是必填项。
ENV TZ=Asia/Shanghai

USER 10001:10001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:9090/healthz || exit 1

ENTRYPOINT ["/app/agoramodel"]
CMD ["--config", "/app/config.json", "--data-dir", "/data", "--listen", "0.0.0.0", "--port", "9090"]
