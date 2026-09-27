# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。

<#
.SYNOPSIS
  AgoraModel 构建脚本（Windows）。

.DESCRIPTION
  一次性产出三平台六份单文件产物（Windows / Linux / macOS × amd64 / arm64），
  并在构建后校验 CGO 已关闭（与 docs/DESIGN.md ADR-001 的跨平台基线一致）。

.EXAMPLE
  pwsh -File build.ps1                 # 默认 dist：构建六份产物并校验
  pwsh -File build.ps1 -Target build   # 只构建当前平台
  pwsh -File build.ps1 -Version 0.2.0  # 指定版本号
#>
[CmdletBinding()]
param(
    [ValidateSet('dist', 'build', 'web', 'verify', 'clean')]
    [string]$Target = 'dist',
    [string]$Version = ''
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

# 统一工具链探测：处理「刚装好 Go / Node 但当前进程 PATH 未刷新」导致的 go 找不到
. (Join-Path $PSScriptRoot 'tools\lib\toolchain.ps1')
Add-ToolchainToPath

# Go 模块代理兜底：受限网络（内网 / 大陆直连）常常连不上 proxy.golang.org，
# 表现为 go build 拉依赖时 dial tcp 超时。仅在调用方未显式设置 GOPROXY 时给出镜像默认值，
# 保留环境变量覆盖能力（镜像与 tools/smoke/*.ps1 保持一致）。
if (-not $env:GOPROXY) {
    $env:GOPROXY = 'https://goproxy.cn,direct'
    Write-Host "[toolchain] GOPROXY=$env:GOPROXY（默认镜像；可设环境变量覆盖）" -ForegroundColor DarkGray
}

if (-not $Version) {
    # 版本号优先取最近的 semver tag；仓库还没有 tag 时不注入，
    # 由 cmd/agoramodel 内置的版本号兜底（页面上显示可读版本号而不是构建哈希）
    $Version = (git describe --tags --abbrev=0 2>$null)
}
if ($Version) {
    # 去掉 semver tag 的 v 前缀：控制台统一按 "v<version>" 展示，避免出现 vv0.2.0
    $Version = $Version -replace '^v', ''
}

$dist = Join-Path $PSScriptRoot 'dist'
$cmd = './cmd/agoramodel'
$ldflags = '-s -w'
if ($Version) {
    $ldflags += " -X main.version=$Version"
}

$targets = @(
    [pscustomobject]@{ Os = 'windows'; Arch = 'amd64' }
    [pscustomobject]@{ Os = 'windows'; Arch = 'arm64' }
    [pscustomobject]@{ Os = 'linux';   Arch = 'amd64' }
    [pscustomobject]@{ Os = 'linux';   Arch = 'arm64' }
    [pscustomobject]@{ Os = 'darwin';  Arch = 'amd64' }
    [pscustomobject]@{ Os = 'darwin';  Arch = 'arm64' }
)

function Invoke-WebBuild {
    $pkg = Join-Path $PSScriptRoot 'web\package.json'
    if (Test-Path $pkg) {
        Write-Host '[web] npm ci && npm run build'
        Push-Location (Join-Path $PSScriptRoot 'web')
        try {
            npm ci
            if ($LASTEXITCODE -ne 0) { throw 'npm ci 失败' }
            npm run build
            if ($LASTEXITCODE -ne 0) { throw 'npm run build 失败' }
        }
        finally { Pop-Location }
    }
    else {
        Write-Host '[web] 跳过：web/package.json 尚未创建（Phase 4 · T4.1）'
    }
}

function Invoke-GoBuild {
    param([string]$Os, [string]$Arch)

    $name = "agoramodel-$Os-$Arch"
    if ($Os -eq 'windows') { $name += '.exe' }
    $out = Join-Path $dist $name

    Write-Host ">> $Os/$Arch -> $out"
    $env:CGO_ENABLED = '0'
    $env:GOOS = $Os
    $env:GOARCH = $Arch
    try {
        # 输出经临时文件捕获：既避免原生命令 stderr 与 $ErrorActionPreference='Stop' 相互干扰，
        # 又能在失败时区分「依赖下载失败」和普通编译错误。
        $log = Join-Path ([System.IO.Path]::GetTempPath()) "agoramodel-build-$Os-$Arch.log"
        & go build -trimpath -ldflags $ldflags -o $out $cmd > $log 2>&1
        if ($LASTEXITCODE -ne 0) {
            $text = if (Test-Path $log) { (Get-Content $log -Raw) } else { '' }
            if ($text) { Write-Host $text.TrimEnd() }
            if ($text -match 'dial tcp|connectex|proxy\.golang\.org|no such host|i/o timeout|connection refused|TLS handshake|x509') {
                throw @"
构建失败：$Os/$Arch —— Go 依赖下载失败，连不上模块代理。
当前 GOPROXY=$env:GOPROXY

请任选其一后重试：
  1) 临时改用镜像（仅当前终端）：`$env:GOPROXY = 'https://goproxy.cn,direct'
  2) 持久化到本机：go env -w GOPROXY=https://goproxy.cn,direct
  3) 依赖已在本机缓存时强制离线：`$env:GOPROXY = 'off'
"@
            }
            throw "构建失败：$Os/$Arch"
        }
        Remove-Item $log -ErrorAction SilentlyContinue
    }
    finally {
        Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
        $env:CGO_ENABLED = '0'
    }
}

function Invoke-Verify {
    Write-Host '[verify] 检查产物构建参数（CGO_ENABLED / GOOS / GOARCH）'
    Get-ChildItem (Join-Path $dist 'agoramodel-*') | ForEach-Object {
        Write-Host "  $($_.Name)"
        & go version -m $_.FullName 2>$null |
            Select-String -Pattern 'CGO_ENABLED|^\s+build\s+GOOS|GOARCH' |
            ForEach-Object { Write-Host "    $($_.Line.Trim())" }
    }
}

switch ($Target) {
    'web' { Invoke-WebBuild }
    'clean' {
        if (Test-Path $dist) { Remove-Item -Recurse -Force $dist }
        Write-Host '[clean] 已删除 dist/'
    }
    'build' {
        Invoke-WebBuild
        New-Item -ItemType Directory -Force -Path $dist | Out-Null
        $hostArch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
        Invoke-GoBuild -Os 'windows' -Arch $hostArch
    }
    'verify' { Invoke-Verify }
    'dist' {
        Invoke-WebBuild
        New-Item -ItemType Directory -Force -Path $dist | Out-Null
        foreach ($t in $targets) { Invoke-GoBuild -Os $t.Os -Arch $t.Arch }
        Invoke-Verify
    }
}

$versionLabel = if ($Version) { $Version } else { '内置版本' }
Write-Host "完成（version=$versionLabel，target=$Target）"
exit 0
