# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  Phase 2 端到端冒烟：SQLite 持久化、凭证加密、网关 Key 生命周期、SSRF 校验。

.DESCRIPTION
  覆盖：首次启动（生成主密钥 / 导入供应商 / 生成网关 Key）、请求日志落库与脱敏、
        重启复用（不重复导入、原 Key 仍有效）、网关 Key 重置（旧 Key 立即失效）、
        allow_internal=false 时内网地址被拒、主密钥丢失时不静默降级。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File tools/smoke/phase2.ps1
#>
[CmdletBinding()]
param(
    [int]$Port = 19091,
    [int]$MockPort = 9999
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repoRoot

. (Join-Path (Split-Path -Parent $PSScriptRoot) 'lib\toolchain.ps1')
Add-ToolchainToPath -Quiet
$env:CGO_ENABLED = '0'

$tmp = [System.IO.Path]::GetTempPath()
$enc = New-Object System.Text.UTF8Encoding($false)
$results = New-Object System.Collections.ArrayList

function Assert-That {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    [void]$results.Add([pscustomobject]@{ Name = $Name; Ok = $Ok; Detail = $Detail })
    $tag = if ($Ok) { 'PASS' } else { 'FAIL' }
    $suffix = if ($Detail) { " - $Detail" } else { '' }
    Write-Host ("  [{0}] {1}{2}" -f $tag, $Name, $suffix)
}

function Save-Text {
    param([string]$Path, [string]$Content)
    [System.IO.File]::WriteAllText($Path, $Content, $enc)
    return $Path
}

function Read-SharedText {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return '' }
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        $sr = New-Object System.IO.StreamReader($fs, $enc)
        try { return $sr.ReadToEnd() } finally { $sr.Dispose() }
    }
    finally { $fs.Dispose() }
}

function Invoke-Curl {
    param([string]$Url, [string[]]$HeaderArgs, [string]$BodyFile, [string]$Method = 'POST')
    $curlArgs = @('-s', '-w', "`n%{http_code}", '--max-time', '20', '-X', $Method, $Url)
    if ($HeaderArgs) { $curlArgs += $HeaderArgs }
    if ($BodyFile) { $curlArgs += @('--data-binary', "@$BodyFile") }
    $raw = (& curl.exe @curlArgs) -join "`n"
    $idx = $raw.LastIndexOf("`n")
    if ($idx -lt 0) { return [pscustomobject]@{ Code = 0; Body = $raw } }
    $code = 0
    [void][int]::TryParse($raw.Substring($idx + 1).Trim(), [ref]$code)
    return [pscustomobject]@{ Code = $code; Body = $raw.Substring(0, $idx) }
}

function Get-KeyFromLog {
    param([string]$LogPath, [int]$TimeoutMs = 8000)
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ((Get-Date) -lt $deadline) {
        $text = Read-SharedText $LogPath
        if ($text -match 'gateway_key=(gw-[0-9a-f]+)') { return $Matches[1] }
        Start-Sleep -Milliseconds 200
    }
    return $null
}

# 以共享模式读取文件（数据库正被网关占用时也能读）
function Read-SharedBytes {
    param([string]$Path)
    if (-not (Test-Path $Path)) { return $null }
    $fs = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        $len = [int]$fs.Length
        $buf = New-Object byte[] $len
        $read = 0
        while ($read -lt $len) {
            $n = $fs.Read($buf, $read, $len - $read)
            if ($n -le 0) { break }
            $read += $n
        }
        return $buf
    }
    finally { $fs.Dispose() }
}

# 在 DB 与其 WAL 文件中按字节搜索字符串（用于断言明文凭证没有落库 / 日志已落库）
function Find-BytesInDB {
    param([string]$DataDir, [string]$Needle)
    $target = [System.Text.Encoding]::UTF8.GetBytes($Needle)
    $first = $target[0]
    $limit = $target.Length
    foreach ($suffix in @('', '-wal')) {
        $bytes = Read-SharedBytes (Join-Path $DataDir ("agora.db" + $suffix))
        if ($null -eq $bytes) { continue }
        for ($i = 0; $i -le $bytes.Length - $limit; $i++) {
            if ($bytes[$i] -ne $first) { continue }
            $match = $true
            for ($j = 1; $j -lt $limit; $j++) {
                if ($bytes[$i + $j] -ne $target[$j]) { $match = $false; break }
            }
            if ($match) { return $true }
        }
    }
    return $false
}

