# AgoraModel 构建脚本（Linux / macOS）
#
# 这是 Linux / macOS 上的统一入口（Windows 用 build.ps1）：
#
#   make build        构建当前平台产物
#   make dist         构建三平台六份产物（Windows / Linux / macOS × amd64 / arm64）并校验
#   make web          仅构建前端（Phase 4 起生效）
#   make verify       校验产物为静态链接且 CGO 关闭
#   make test         单元测试（CGO_ENABLED=0，与发布基线一致）
#   make vet          go vet 静态检查
#   make fmt          检查 gofmt（只报不写）
#   make fmt-fix      按 gofmt 重写
#   make lint         vet + fmt（提交前跑这个）
#   make smoke        端到端冒烟 phase1–5（Node 脚本，与 Windows 共用同一份）
#   make smoke-phase3 只跑指定阶段（phase1 … phase5）
#   make tidy         整理 go.mod
#   make clean        清理 dist/

# 版本号优先取最近的 semver tag；仓库还没有 tag 时不注入，
# 由 cmd/agoramodel 内置的版本号兜底（页面上显示可读版本号而不是构建哈希）
# 去掉 tag 的 v 前缀：控制台统一按 "v<version>" 展示，避免出现 vv0.2.0
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null)
VERSION := $(patsubst v%,%,$(VERSION))
DIST    := dist
CMD     := ./cmd/agoramodel
GOFLAGS := -trimpath
LDFLAGS := -s -w $(if $(VERSION),-X main.version=$(VERSION),)

# 目标矩阵（与 docs/DESIGN.md ADR-001 一致）
TARGETS := windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# Go 模块代理兜底：受限网络（内网 / 大陆直连）常常连不上 proxy.golang.org，
# 表现为 go build 拉依赖时 `dial tcp 142.251.x.x:443: i/o timeout`（交叉编译到
# windows 时尤其明显：go-isatty / go-strftime 这类平台专属依赖此前没被缓存过）。
# 仅在调用方未显式设置 GOPROXY 时给镜像默认值，保留 `GOPROXY=off` 等离线用法
# （与 build.ps1、tools/smoke/lib/harness.mjs 的兜底保持一致）。
GOPROXY ?= https://goproxy.cn,direct
export GOPROXY

.PHONY: build dist web verify test vet fmt fmt-fix lint smoke smoke-phase1 smoke-phase2 smoke-phase3 smoke-phase4 smoke-phase5 tidy clean

build: web
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(DIST)/agoramodel ./cmd/agoramodel

# 前端构建：web/package.json 建立（Phase 4 · T4.1）后才真正生效
web:
	@if [ -f web/package.json ]; then \
		echo "[web] npm ci && npm run build"; \
		npm --prefix web ci && npm --prefix web run build; \
	else \
		echo "[web] 跳过：web/package.json 尚未创建（Phase 4 · T4.1）"; \
	fi

dist: web
	@mkdir -p $(DIST)
	@set -e; for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		out=$(DIST)/agoramodel-$$os-$$arch; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo ">> $$os/$$arch -> $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $$out $(CMD); \
	done
	@$(MAKE) verify

# 断言 CGO_ENABLED=0（优先用 go version -m，跨平台可用）
verify:
	@echo "[verify] 检查产物构建参数（CGO_ENABLED / GOOS / GOARCH）"
	@for f in $(DIST)/agoramodel-*; do \
		echo "  $$f"; \
		go version -m "$$f" 2>/dev/null | grep -E "CGO_ENABLED|GOOS|GOARCH" | sed 's/^/    /' || true; \
	done
	@echo "[verify] 检查本机平台产物为静态链接"
	@host="$$(go env GOHOSTOS)-$$(go env GOHOSTARCH)"; \
	f="$(DIST)/agoramodel-$$host"; \
	if [ ! -f "$$f" ]; then \
		echo "  跳过：本机平台产物 $$f 不存在"; \
	elif ! command -v ldd >/dev/null 2>&1; then \
		echo "  跳过：本机没有 ldd"; \
	else \
		if LC_ALL=C ldd "$$f" 2>&1 | grep -qE "not a dynamic executable|statically linked"; then \
			echo "  $$f 静态链接 OK"; \
		else \
			echo "  WARN $$f 可能动态链接："; LC_ALL=C ldd "$$f" || true; \
		fi; \
	fi

tidy:
	go mod tidy

# ---------------------------------------------------------------- 测试与静态检查
#
# 与 CI（.github/workflows/ci.yml）保持一致：单测一律 CGO_ENABLED=0，
# 与发布产物同基线；race 检测需要 cgo，单独在 CI 里跑。

test:
	CGO_ENABLED=0 go test ./... -count=1

vet:
	CGO_ENABLED=0 go vet ./...

# 只检查不修改：CI 与本地提交前都用它，避免「本地 gofmt 悄悄改了文件」
fmt:
	@out="$$(gofmt -l cmd internal)"; \
	if [ -n "$$out" ]; then \
		echo "[fmt] 以下文件未按 gofmt 格式化："; echo "$$out"; \
		echo "[fmt] 执行 make fmt-fix 修复"; exit 1; \
	fi; \
	echo "[fmt] cmd / internal 均已格式化"

fmt-fix:
	gofmt -w cmd internal

lint: vet fmt

# ---------------------------------------------------------------- 端到端冒烟
#
# 冒烟脚本是 Node（tools/smoke/*.mjs），三个平台共用同一份；
# 各阶段自建临时数据目录、自带 mock 上游，互不干扰，可单独执行。

smoke: smoke-phase1 smoke-phase2 smoke-phase3 smoke-phase4 smoke-phase5

smoke-phase1:
	node tools/smoke/phase1.mjs

smoke-phase2:
	node tools/smoke/phase2.mjs

smoke-phase3:
	node tools/smoke/phase3.mjs

smoke-phase4:
	node tools/smoke/phase4.mjs

smoke-phase5:
	node tools/smoke/phase5.mjs

clean:
	rm -rf $(DIST)
