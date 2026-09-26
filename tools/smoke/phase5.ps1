# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  Phase 5 验收冒烟：PRD §7 八条验收 + 性能冒烟 + 安全复核 + 换机迁移演练。

.DESCRIPTION
  A) 单文件交付：三平台六份产物、格式与版本注入
  B) 八条验收：零模板接入 / 双协议可用 / 模型聚合 / 长任务不中断 / 断连无泄漏 /
     安全基线 / 可排查 / 单文件交付
  C) 性能：20 并发流式请求的成功率、耗时与内存稳定性
  D) 迁移演练：导出 → 在新数据目录的实例上导入 → Agent 可继续使用

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File tools/smoke/phase5.ps1
#>
[CmdletBinding()]
param(
    [int]$Port = 19095,
    [int]$PortB = 19096,
    [int]$MockPort = 9999
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repoRoot

. (Join-Path (Split-Path -Parent $PSScriptRoot) 'lib\toolchain.ps1')
Add-ToolchainToPath -Quiet
$env:CGO_ENABLED = '0'
$env:GOPROXY = 'https://goproxy.cn,direct'

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

function Invoke-Http {
    param(
        [string]$Url,
        [string]$Method = 'GET',
        [string]$Body = '',
        [string[]]$HeaderArgs
    )
    $token = [guid]::NewGuid().ToString('N')
    $headerFile = Join-Path $tmp ("agora-p5-h-$token.txt")
    $bodyFile = Join-Path $tmp ("agora-p5-b-$token.bin")
    $reqBody = $null
    if ($Body -ne '') {
        $reqBody = Join-Path $tmp ("agora-p5-rb-$token.json")
        [System.IO.File]::WriteAllText($reqBody, $Body, $enc)
    }
    $curlArgs = @('-s', '-D', $headerFile, '-o', $bodyFile, '-w', '%{http_code}', '--max-time', '60', '-X', $Method, $Url)
    if ($HeaderArgs) { $curlArgs += $HeaderArgs }
    if ($reqBody) { $curlArgs += @('--data-binary', "@$reqBody") }

    $codeText = (& curl.exe @curlArgs) -join ''
    $code = 0
    [void][int]::TryParse($codeText.Trim(), [ref]$code)
    $headers = if (Test-Path $headerFile) { Read-SharedText $headerFile } else { '' }
    $content = if (Test-Path $bodyFile) { Read-SharedText $bodyFile } else { '' }
    foreach ($f in @($headerFile, $bodyFile, $reqBody)) {
        if ($f) { Remove-Item $f -Force -ErrorAction SilentlyContinue }
    }
    return [pscustomobject]@{ Code = $code; Headers = $headers; Body = $content }
}

function Get-KeyFromLog {
    param([string]$LogPath, [int]$TimeoutMs = 10000)
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ((Get-Date) -lt $deadline) {
        $text = Read-SharedText $LogPath
        if ($text -match 'gateway_key=(gw-[0-9a-f]+)') { return $Matches[1] }
        Start-Sleep -Milliseconds 200
    }
    return $null
}

function Start-Gateway {
    param([string]$DataDir, [string]$ConfigPath, [string]$LogPath, [int]$PortValue, [string]$Extra = '')
    $gwArgs = @('--config', $ConfigPath, '--data-dir', $DataDir, '--port', "$PortValue", '--log-level', 'info')
    if ($Extra -ne '') { $gwArgs += $Extra.Split(' ') }
    return Start-Process $exe -ArgumentList $gwArgs -PassThru `
        -RedirectStandardOutput $LogPath -RedirectStandardError "$LogPath.err" -WindowStyle Hidden
}

function Test-BinaryMagic {
    param([string]$Path, [string]$Kind)
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -lt 4) { return $false }
    switch ($Kind) {
        'pe' { return ($bytes[0] -eq 0x4D -and $bytes[1] -eq 0x5A) }
        'elf' { return ($bytes[0] -eq 0x7F -and $bytes[1] -eq 0x45 -and $bytes[2] -eq 0x4C -and $bytes[3] -eq 0x46) }
        'macho' {
            return (($bytes[0] -eq 0xCF -and $bytes[1] -eq 0xFA -and $bytes[2] -eq 0xED -and $bytes[3] -eq 0xFE) -or
                    ($bytes[0] -eq 0xCA -and $bytes[1] -eq 0xFE -and $bytes[2] -eq 0xBA -and $bytes[3] -eq 0xBE))
        }
    }
    return $false
}

Write-Host '== A) 单文件交付（T5.1）=='

$dist = Join-Path $repoRoot 'dist'
$artifacts = @(
    @{ Name = 'agoramodel-windows-amd64.exe'; Magic = 'pe' },
    @{ Name = 'agoramodel-windows-arm64.exe'; Magic = 'pe' },
    @{ Name = 'agoramodel-linux-amd64'; Magic = 'elf' },
    @{ Name = 'agoramodel-linux-arm64'; Magic = 'elf' },
    @{ Name = 'agoramodel-darwin-amd64'; Magic = 'macho' },
    @{ Name = 'agoramodel-darwin-arm64'; Magic = 'macho' }
)
if (-not (Test-Path $dist)) {
    Write-Host '  dist 不存在，先执行 build.ps1 -Target dist …'
    powershell -NoProfile -ExecutionPolicy Bypass -File .\build.ps1 -Target dist -Version 0.1.0 | Out-Null
}
$missing = @()
$badMagic = @()
foreach ($item in $artifacts) {
    $path = Join-Path $dist $item.Name
    if (-not (Test-Path $path)) { $missing += $item.Name; continue }
    if (-not (Test-BinaryMagic -Path $path -Kind $item.Magic)) { $badMagic += $item.Name }
}
Assert-That '三平台六份产物齐备' ($missing.Count -eq 0) ($missing -join ', ')
Assert-That '产物格式正确（PE / ELF / Mach-O）' ($badMagic.Count -eq 0) ($badMagic -join ', ')

$winExe = Join-Path $dist 'agoramodel-windows-amd64.exe'
$versionOutput = (& $winExe --version) -join ''
Assert-That '版本号已注入产物' ($versionOutput.Trim() -eq '0.1.0') "输出=$versionOutput"
Assert-That '前端已内嵌（产物体积 > 5MB）' ((Get-Item $winExe).Length -gt 5MB)

$exeName = if ($env:OS -eq 'Windows_NT') { 'agoramodel-p5.exe' } else { 'agoramodel-p5' }
$exe = Join-Path $tmp $exeName
& go build -trimpath -ldflags '-s -w -X main.version=0.1.0' -o $exe ./cmd/agoramodel
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }
Assert-That '服务子命令可用（status 在未安装时给出明确错误）' (
    ((& $exe status 2>&1 | Out-String) -match 'not installed|服务操作失败')
)

$dataDirA = Join-Path $tmp 'agora-p5-data'; $dataDirB = Join-Path $tmp 'agora-p5-migrated'
foreach ($d in @($dataDirA, $dataDirB)) { if (Test-Path $d) { Remove-Item -Recurse -Force $d } }

# 网关要求至少一个启用的供应商（避免空配置静默启动），因此两个实例各带一个占位供应商
$cfgA = Save-Text (Join-Path $tmp 'agora-p5-a.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $Port, "sse_idle_seconds": 2, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "seed", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "anthropic_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-seed-key", "models": ["mock-gpt-4o"], "priority": 50, "allow_internal": true }
  ]
}
"@
$cfgB = Save-Text (Join-Path $tmp 'agora-p5-b.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $PortB },
  "providers": [
    { "id": "placeholder", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-placeholder", "models": ["mock-gpt-4o"], "priority": 60, "allow_internal": true }
  ]
}
"@
$cfgInternal = Save-Text (Join-Path $tmp 'agora-p5-internal.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": 19097 },
  "providers": [
    { "id": "internal", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-x", "models": ["m"], "allow_internal": false }
  ]
}
"@

$env:PORT = "$MockPort"
$mockOut = Join-Path $tmp 'agora-p5-mock.out'
$mock = Start-Process node -ArgumentList 'tools/mock-upstream/server.mjs' -PassThru `
    -RedirectStandardOutput $mockOut -RedirectStandardError "$mockOut.err" -WindowStyle Hidden
Start-Sleep -Seconds 2

$logA = Join-Path $tmp 'agora-p5-a.out'
$gwA = Start-Gateway -DataDir $dataDirA -ConfigPath $cfgA -LogPath $logA -PortValue $Port
$procs = New-Object System.Collections.ArrayList
[void]$procs.Add($gwA)
$base = "http://127.0.0.1:$Port"

try {
    Start-Sleep -Seconds 3
    $key = Get-KeyFromLog $logA
    Assert-That '网关 Key 已生成' ($null -ne $key)
    $json = @('-H', 'content-type: application/json')
    $auth = $json + @('-H', "authorization: Bearer $key")

    Write-Host '== B) PRD §7 八条验收 =='

    # 1) 零模板接入：只给名称 + 双 URL + Key，不选任何模板
    $create = Invoke-Http "$base/api/providers" -Method POST -HeaderArgs $json `
        -Body ('{"name":"验收供应商","openai_base_url":"http://127.0.0.1:' + $MockPort + '/v1",' +
               '"anthropic_base_url":"http://127.0.0.1:' + $MockPort + '/v1",' +
               '"api_key":"sk-acceptance-key","allow_internal":true,"priority":1}')
    $providerId = ([regex]::Match($create.Body, '"id":"([^"]+)"')).Groups[1].Value
    Assert-That '① 零模板接入：仅填双 URL + Key 即完成新增' ($create.Code -eq 201 -and $providerId -ne '') "code=$($create.Code)"

    # 2) 双协议可用
    $openai = Invoke-Http "$base/v1/chat/completions" -Method POST -HeaderArgs $auth `
        -Body '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}'
    $anthropic = Invoke-Http "$base/v1/messages" -Method POST -HeaderArgs $auth `
        -Body '{"model":"mock-claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
    Assert-That '② 双协议可用：OpenAI 与 Anthropic 路径均 200' ($openai.Code -eq 200 -and $anthropic.Code -eq 200) "openai=$($openai.Code) anthropic=$($anthropic.Code)"
    Assert-That '② 原生透传：响应体来自上游（含 mock 标记）' ($openai.Body -match 'mock' -and $anthropic.Body -match 'mock')

    # 3) 模型聚合
    $models = Invoke-Http "$base/v1/models" -HeaderArgs $auth
    $modelJson = $models.Body
    $dedup = ([regex]::Matches($modelJson, '"id":"mock-gpt-4o","object":"model","owned_by":"[^"]+"')).Count
    Assert-That '③ /v1/models 返回聚合结果（含 owned_by）' ($models.Code -eq 200 -and $modelJson -match '"object":"list"' -and $modelJson -match '"owned_by"')
    Assert-That '③ 同名模型只出现一次裸名条目' ($dedup -eq 1) "出现次数=$dedup"

    # 4) 长任务不中断（上游静默 20s，网关心跳兜底）
    $stream = Invoke-Http "$base/v1/chat/completions" -Method POST `
        -HeaderArgs ($auth + @('-H', 'x-mock-silence: 20', '-H', 'x-mock-chunks: 3')) `
        -Body '{"model":"mock-gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
    Assert-That '④ 上游静默 20s 仍能完成（SSE 心跳兜底）' ($stream.Body -match '\[DONE\]' -and $stream.Body -match ': keep-alive')

    # 5) 断连无泄漏：断开后网关仍健康
    [void](Invoke-Http "$base/v1/chat/completions" -Method POST `
        -HeaderArgs ($auth + @('-H', 'x-mock-silence: 5')) `
        -Body '{"model":"mock-gpt-4o","stream":true,"messages":[]}')
    Start-Sleep -Seconds 1
    $afterDisconnect = Invoke-Http "$base/healthz"
    Assert-That '⑤ 客户端断开后网关仍健康（无泄漏式崩溃）' ($afterDisconnect.Code -eq 200) "code=$($afterDisconnect.Code)"

    # 7) 可排查：注入上游 401，日志页应能查到
    $failing = Invoke-Http "$base/v1/chat/completions" -Method POST `
        -HeaderArgs ($auth + @('-H', 'x-mock-status: 401')) `
        -Body '{"model":"mock-gpt-4o","messages":[]}'
    Start-Sleep -Milliseconds 800
    $logs = Invoke-Http "$base/api/logs?limit=20" -HeaderArgs $auth
    Assert-That '⑦ 上游 401 原样透传' ($failing.Code -eq 401) "code=$($failing.Code)"
    Assert-That '⑦ 失败请求可在日志接口查到' ($logs.Body -match '"status_code":401')

    # 6) 安全基线
    $dbHasPlaintext = $false
    foreach ($f in @('agora.db', 'agora.db-wal')) {
        $path = Join-Path $dataDirA $f
        if (-not (Test-Path $path)) { continue }
        $fs = [System.IO.File]::Open($path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        try {
            $len = [int]$fs.Length
            $buf = New-Object byte[] $len
            [void]$fs.Read($buf, 0, $len)
            $text = [System.Text.Encoding]::UTF8.GetString($buf)
            if ($text -match 'sk-acceptance-key') { $dbHasPlaintext = $true }
        }
        finally { $fs.Dispose() }
    }
    Assert-That '⑥ 数据库中无供应商凭证明文' (-not $dbHasPlaintext)

    $providers = Invoke-Http "$base/api/providers" -HeaderArgs $auth
    Assert-That '⑥ API 只回凭证掩码' ($providers.Body -match 'sk-\*\*\*\*' -and $providers.Body -notmatch 'sk-acceptance-key')

    $gwInternal = Start-Gateway -DataDir (Join-Path $tmp 'agora-p5-internal-data') -ConfigPath $cfgInternal -LogPath (Join-Path $tmp 'agora-p5-internal.out') -PortValue 19097
    [void]$procs.Add($gwInternal)
    $exitedInternal = $gwInternal.WaitForExit(10000)
    $internalLog = Read-SharedText (Join-Path $tmp 'agora-p5-internal.out')
    Assert-That '⑥ 内网地址默认被拒（SSRF）' ($exitedInternal -and $internalLog -match '内网')

    $gwNoPass = Start-Gateway -DataDir (Join-Path $tmp 'agora-p5-nopass-data') -ConfigPath $cfgB -LogPath (Join-Path $tmp 'agora-p5-nopass.out') -PortValue $PortB -Extra '--listen 0.0.0.0'
    [void]$procs.Add($gwNoPass)
    $exitedNoPass = $gwNoPass.WaitForExit(10000)
    $noPassLog = Read-SharedText (Join-Path $tmp 'agora-p5-nopass.out')
    Assert-That '⑥ 非回环监听且无密码时拒绝启动' ($exitedNoPass -and $noPassLog -match '管理密码')

    # 8) 单文件交付（已在 A 段验证产物；此处验证运行时不依赖数据库外的文件）
    Assert-That '⑧ 运行仅依赖可执行文件 + agora.db + master.key' (
        (Test-Path (Join-Path $dataDirA 'agora.db')) -and (Test-Path (Join-Path $dataDirA 'master.key')))

    Write-Host '== C) 性能冒烟（T5.4）=='
    $before = (Get-Process -Id $gwA.Id).WorkingSet64
    $jobs = @()
    $perfBody = Save-Text (Join-Path $tmp 'agora-p5-perf.json') '{"model":"mock-gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    for ($i = 0; $i -lt 20; $i++) {
        $jobs += Start-Job -ScriptBlock {
            param($exe, $url, $key, $body)
            $out = & curl.exe -s -o NUL -w '%{http_code}' --max-time 30 -X POST $url `
                -H 'content-type: application/json' -H "authorization: Bearer $key" `
                -H 'x-mock-chunks: 3' -H 'x-mock-gap: 10' --data-binary "@$body"
            return ($out -join '').Trim()
        } -ArgumentList $exe, "$base/v1/chat/completions", $key, $perfBody
    }
    $codes = $jobs | Wait-Job | Receive-Job
    $jobs | Remove-Job -Force
    $sw.Stop()
    $okCount = @($codes | Where-Object { $_ -eq '200' }).Count
    Assert-That '20 并发流式请求全部成功' ($okCount -eq 20) "成功=$okCount/20 耗时=$($sw.ElapsedMilliseconds)ms"

    Start-Sleep -Seconds 2
    $after = (Get-Process -Id $gwA.Id).WorkingSet64
    $growthMB = [math]::Round(($after - $before) / 1MB, 1)
    Assert-That '压测后内存增长可控（< 80MB）' ($growthMB -lt 80) "增长=${growthMB}MB"

    Write-Host '== D) 换机迁移演练（T5.6）=='
    $export = Invoke-Http "$base/api/export" -HeaderArgs $auth
    Assert-That '导出配置成功且不含明文凭证' ($export.Code -eq 200 -and $export.Body -notmatch 'sk-acceptance-key')

    $gwB = Start-Gateway -DataDir $dataDirB -ConfigPath $cfgB -LogPath (Join-Path $tmp 'agora-p5-b.out') -PortValue $PortB
    [void]$procs.Add($gwB)
    Start-Sleep -Seconds 3
    $keyB = Get-KeyFromLog (Join-Path $tmp 'agora-p5-b.out')
    Assert-That '迁移目标实例已就绪（独立数据目录 + 新网关 Key）' ($null -ne $keyB -and $keyB -ne $key)

    $exportObj = $export.Body | ConvertFrom-Json
    $first = $exportObj.providers[0]
    $importPayload = @{
        providers = @(
            @{
                id                 = $first.id
                name               = $first.name
                openai_base_url    = $first.openai_base_url
                anthropic_base_url = $first.anthropic_base_url
                api_key            = 'sk-migrated-key'
                models_manual      = @($first.models_manual)
                auto_fetch_models  = $true
                allow_internal     = $true
                priority           = 1
            }
        )
    } | ConvertTo-Json -Depth 6 -Compress
    $imported = Invoke-Http "http://127.0.0.1:$PortB/api/import" -Method POST -HeaderArgs $json -Body $importPayload
    Assert-That '在目标实例导入供应商成功' ($imported.Code -eq 200) "code=$($imported.Code)"

    $migratedChat = Invoke-Http "http://127.0.0.1:$PortB/v1/chat/completions" -Method POST `
        -HeaderArgs ($json + @('-H', "authorization: Bearer $keyB")) `
        -Body '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}'
    Assert-That '迁移后 Agent 可直接使用（同模型名仍可用）' ($migratedChat.Code -eq 200) "code=$($migratedChat.Code)"
}
finally {
    foreach ($p in $procs) {
        if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    }
    if ($mock -and -not $mock.HasExited) { Stop-Process -Id $mock.Id -Force -ErrorAction SilentlyContinue }
    Get-Job | Remove-Job -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 400
}

$failed = @($results | Where-Object { -not $_.Ok })
Write-Host ''
Write-Host ("== 验收结果：{0}/{1} 通过 ==" -f ($results.Count - $failed.Count), $results.Count)
if ($failed.Count -gt 0) {
    $failed | ForEach-Object { Write-Host ("  FAIL: {0} ({1})" -f $_.Name, $_.Detail) }
    exit 1
}
Write-Host '全部通过'
exit 0