function Start-Gateway {
    param([string]$DataDir, [string]$ConfigPath, [string]$LogPath, [int]$Port, [switch]$ResetKey)
    $gwArgs = @('--config', $ConfigPath, '--data-dir', $DataDir, '--port', "$Port", '--log-level', 'debug')
    if ($ResetKey) { $gwArgs += '--reset-gateway-key' }
    return Start-Process $exe -ArgumentList $gwArgs -PassThru `
        -RedirectStandardOutput $LogPath -RedirectStandardError "$LogPath.err" -WindowStyle Hidden
}

Write-Host '== 构建开发二进制 =='
$exe = Join-Path $tmp ('agoramodel-p2' + $(if ($env:OS -eq 'Windows_NT') { '.exe' } else { '' }))
& go build -trimpath -o $exe ./cmd/agoramodel
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }

$dataDirA = Join-Path $tmp 'agora-p2-data'
$dataDirB = Join-Path $tmp 'agora-p2-internal'
$dataDirC = Join-Path $tmp 'agora-p2-lost-key'
foreach ($d in @($dataDirA, $dataDirB, $dataDirC)) {
    if (Test-Path $d) { Remove-Item -Recurse -Force $d }
}

$cfgOk = Save-Text (Join-Path $tmp 'agora-p2-ok.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $Port, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "mock", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "anthropic_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-mock-provider-key", "models": ["mock-gpt-4o"],
      "priority": 10, "allow_internal": true }
  ]
}
"@

$cfgBad = Save-Text (Join-Path $tmp 'agora-p2-internal.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $Port },
  "providers": [
    { "id": "internal", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-internal", "models": ["m"], "allow_internal": false }
  ]
}
"@

$bodyFile = Save-Text (Join-Path $tmp 'agora-p2-req.json') '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}'
$base = "http://127.0.0.1:$Port"
$logA = Join-Path $tmp 'agora-p2-a.out'
$logB = Join-Path $tmp 'agora-p2-b.out'
$logC = Join-Path $tmp 'agora-p2-c.out'
$logD = Join-Path $tmp 'agora-p2-d.out'
$logE = Join-Path $tmp 'agora-p2-e.out'

$env:PORT = "$MockPort"
$mockOut = Join-Path $tmp 'agora-p2-mock.out'
$mock = Start-Process node -ArgumentList 'tools/mock-upstream/server.mjs' -PassThru `
    -RedirectStandardOutput $mockOut -RedirectStandardError "$mockOut.err" -WindowStyle Hidden
Start-Sleep -Seconds 2

$procs = New-Object System.Collections.ArrayList
try {
    Write-Host '== 断言 =='

    # ---------- A) 首次启动 ----------
    $gwA = Start-Gateway -DataDir $dataDirA -ConfigPath $cfgOk -LogPath $logA -Port $Port
    [void]$procs.Add($gwA)
    Start-Sleep -Seconds 2
    $key1 = Get-KeyFromLog $logA
    Assert-That '首次启动打印网关 Key 明文（仅一次）' ($null -ne $key1) "hint=$(if ($key1) { $key1.Substring(0, 7) + '***' })"

    $logAText = Read-SharedText $logA
    Assert-That '首次启动从引导配置导入供应商' ($logAText -match '已从引导配置导入供应商' -and $logAText -match 'count=1')
    Assert-That '首次启动生成主密钥文件' ($logAText -match '已生成新的主密钥文件')

    Assert-That 'healthz 返回 200' ((Invoke-Curl "$base/healthz" -Method GET).Code -eq 200)

    $auth = @('-H', 'content-type: application/json', '-H', "authorization: Bearer $key1")
    $resp = Invoke-Curl "$base/v1/chat/completions" $auth $bodyFile
    Assert-That '使用数据库中的网关 Key 可完成透传' ($resp.Code -eq 200 -and $resp.Body -match 'mock') "code=$($resp.Code)"
    Assert-That '错误网关 Key 被拒' ((Invoke-Curl "$base/v1/chat/completions" @('-H', 'content-type: application/json', '-H', 'authorization: Bearer gw-nope') $bodyFile).Code -eq 401)

    $masterPath = Join-Path $dataDirA 'master.key'
    Assert-That '数据目录含 agora.db 与 master.key' ((Test-Path (Join-Path $dataDirA 'agora.db')) -and (Test-Path $masterPath))
    $masterText = (Read-SharedText $masterPath).Trim()
    Assert-That 'master.key 为 64 位 hex' ($masterText -match '^[0-9a-f]{64}$')

    Start-Sleep -Milliseconds 1500  # 等待异步日志批量落库
    Assert-That '请求日志已落库（可按模型检索到）' (Find-BytesInDB -DataDir $dataDirA -Needle 'mock-gpt-4o')
    Assert-That '数据库中不含供应商凭证明文' (-not (Find-BytesInDB -DataDir $dataDirA -Needle 'sk-mock-provider-key'))

    Stop-Process -Id $gwA.Id -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500

    # ---------- B) 重启复用 ----------
    $gwB = Start-Gateway -DataDir $dataDirA -ConfigPath $cfgOk -LogPath $logB -Port $Port
    [void]$procs.Add($gwB)
    Start-Sleep -Seconds 2
    $logBText = Read-SharedText $logB
    Assert-That '重启不再生成网关 Key（读库复用）' ($logBText -notmatch '已生成网关 Key')
    Assert-That '重启不再重复导入供应商' ($logBText -notmatch '已从引导配置导入')
    Assert-That '重启后原网关 Key 仍有效' ((Invoke-Curl "$base/v1/chat/completions" $auth $bodyFile).Code -eq 200)
    Stop-Process -Id $gwB.Id -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500

    # ---------- C) 网关 Key 重置 ----------
    $gwC = Start-Gateway -DataDir $dataDirA -ConfigPath $cfgOk -LogPath $logC -Port $Port -ResetKey
    [void]$procs.Add($gwC)
    Start-Sleep -Seconds 2
    $key2 = Get-KeyFromLog $logC
    Assert-That '重置后打印新 Key 且与旧 Key 不同' ($null -ne $key2 -and $key2 -ne $key1)
    $auth2 = @('-H', 'content-type: application/json', '-H', "authorization: Bearer $key2")
    Assert-That '重置后新 Key 可用' ((Invoke-Curl "$base/v1/chat/completions" $auth2 $bodyFile).Code -eq 200)
    Assert-That '重置后旧 Key 立即失效（401）' ((Invoke-Curl "$base/v1/chat/completions" $auth $bodyFile).Code -eq 401)
    Stop-Process -Id $gwC.Id -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500

    # ---------- D) SSRF 校验 ----------
    $gwD = Start-Gateway -DataDir $dataDirB -ConfigPath $cfgBad -LogPath $logD -Port $Port
    [void]$procs.Add($gwD)
    $exitedD = $gwD.WaitForExit(10000)
    $logDText = Read-SharedText $logD
    Assert-That 'allow_internal=false 时内网地址被拒并启动失败' $exitedD "exited=$exitedD"
    Assert-That 'SSRF 拒绝信息可读（提示内网地址）' ($logDText -match '内网')
    if (-not $exitedD) { Stop-Process -Id $gwD.Id -Force -ErrorAction SilentlyContinue }

    # ---------- E) 主密钥丢失 ----------
    Copy-Item -Recurse -Force $dataDirA $dataDirC
    Remove-Item (Join-Path $dataDirC 'master.key') -Force -ErrorAction SilentlyContinue
    $gwE = Start-Gateway -DataDir $dataDirC -ConfigPath $cfgOk -LogPath $logE -Port $Port
    [void]$procs.Add($gwE)
    $exitedE = $gwE.WaitForExit(10000)
    $logEText = Read-SharedText $logE
    Assert-That '主密钥丢失时启动失败（不静默降级）' ($exitedE -and ($logEText -match '解密失败')) "exited=$exitedE"
    if (-not $exitedE) { Stop-Process -Id $gwE.Id -Force -ErrorAction SilentlyContinue }
}
finally {
    foreach ($p in $procs) {
        if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    }
    if ($mock -and -not $mock.HasExited) { Stop-Process -Id $mock.Id -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 400
}

$failed = @($results | Where-Object { -not $_.Ok })
Write-Host ''
Write-Host ("== 冒烟结果：{0}/{1} 通过 ==" -f ($results.Count - $failed.Count), $results.Count)
if ($failed.Count -gt 0) {
    $failed | ForEach-Object { Write-Host ("  FAIL: {0} ({1})" -f $_.Name, $_.Detail) }
    exit 1
}
Write-Host '全部通过'
exit 0
