#!/usr/bin/env bash
# AgoraModel Linux / macOS 开发环境预检。
#
# 与 Windows 侧的 tools/lib/toolchain.ps1 对应：那边的痛点是「刚装的工具没进当前
# 终端 PATH」，这边的痛点是「发行版自带的 Go 太旧（go.mod 要求 1.25+）」，
# 所以这里显式做版本比对，而不是只判断命令存不存在。
#
# 用法：
#   ./tools/dev/preflight.sh            # 预检并给出安装指引
#   ./tools/dev/preflight.sh --quiet    # 只回退出码（供脚本 / CI 前置判断）
#
# 退出码：0 全部就绪；1 缺少必需工具或版本不满足。

set -uo pipefail

QUIET=0
if [ "${1:-}" = "--quiet" ]; then
  QUIET=1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

MISSING=0
WARNINGS=0

say() {
  [ "$QUIET" -eq 1 ] && return 0
  printf '%s\n' "$*"
}

ok() {
  [ "$QUIET" -eq 1 ] && return 0
  printf '  [ok]   %s\n' "$*"
}

fail() {
  [ "$QUIET" -eq 1 ] && return 0
  printf '  [缺少] %s\n' "$*"
}

warn() {
  [ "$QUIET" -eq 1 ] && return 0
  printf '  [注意] %s\n' "$*"
}

# 把版本号规范成「主.次.补丁」便于比较（去掉 v 前缀与预发布后缀）
normalize_version() {
  printf '%s' "$1" | sed -E 's/^v//' | sed -E 's/[-+].*$//'
}

# version_ge A B → A >= B
version_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

# go.mod 里声明的 Go 版本是唯一事实来源，避免文档与代码各说一套
required_go="$(sed -nE 's/^go[[:space:]]+([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/p' go.mod | head -n1)"
required_go="${required_go:-1.25}"

say "== AgoraModel 开发环境预检（Linux / macOS）=="
say ""

# ------------------------------------------------------------------ Go 工具链
if command -v go >/dev/null 2>&1; then
  go_raw="$(go version | awk '{print $3}')"
  go_ver="$(normalize_version "$go_raw")"
  go_path="$(command -v go)"
  if version_ge "$go_ver" "$required_go"; then
    ok "Go $go_ver（$go_path；go.mod 要求 >= $required_go）"
  else
    fail "Go $go_ver 低于 go.mod 要求的 $required_go（modernc.org/sqlite 需要新版工具链）"
    MISSING=1
  fi
  # 机器上装了多个 Go 时，PATH 里排在前面的那个才生效；临时解压的工具链
  # （如 /tmp/go/bin/go）混进 PATH 会让人误判「构建用的到底是哪个版本」。
  case "$go_path" in
    /usr/bin/go | /bin/go | /usr/local/go/bin/go | /opt/homebrew/bin/go | \
      "$HOME"/.local/go/bin/go | "$HOME"/go/bin/go | "$HOME"/sdk/*/bin/go | /usr/lib/go*/bin/go) ;;
    *)
      warn "go 来自非标准位置（$go_path）：若机器上装了多个 Go，生效的是 PATH 中最靠前的这个"
      WARNINGS=1
      ;;
  esac
else
  fail "未找到 go（go.mod 要求 >= $required_go）"
  MISSING=1
fi

# ------------------------------------------------------------------ Node / npm
# Node 只用于构建前端（web/）与跑 tools/smoke/*.mjs；不参与 Go 产物构建。
if command -v node >/dev/null 2>&1; then
  node_ver="$(normalize_version "$(node -v)")"
  if version_ge "$node_ver" "20.0.0"; then
    ok "Node $node_ver（>= 20，用于前端构建与 tools/smoke/*.mjs）"
  else
    warn "Node $node_ver 偏旧，建议 >= 20（Vite 6 与冒烟脚本按 Node 20+ 验证）"
    WARNINGS=1
  fi
else
  fail "未找到 node（>= 20，前端构建与端到端冒烟都需要）"
  MISSING=1
fi

if command -v npm >/dev/null 2>&1; then
  ok "npm $(npm -v)"
else
  fail "未找到 npm（随 Node 一起安装）"
  MISSING=1
fi

# ------------------------------------------------------------ 构建 / 版本控制
if command -v make >/dev/null 2>&1; then
  ok "make $(make -v | head -n1 | awk '{print $3}')"
else
  fail "未找到 make（Linux / macOS 用 Makefile 作为统一入口；Windows 用 build.ps1）"
  MISSING=1
fi

if command -v git >/dev/null 2>&1; then
  ok "git $(git --version | awk '{print $3}')"
else
  fail "未找到 git（构建脚本用 git describe 推导版本号）"
  MISSING=1
fi

# 行尾一致性：仓库以 LF 存储，但 Windows 上克隆过一次的文件可能是 CRLF
if [ -f .gitattributes ] && command -v git >/dev/null 2>&1; then
  if [ -n "$(git ls-files --eol | grep -E 'i/crlf' || true)" ]; then
    warn "工作区存在 CRLF 索引文件，建议执行：git add --renormalize ."
    WARNINGS=1
  else
    ok "行尾统一（LF 存储，.ps1 除外）"
  fi
fi

say ""

if [ "$MISSING" -ne 0 ]; then
  cat <<'GUIDE'
== 安装指引 ==

Go（二选一，注意需要 go.mod 声明的版本以上）：
  1) 官方 tarball（无需 root，装到 ~/.local/go）：
       curl -fsSL -o /tmp/go.tgz https://golang.google.cn/dl/go1.27.1.linux-amd64.tar.gz
       mkdir -p ~/.local && rm -rf ~/.local/go && tar -C ~/.local -xzf /tmp/go.tgz
       export PATH="$HOME/.local/go/bin:$PATH"      # 建议写进 ~/.bashrc
  2) 发行版仓库（版本可能偏低，装完执行 go version 确认）：
       Debian / Ubuntu: sudo apt install golang-go

Node 20+（三选一）：
  1) nvm：      curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash && nvm install 20
  2) NodeSource：curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash - && sudo apt install -y nodejs
  3) 发行版仓库：sudo apt install nodejs npm（可能低于 20，仅够跑冒烟）

macOS：brew install go node make

装完重跑本脚本确认：./tools/dev/preflight.sh
GUIDE
  exit 1
fi

say "环境就绪。常用命令："
say "  make lint          静态检查（go vet + gofmt）"
say "  make test          单元测试"
say "  make dist          构建三平台六份产物"
say "  make smoke         端到端冒烟 phase1–5"
[ "$WARNINGS" -ne 0 ] && say "（存在上面的「注意」项，通常不阻塞开发）"
exit 0
