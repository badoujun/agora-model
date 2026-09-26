# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  Phase 3 端到端冒烟：模型聚合、/v1/models、provider/model 命名空间路由。

.DESCRIPTION
  覆盖：启动即聚合（自动拉取上游 /models）、手动模型与排除列表、失败隔离（不可达供应商不影响他人）、
        /v1/models 的裸名与命名空间条目、用聚合得到的模型直接透传、命名空间路由改写 model、
        未授权访问 /v1/models。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File tools/smoke/phase3.ps1
#>
[CmdletBinding()]
param(
    [int]$Port = 19092,
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

function Invoke-Json {
    param([string]$Url, [string[]]$HeaderArgs, [string]$BodyFile, [string]$Method = 'GET')
    $curlArgs = @('-s', '-w', "`n%{http_code}", '--max-time', '20', '-X', $Method, $Url)
    if ($HeaderArgs) { $curlArgs += $HeaderArgs }
    if ($BodyFile) { $curlArgs += @('--data-binary', "@$BodyFile") }
    $raw = (& curl.exe @curlArgs) -join "`n"
    $idx = $raw.LastIndexOf("`n")
    if ($idx -lt 0) { return [pscustomobject]@{ Code = 0; Raw = $raw; Json = $null } }
    $code = 0
    [void][int]::TryParse($raw.Substring($idx + 1).Trim(), [ref]$code)
    $body = $raw.Substring(0, $idx)
    $json = $null
    try { $json = $body | ConvertFrom-Json } catch { }
    return [pscustomobject]@{ Code = $code; Raw = $body; Json = $json }
}

Write-Host '== 构建开发二进制 =='
$exeName = if ($env:OS -eq 'Windows_NT') { 'agoramodel-p3.exe' } else { 'agoramodel-p3' }
$exe = Join-Path $tmp $exeName
& go build -trimpath -o $exe ./cmd/agoramodel
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }

$dataDir = Join-Path $tmp 'agora-p3-data'
if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }

# mock：models 为空 + 自动拉取；排除一个上游模型；另有手动模型
# dead：指向未监听端口，用于验证失败隔离
$cfg = Save-Text (Join-Path $tmp 'agora-p3-config.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $Port, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "mock", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "anthropic_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-mock-provider-key", "models": ["my-manual-model"],
      "models_excluded": ["mock-deepseek-v3"], "auto_fetch_models": true,
      "priority": 10, "allow_internal": true },
    { "id": "dead", "openai_base_url": "http://127.0.0.1:9998/v1",
      "api_key": "sk-dead", "models": [], "auto_fetch_models": true,
      "priority": 20, "allow_internal": true }
  ]
}
"@

$autoModel = Save-Text (Join-Path $tmp 'agora-p3-auto.json') '{"model":"mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}'
$nsModel = Save-Text (Join-Path $tmp 'agora-p3-ns.json') '{"model":"mock/mock-gpt-4o","messages":[{"role":"user","content":"hi"}]}'
$manualModel = Save-Text (Join-Path $tmp 'agora-p3-manual.json') '{"model":"my-manual-model","messages":[{"role":"user","content":"hi"}]}'
$unknownNS = Save-Text (Join-Path $tmp 'agora-p3-unknown-ns.json') '{"model":"nope/nope-model","messages":[{"role":"user","content":"hi"}]}'

$env:PORT = "$MockPort"
$mockOut = Join-Path $tmp 'agora-p3-mock.out'
$mock = Start-Process node -ArgumentList 'tools/mock-upstream/server.mjs' -PassThru `
    -RedirectStandardOutput $mockOut -RedirectStandardError "$mockOut.err" -WindowStyle Hidden
Start-Sleep -Seconds 2

$gwOut = Join-Path $tmp 'agora-p3-gw.out'
$gw = Start-Process $exe -ArgumentList '--config', $cfg, '--data-dir', $dataDir, '--port', "$Port", '--log-level', 'debug' -PassThru `
    -RedirectStandardOutput $gwOut -RedirectStandardError "$gwOut.err" -WindowStyle Hidden
$base = "http://127.0.0.1:$Port"

try {
    Start-Sleep -Seconds 3  # 等待启动 + 首轮聚合完成

    $log = Read-SharedText $gwOut
    if ($log -match 'gateway_key=(gw-[0-9a-f]+)') { $key = $Matches[1] } else { throw "未能取得网关 Key：$log" }
    $auth = @('-H', 'content-type: application/json', '-H', "authorization: Bearer $key")

    Write-Host '== 断言 =='

    Assert-That '启动即完成自动聚合（记录拉到 3 个模型）' ($log -match '模型聚合完成' -and $log -match 'provider=mock' -and $log -match 'models=3')
    Assert-That '失败隔离：不可达供应商记录拉取失败' ($log -match '模型聚合：拉取失败' -and $log -match 'provider=dead')

    $models = Invoke-Json "$base/v1/models" $auth
    Assert-That 'GET /v1/models 返回 200' ($models.Code -eq 200) "code=$($models.Code)"
    $ids = @($models.Json.data | ForEach-Object { $_.id })
    Assert-That '聚合结果包含自动拉取的模型' (($ids -contains 'mock-gpt-4o') -and ($ids -contains 'mock-claude-sonnet-4-5'))
    Assert-That '聚合结果包含手动模型' ($ids -contains 'my-manual-model')
    Assert-That '排除列表生效（mock-deepseek-v3 不出现）' (-not ($ids -contains 'mock-deepseek-v3'))
    Assert-That '列出命名空间形式 provider/model' (($ids -contains 'mock/mock-gpt-4o') -and ($ids -contains 'mock/my-manual-model'))
    Assert-That '不包含失败供应商的模型' (-not ($ids -contains 'dead/mock-gpt-4o'))

    $shared = $models.Json.data | Where-Object { $_.id -eq 'mock-gpt-4o' }
    Assert-That '裸名条目 owned_by 为供应商 id' ($shared.owned_by -eq 'mock')

    $unauth = Invoke-Json "$base/v1/models" $null
    Assert-That '未授权访问 /v1/models → 401' ($unauth.Code -eq 401) "code=$($unauth.Code)"

    $auto = Invoke-Json "$base/v1/chat/completions" $auth $autoModel 'POST'
    Assert-That '用聚合得到的模型直接透传（无需手动配置）' ($auto.Code -eq 200 -and $auto.Raw -match 'mock') "code=$($auto.Code)"

    $manual = Invoke-Json "$base/v1/chat/completions" $auth $manualModel 'POST'
    Assert-That '手动模型同样可路由' ($manual.Code -eq 200) "code=$($manual.Code)"

    $ns = Invoke-Json "$base/v1/chat/completions" $auth $nsModel 'POST'
    Assert-That '命名空间路由可用（mock/mock-gpt-4o）' ($ns.Code -eq 200 -and $ns.Raw -match 'mock') "code=$($ns.Code)"

    $mockRequests = Invoke-Json "http://127.0.0.1:$MockPort/__requests"
    $lastNS = @($mockRequests.Json.requests | Where-Object { $_.model -eq 'mock-gpt-4o' }) | Select-Object -Last 1
    Assert-That '命名空间前缀已从转发给上游的 model 中剥离' ($null -ne $lastNS) "上游收到 model=$($lastNS.model)"

    $unknown = Invoke-Json "$base/v1/chat/completions" $auth $unknownNS 'POST'
    Assert-That '未知命名空间前缀退回普通模型名 → 404' ($unknown.Code -eq 404) "code=$($unknown.Code)"
}
finally {
    foreach ($p in @($gw, $mock)) {
        if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
    }
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
