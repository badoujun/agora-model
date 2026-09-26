# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  统一的工具链 PATH 探测：确保 go / node / npm 在当前进程可用。

.DESCRIPTION
  通过 winget 等方式安装工具时，通常只更新了「系统/用户 PATH」，
  而已经打开的终端（或父进程）仍持有旧 PATH，于是脚本里调用 go 会报
  "无法将 go 项识别为 cmdlet..."。

  本函数做两件事：
    1) 把机器级 + 用户级 PATH 合并进当前进程；
    2) 若仍找不到 go，再探测常见安装目录（含 winget 包目录）兜底。

.EXAMPLE
  . .\tools\lib\toolchain.ps1
  Add-ToolchainToPath
#>
function Add-ToolchainToPath {
    [CmdletBinding()]
    param(
        # 静默模式：不输出探测信息（供 CI 使用）
        [switch]$Quiet
    )

    # 1) 合并机器级 / 用户级 PATH —— 解决「刚装完工具但当前进程 PATH 未刷新」
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @($machinePath, $userPath, $env:Path) | Where-Object { $_ -and $_.Trim() -ne '' }
    $env:Path = ($parts -join ';')

    # 2) 仍找不到 go 时，探测常见安装位置
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        $candidates = New-Object System.Collections.ArrayList
        if ($env:ProgramFiles) { [void]$candidates.Add((Join-Path $env:ProgramFiles 'Go\bin')) }
        if (${env:ProgramFiles(x86)}) { [void]$candidates.Add((Join-Path ${env:ProgramFiles(x86)} 'Go\bin')) }
        if ($env:LOCALAPPDATA) { [void]$candidates.Add((Join-Path $env:LOCALAPPDATA 'Programs\Go\bin')) }
        [void]$candidates.Add('C:\Go\bin')
        [void]$candidates.Add('/usr/local/go/bin')

        foreach ($dir in $candidates) {
            $exe = Join-Path $dir 'go.exe'
            $bin = Join-Path $dir 'go'
            if ((Test-Path $exe) -or (Test-Path $bin)) {
                $env:Path = "$dir;$env:Path"
                if (-not $Quiet) { Write-Host "[toolchain] 已将 $dir 加入 PATH" -ForegroundColor DarkGray }
                break
            }
        }

        # winget 的包目录结构较深，做一次受限搜索兜底
        if (-not (Get-Command go -ErrorAction SilentlyContinue) -and $env:LOCALAPPDATA) {
            $pkgRoot = Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Packages'
            if (Test-Path $pkgRoot) {
                $found = Get-ChildItem -Path $pkgRoot -Filter 'go.exe' -Recurse -ErrorAction SilentlyContinue |
                    Select-Object -First 1
                if ($found) {
                    $env:Path = "$($found.DirectoryName);$env:Path"
                    if (-not $Quiet) { Write-Host "[toolchain] 已将 $($found.DirectoryName) 加入 PATH" -ForegroundColor DarkGray }
                }
            }
        }
    }

    # 3) 明确报错，而不是让脚本以晦涩的方式失败
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw @'
未找到 Go 工具链。请任选其一：
  1) 安装 Go 1.25+（Windows: winget install GoLang.Go），然后重开终端；
  2) 若已安装，请把 Go 的 bin 目录加入 PATH（通常为 C:\Program Files\Go\bin）；
  3) 临时指定：$env:Path = "C:\Program Files\Go\bin;$env:Path"
'@
    }
    if (-not (Get-Command node -ErrorAction SilentlyContinue) -and -not $Quiet) {
        Write-Warning '未找到 node：前端构建会被跳过（需要 Web UI 请安装 Node 20+ 并重开终端）。'
    }

    if (-not $Quiet) {
        $goVersion = ((& go version) -join ' ').Trim()
        Write-Host "[toolchain] $goVersion"
        $node = Get-Command node -ErrorAction SilentlyContinue
        if ($node) { Write-Host "[toolchain] node $(((& node --version) -join '').Trim())" }
    }
}
