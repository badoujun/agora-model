# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  Phase 1 端到端冒烟：启动 mock 上游与网关，逐项断言核心行为。

.DESCRIPTION
  覆盖：健康检查、认证与错误体风格（分协议）、路由与错误码、非流式与流式透传、
        上游凭证替换与头透传、SSE 心跳保活、上游超时、上游不可达、请求体上限、
        extra_body / extra_headers 合并、客户端断连取消。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File tools/smoke/phase1.ps1
#>
[CmdletBinding()]
param(
    [int]$GatewayPort = 19090,
    [int]$MockPort = 9999
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $repoRoot

$env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' +
            [Environment]::GetEnvironmentVariable('Path', 'User') + ';' + $env:Path
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

function Invoke-Curl {
    param(
        [string]$Url,
        [string[]]$HeaderArgs,
        [string]$BodyFile,
        [int]$MaxTime = 30,
        [string]$Method = 'POST',
        [switch]$Stream
    )
    $curlArgs = @('-s', '-w', "`n%{http_code}")
    if ($Stream) { $curlArgs += '-N' }
    $curlArgs += @('--max-time', "$MaxTime", '-X', $Method, $Url)
    if ($HeaderArgs) { $curlArgs += $HeaderArgs }
    if ($BodyFile) { $curlArgs += @('--data-binary', "@$BodyFile") }

    $raw = (& curl.exe @curlArgs) -join "`n"
    $idx = $raw.LastIndexOf("`n")
    if ($idx -lt 0) { return [pscustomobject]@{ Code = 0; Body = $raw } }
    $code = 0
    [void][int]::TryParse($raw.Substring($idx + 1).Trim(), [ref]$code)
    return [pscustomobject]@{ Code = $code; Body = $raw.Substring(0, $idx) }
}

$jsonHeader = @('-H', 'content-type: application/json')
$exeName = if ($env:OS -eq 'Windows_NT') { 'agoramodel-smoke.exe' } else { 'agoramodel-smoke' }
$exe = Join-Path $tmp $exeName

Write-Host '== 构建开发二进制 =='
& go build -trimpath -o $exe ./cmd/agoramodel
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }

$cfgFile = Join-Path $tmp 'agora-smoke-config.json'
$cfg = @"
{
  "gateway": {
    "api_key": "$key",
    "listen": "127.0.0.1",
    "port": $GatewayPort,
    "sse_idle_seconds": 1,
    "max_body_bytes": 1024
  },
  "providers": [
    { "id": "mock", "openai_base_url": "http://127.0.0.1:$MockPort/v1", "anthropic_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-mock-provider-key", "models": ["mock-gpt-4o", "mock-claude-sonnet-4-5"], "allow_internal": true, "priority": 10, "timeout_seconds": 120 },
    { "id": "mock-slow", "openai_base_url": "http://127.0.0.1:$MockPort/v1", "api_key": "sk-slow",
      "models": ["slow-model"], "allow_internal": true, "priority": 10, "timeout_seconds": 1 },
    { "id": "dead", "openai_base_url": "http://127.0.0.1:9998/v1", "api_key": "sk-dead",
      "models": ["dead-model"], "allow_internal": true, "priority": 10, "timeout_seconds": 5 },
    { "id": "mock-extra", "openai_base_url": "http://127.0.0.1:$MockPort/v1", "api_key": "sk-extra",
      "models": ["extra-model"], "allow_internal": true, "priority": 10, "timeout_seconds": 60,
      "extra_headers": { "x-tenant": "agora" }, "extra_body": { "temperature": 0.1, "top_p": 0.9 } }
  ]
}
"@
[void](Save-Text $cfgFile $cfg)

$bodyOpenAI = Save-Text (Join-Path $tmp 'agora-req-openai.json') '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}'
$bodyOpenAIStream = Save-Text (Join-Path $tmp 'agora-req-openai-stream.json') '{"model":"mock-gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
$bodyAnthropicStream = Save-Text (Join-Path $tmp 'agora-req-anthropic-stream.json') '{"model":"mock-claude-sonnet-4-5","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
$bodyNoModel = Save-Text (Join-Path $tmp 'agora-req-nomodel.json') '{"messages":[]}'
$bodyUnknown = Save-Text (Join-Path $tmp 'agora-req-unknown.json') '{"model":"nope-1","messages":[]}'
$bodySlow = Save-Text (Join-Path $tmp 'agora-req-slow.json') '{"model":"slow-model","messages":[]}'
$bodyDead = Save-Text (Join-Path $tmp 'agora-req-dead.json') '{"model":"dead-model","messages":[]}'
$bodyExtra = Save-Text (Join-Path $tmp 'agora-req-extra.json') '{"model":"extra-model","messages":[]}'
$bodyBig = Save-Text (Join-Path $tmp 'agora-req-big.json') ('{"model":"mock-gpt-4o","pad":"' + ('x' * 2048) + '"}')

