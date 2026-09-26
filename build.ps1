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
  pwsh -File build.ps1 -Version 0.1.0  # 指定版本号
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

if (-not $Version) {
    $Version = (git describe --tags --always --dirty 2>$null)
    if (-not $Version) { $Version = 'dev' }
}

$dist = Join-Path $PSScriptRoot 'dist'
$cmd = './cmd/agoramodel'
$ldflags = "-s -w -X main.version=$Version"

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
        & go build -trimpath -ldflags $ldflags -o $out $cmd
        if ($LASTEXITCODE -ne 0) { throw "构建失败：$Os/$Arch" }
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

Write-Host "完成（version=$Version，target=$Target）"
exit 0
