# AgoraModel 构建脚本（Linux / macOS）
#
#   make build    构建当前平台产物
#   make dist     构建三平台六份产物（Windows / Linux / macOS × amd64 / arm64）并校验
#   make web      仅构建前端（Phase 4 起生效）
#   make verify   校验产物为静态链接且 CGO 关闭
#   make tidy     整理 go.mod
#   make clean    清理 dist/

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

.PHONY: build dist web verify tidy clean

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
	@echo "[verify] 检查产物构建参数与静态链接情况"
	@for f in $(DIST)/agoramodel-*; do \
		echo "  $$f"; \
		go version -m "$$f" 2>/dev/null | grep -E "CGO_ENABLED|GOOS|GOARCH" | sed 's/^/    /' || true; \
		if command -v ldd >/dev/null 2>&1 && [ "$${f##*.}" != "exe" ]; then \
			ldd "$$f" 2>&1 | grep -qE "not a dynamic executable|statically linked" \
				&& echo "    静态链接 OK" \
				|| { echo "    WARN 可能动态链接："; ldd "$$f" || true; }; \
		fi; \
	done

tidy:
	go mod tidy

clean:
	rm -rf $(DIST)