$mockOut = Join-Path $tmp 'agora-mock.out'
$gwOut = Join-Path $tmp 'agora-gw.out'
$dataDir = Join-Path $tmp 'agora-smoke-data'
if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
$env:PORT = "$MockPort"
$mock = Start-Process node -ArgumentList 'tools/mock-upstream/server.mjs' -PassThru `
    -RedirectStandardOutput $mockOut -RedirectStandardError "$mockOut.err" -WindowStyle Hidden
$gw = Start-Process $exe -ArgumentList '--config', $cfgFile, '--data-dir', $dataDir, '--log-level', 'debug' -PassThru `
    -RedirectStandardOutput $gwOut -RedirectStandardError "$gwOut.err" -WindowStyle Hidden
Start-Sleep -Seconds 2

# Phase 2 起网关 Key 由数据库管理：从启动日志取回本次生成的明文
function Read-SharedText([string]$path) {
    if (-not (Test-Path $path)) { return '' }
    $fs = [System.IO.File]::Open($path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
    try {
        $sr = New-Object System.IO.StreamReader($fs)
        try { return $sr.ReadToEnd() } finally { $sr.Dispose() }
    }
    finally { $fs.Dispose() }
}
$bootLog = ''
for ($i = 0; $i -lt 20; $i++) {
    $bootLog = Read-SharedText $gwOut
    if ($bootLog -match 'gateway_key=(gw-[0-9a-f]+)') { break }
    Start-Sleep -Milliseconds 300
}
if ($bootLog -notmatch 'gateway_key=(gw-[0-9a-f]+)') { throw "未能从启动日志取得网关 Key：$bootLog" }
$key = $Matches[1]
Write-Host "  网关 Key 已取得：$($key.Substring(0, 7))***"
$openAIHeaders = $jsonHeader + @('-H', "authorization: Bearer $key")
$anthropicHeaders = $jsonHeader + @('-H', "x-api-key: $key")

$base = "http://127.0.0.1:$GatewayPort"
try {
    Write-Host '== 断言 =='

    $r = Invoke-Curl "$base/healthz" -MaxTime 5 -Method GET
    Assert-That 'healthz 返回 200' ($r.Code -eq 200) "code=$($r.Code)"

    $r = Invoke-Curl "$base/v1/chat/completions" $jsonHeader $bodyOpenAI
    Assert-That '无凭证（OpenAI 入站）→ 401' ($r.Code -eq 401) "code=$($r.Code)"
    Assert-That 'OpenAI 风格错误体含 error.code=invalid_api_key' ($r.Body -match '"code":"invalid_api_key"')

    $r = Invoke-Curl "$base/v1/messages" $jsonHeader $bodyOpenAI
    Assert-That '无凭证（Anthropic 入站）→ 401' ($r.Code -eq 401) "code=$($r.Code)"
    Assert-That 'Anthropic 风格错误体含 type=error' ($r.Body -match '"type":"error"')

    $r = Invoke-Curl "$base/v1/chat/completions" $openAIHeaders $bodyNoModel
    Assert-That '缺少 model → 400' ($r.Code -eq 400) "code=$($r.Code)"

    $r = Invoke-Curl "$base/v1/chat/completions" $openAIHeaders $bodyUnknown
    Assert-That '未知模型 → 404 model_not_found' ($r.Code -eq 404 -and $r.Body -match 'model_not_found') "code=$($r.Code)"

    $r = Invoke-Curl "$base/v1/chat/completions" $openAIHeaders $bodyOpenAI
    Assert-That '非流式透传 → 200 且内容来自上游' ($r.Code -eq 200 -and $r.Body -match 'mock 非流式回复') "code=$($r.Code)"

    $rr = Invoke-RestMethod -Uri "http://127.0.0.1:$MockPort/__requests" -UseBasicParsing
    $last = $rr.requests | Select-Object -Last 1
    Assert-That '上游收到替换后的供应商凭证' ($last.received.authorization -eq 'Bearer sk-mock-provider-key') "auth=$($last.received.authorization)"

    $r = Invoke-Curl "$base/v1/chat/completions" ($openAIHeaders + @('-H', 'x-mock-chunks: 3', '-H', 'x-mock-gap: 40')) $bodyOpenAIStream -Stream
    Assert-That 'OpenAI 流式透传 → 含 [DONE]' ($r.Body -match '\[DONE\]')

    $r = Invoke-Curl "$base/v1/messages" ($anthropicHeaders + @('-H', 'anthropic-version: 2023-06-01', '-H', 'x-mock-chunks: 4', '-H', 'x-mock-gap: 30')) $bodyAnthropicStream -Stream
    Assert-That 'Anthropic 流式透传 → 含 message_stop' ($r.Body -match 'message_stop')

    $rr = Invoke-RestMethod -Uri "http://127.0.0.1:$MockPort/__requests" -UseBasicParsing
    $last = $rr.requests | Select-Object -Last 1
    Assert-That 'anthropic-version 被透传到上游' ($last.received.'anthropic-version' -eq '2023-06-01')
    Assert-That 'x-api-key 被替换为供应商凭证' ($last.received.'x-api-key' -eq 'sk-mock-provider-key')

    $r = Invoke-Curl "$base/v1/chat/completions" ($openAIHeaders + @('-H', 'x-mock-silence: 3', '-H', 'x-mock-chunks: 2')) $bodyOpenAIStream -Stream -MaxTime 12
    Assert-That 'SSE 空闲注入心跳（keep-alive）' ($r.Body -match ': keep-alive')

    $r = Invoke-Curl "$base/v1/chat/completions" ($openAIHeaders + @('-H', 'x-mock-slow: 5')) $bodySlow
    Assert-That '上游超时 → 504 upstream_timeout' ($r.Code -eq 504 -and $r.Body -match 'upstream_timeout') "code=$($r.Code)"

    $r = Invoke-Curl "$base/v1/chat/completions" $openAIHeaders $bodyDead
    Assert-That '上游不可达 → 502 upstream_unreachable' ($r.Code -eq 502 -and $r.Body -match 'upstream_unreachable') "code=$($r.Code)"

    $r = Invoke-Curl "$base/v1/chat/completions" $openAIHeaders $bodyBig
    Assert-That '请求体超限 → 413 payload_too_large' ($r.Code -eq 413 -and $r.Body -match 'payload_too_large') "code=$($r.Code)"

    $r = Invoke-Curl "$base/v1/chat/completions" $openAIHeaders $bodyExtra
    $rr = Invoke-RestMethod -Uri "http://127.0.0.1:$MockPort/__requests" -UseBasicParsing
    $last = $rr.requests | Select-Object -Last 1
    Assert-That 'extra_headers 注入到上游' ($last.xHeaders.'x-tenant' -eq 'agora')
    Assert-That 'extra_body 合并进请求体' ($last.bodyPreview -match '"temperature":0.1' -and $last.bodyPreview -match '"top_p":0.9')

    [void](Invoke-Curl "$base/v1/chat/completions" ($openAIHeaders + @('-H', 'x-mock-silence: 3', '-H', 'x-mock-chunks: 2')) $bodyOpenAIStream -Stream -MaxTime 1)
    Start-Sleep -Seconds 3

    $alive = Invoke-Curl "http://127.0.0.1:$MockPort/v1/models" -MaxTime 5 -Method GET
    Assert-That 'mock 上游在断连后仍存活' ($alive.Code -eq 200) "code=$($alive.Code)"
}
finally {
    foreach ($p in @($gw, $mock)) {
        if ($p) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    }
    Start-Sleep -Milliseconds 400
}

# 进程已停止、stdout 文件解锁后再断言断连取消行为
$log = [System.IO.File]::ReadAllText($gwOut, $enc)
Assert-That '客户端断开 → 上游请求被取消（日志含 context canceled）' ($log -match 'context canceled')

$failed = @($results | Where-Object { -not $_.Ok })
Write-Host ''
Write-Host ("== 冒烟结果：{0}/{1} 通过 ==" -f ($results.Count - $failed.Count), $results.Count)
if ($failed.Count -gt 0) {
    $failed | ForEach-Object { Write-Host ("  FAIL: {0} ({1})" -f $_.Name, $_.Detail) }
    exit 1
}
Write-Host '全部通过'
exit 0
