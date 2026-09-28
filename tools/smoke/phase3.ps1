# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  Phase 3 端到端冒烟：模型拉取与勾选、/v1/models 聚合、供应商名命名空间路由、模型别名。

.DESCRIPTION
  覆盖：启动时未勾选模型不对外暴露、按需拉取上游 /models 作为候选、勾选后立即生效、
        勾选之外的上游模型不出现、模型别名即对外模型名（转发时换回上游名）、
        供应商名称命名空间路由、拉取失败不影响其他供应商、未授权访问 /v1/models。

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

# mock：启动时未勾选任何模型，随后通过 API 拉取候选并勾选
# dead：指向未监听端口，用于验证拉取失败隔离
$cfg = Save-Text (Join-Path $tmp 'agora-p3-config.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $Port, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "mock", "name": "mock 供应商", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-mock-provider-key", "models": [], "allow_internal": true },
    { "id": "dead", "name": "dead 供应商", "openai_base_url": "http://127.0.0.1:9998/v1",
      "api_key": "sk-dead", "models": [], "allow_internal": true }
  ]
}
"@

$selectBody = Save-Text (Join-Path $tmp 'agora-p3-select.json') (@'
{"name":"mock 供应商","models_selected":["mock-gpt-4o","mock-claude-sonnet-4-5"],
 "model_aliases":{"mock-gpt-4o":"我的模型"},"allow_internal":true,"enabled":true}
'@)

$aliasModel = Save-Text (Join-Path $tmp 'agora-p3-alias.json') '{"model":"我的模型","messages":[{"role":"user","content":"hi"}]}'
$nsModel = Save-Text (Join-Path $tmp 'agora-p3-ns.json') '{"model":"mock 供应商/mock-claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}'
$unknownNS = Save-Text (Join-Path $tmp 'agora-p3-unknown-ns.json') '{"model":"nope/nope-model","messages":[{"role":"user","content":"hi"}]}'

$env:PORT = "$MockPort"
$mockOut = Join-Path $tmp 'agora-p3-mock.out'
$mock = Start-Process node -ArgumentList '--no-deprecation', 'tools/mock-upstream/server.mjs' -PassThru `
    -RedirectStandardOutput $mockOut -RedirectStandardError "$mockOut.err" -WindowStyle Hidden
Start-Sleep -Seconds 2

$gwOut = Join-Path $tmp 'agora-p3-gw.out'
$gw = Start-Process $exe -ArgumentList '--config', $cfg, '--data-dir', $dataDir, '--port', "$Port", '--log-level', 'debug' -PassThru `
    -RedirectStandardOutput $gwOut -RedirectStandardError "$gwOut.err" -WindowStyle Hidden
$base = "http://127.0.0.1:$Port"
$json = @('-H', 'content-type: application/json')

try {
    Start-Sleep -Seconds 3

    $log = Read-SharedText $gwOut
    if ($log -match 'gateway_key=(gw-[0-9a-f]+)') { $key = $Matches[1] } else { throw "未能取得网关 Key：$log" }
    $auth = $json + @('-H', "authorization: Bearer $key")

    Write-Host '== 断言 =='

    $models = Invoke-Json "$base/v1/models" $auth
    Assert-That 'GET /v1/models 返回 200' ($models.Code -eq 200) "code=$($models.Code)"
    Assert-That '未勾选模型时 /v1/models 为空' (@($models.Json.data).Count -eq 0) "count=$(@($models.Json.data).Count)"

    $unauth = Invoke-Json "$base/v1/models" $null
    Assert-That '未授权访问 /v1/models → 401' ($unauth.Code -eq 401) "code=$($unauth.Code)"

    $fetch = Invoke-Json "$base/api/providers/mock/fetch-models" $auth $null 'POST'
    Assert-That '拉取模型接口返回候选（3 个）' ($fetch.Code -eq 200 -and @($fetch.Json.candidate_models).Count -eq 3) "code=$($fetch.Code)"
    Assert-That '拉取候选不改变已勾选模型' (@($fetch.Json.models_selected).Count -eq 0)

    $fetchDead = Invoke-Json "$base/api/providers/dead/fetch-models" $auth $null 'POST'
    Assert-That '不可达供应商拉取失败 → 502' ($fetchDead.Code -eq 502) "code=$($fetchDead.Code)"
    $deadProviders = Invoke-Json "$base/api/providers" $auth
    $deadRec = @($deadProviders.Json.items | Where-Object { $_.id -eq 'dead' })[0]
    Assert-That '失败原因写入 last_fetch_error' ($deadRec.last_fetch_error -ne $null -and $deadRec.last_fetch_error -ne '') "err=$($deadRec.last_fetch_error)"
    $mockRec = @($deadProviders.Json.items | Where-Object { $_.id -eq 'mock' })[0]
    Assert-That '失败供应商不影响其他供应商' ([string]::IsNullOrEmpty($mockRec.last_fetch_error)) "err=$($mockRec.last_fetch_error)"

    $select = Invoke-Json "$base/api/providers/mock" $auth $selectBody 'PUT'
    Assert-That '勾选模型并设置别名 → 200' ($select.Code -eq 200) "code=$($select.Code)"

    $models = Invoke-Json "$base/v1/models" $auth
    $ids = @($models.Json.data | ForEach-Object { $_.id })
    Assert-That '勾选的模型出现在 /v1/models' (($ids -contains '我的模型') -and ($ids -contains 'mock-claude-sonnet-4-5'))
    Assert-That '未勾选的上游模型不出现' (-not ($ids -contains 'mock-deepseek-v3'))
    Assert-That '配置别名后不再暴露上游原名' (-not ($ids -contains 'mock-gpt-4o'))
    Assert-That '列出命名空间形式（供应商名/模型名）' (($ids -contains 'mock 供应商/我的模型') -and ($ids -contains 'mock 供应商/mock-claude-sonnet-4-5'))
    Assert-That '不包含失败供应商的模型' (-not ($ids -contains 'dead 供应商/mock-gpt-4o'))

    $aliasEntry = @($models.Json.data | Where-Object { $_.id -eq '我的模型' })[0]
    Assert-That '裸名条目 owned_by 为供应商名称' ($aliasEntry.owned_by -eq 'mock 供应商') "owned_by=$($aliasEntry.owned_by)"

    $alias = Invoke-Json "$base/v1/chat/completions" $auth $aliasModel 'POST'
    Assert-That '用别名请求可直接透传（Agent 立即可用）' ($alias.Code -eq 200 -and $alias.Raw -match 'mock') "code=$($alias.Code)"

    $mockRequests = Invoke-Json "http://127.0.0.1:$MockPort/__requests"
    $lastAlias = @($mockRequests.Json.requests | Where-Object { $_.model -eq 'mock-gpt-4o' }) | Select-Object -Last 1
    Assert-That '别名已换回上游真实模型名' ($null -ne $lastAlias) "上游收到 model=$($lastAlias.model)"

    $ns = Invoke-Json "$base/v1/chat/completions" $auth $nsModel 'POST'
    Assert-That '命名空间路由可用（供应商名/模型名）' ($ns.Code -eq 200 -and $ns.Raw -match 'mock') "code=$($ns.Code)"

    $mockRequests = Invoke-Json "http://127.0.0.1:$MockPort/__requests"
    $lastNS = @($mockRequests.Json.requests | Where-Object { $_.model -eq 'mock-claude-sonnet-4-5' }) | Select-Object -Last 1
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
