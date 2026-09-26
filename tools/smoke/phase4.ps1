# 注意：本文件必须以 UTF-8 with BOM + CRLF 保存。
# Windows PowerShell 5.1 在无 BOM 时会按 ANSI 解码，导致中文注释/输出乱码并报解析错误。
<#
.SYNOPSIS
  Phase 4 端到端冒烟：Web UI 静态资源 + 控制面 API 全链路 + 登录模式。

.DESCRIPTION
  覆盖：内嵌前端可访问（index.html / assets 缓存头 / SPA fallback）、/api 与 /v1 未知路径不被前端路由吞掉、
        健康检查与会话状态、模拟 Web UI 的操作序列（新增供应商 → 自动拉取模型 → 连接测试 →
        用新模型直接透传 → 日志可查 → 编辑保持凭证 → 删除）、以及设置 ADMIN_PASSWORD 后的登录流程。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File tools/smoke/phase4.ps1
#>
[CmdletBinding()]
param(
    [int]$Port = 19093,
    [int]$AuthPort = 19094,
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
    $headerFile = Join-Path $tmp ("agora-p4-h-$token.txt")
    $bodyFile = Join-Path $tmp ("agora-p4-b-$token.bin")
    $reqBody = $null
    if ($Body -ne '') {
        $reqBody = Join-Path $tmp ("agora-p4-rb-$token.json")
        [System.IO.File]::WriteAllText($reqBody, $Body, $enc)
    }

    $curlArgs = @('-s', '-D', $headerFile, '-o', $bodyFile, '-w', '%{http_code}', '--max-time', '30', '-X', $Method, $Url)
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
    param([string]$LogPath, [int]$TimeoutMs = 8000)
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    while ((Get-Date) -lt $deadline) {
        $text = Read-SharedText $LogPath
        if ($text -match 'gateway_key=(gw-[0-9a-f]+)') { return $Matches[1] }
        Start-Sleep -Milliseconds 200
    }
    return $null
}

function Start-Gateway {
    param([string]$DataDir, [string]$ConfigPath, [string]$LogPath, [int]$Port, [string]$AdminPassword = '')
    $gwArgs = @('--config', $ConfigPath, '--data-dir', $DataDir, '--port', "$Port", '--log-level', 'debug')
    if ($AdminPassword -ne '') { $gwArgs += @('--admin-password', $AdminPassword) }
    return Start-Process $exe -ArgumentList $gwArgs -PassThru `
        -RedirectStandardOutput $LogPath -RedirectStandardError "$LogPath.err" -WindowStyle Hidden
}

Write-Host '== 准备：构建前端与开发二进制 =='
if (-not (Test-Path (Join-Path $repoRoot 'internal\webui\dist\index.html'))) {
    Write-Host '  前端尚未构建，执行 npm run build …'
    Push-Location (Join-Path $repoRoot 'web')
    try {
        npm run build 2>&1 | Select-Object -Last 3
    }
    finally { Pop-Location }
}
Assert-That '前端构建产物存在（internal/webui/dist/index.html）' (Test-Path (Join-Path $repoRoot 'internal\webui\dist\index.html'))

$exeName = if ($env:OS -eq 'Windows_NT') { 'agoramodel-p4.exe' } else { 'agoramodel-p4' }
$exe = Join-Path $tmp $exeName
& go build -trimpath -o $exe ./cmd/agoramodel
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }

$configA = Save-Text (Join-Path $tmp 'agora-p4-a.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $Port, "sse_idle_seconds": 15, "max_body_bytes": 1048576 },
  "providers": [
    { "id": "seed", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "anthropic_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-seed-key", "models": ["mock-gpt-4o"], "priority": 50, "allow_internal": true }
  ]
}
"@
$configB = Save-Text (Join-Path $tmp 'agora-p4-b.json') @"
{
  "gateway": { "listen": "127.0.0.1", "port": $AuthPort },
  "providers": [
    { "id": "seed", "openai_base_url": "http://127.0.0.1:$MockPort/v1",
      "api_key": "sk-seed-key", "models": ["mock-gpt-4o"], "allow_internal": true }
  ]
}
"@

$dataDirA = Join-Path $tmp 'agora-p4-data'; $dataDirB = Join-Path $tmp 'agora-p4-auth'
foreach ($d in @($dataDirA, $dataDirB)) { if (Test-Path $d) { Remove-Item -Recurse -Force $d } }

$env:PORT = "$MockPort"
$mockOut = Join-Path $tmp 'agora-p4-mock.out'
$mock = Start-Process node -ArgumentList 'tools/mock-upstream/server.mjs' -PassThru `
    -RedirectStandardOutput $mockOut -RedirectStandardError "$mockOut.err" -WindowStyle Hidden
Start-Sleep -Seconds 2

$logA = Join-Path $tmp 'agora-p4-a.out'
$logB = Join-Path $tmp 'agora-p4-b.out'
$gwA = Start-Gateway -DataDir $dataDirA -ConfigPath $configA -LogPath $logA -Port $Port
$base = "http://127.0.0.1:$Port"
$procs = New-Object System.Collections.ArrayList
[void]$procs.Add($gwA)

try {
    Start-Sleep -Seconds 3
    $key = Get-KeyFromLog $logA
    Assert-That '网关 Key 已生成（供 Agent 使用）' ($null -ne $key)
    $authHeader = @('-H', 'content-type: application/json', '-H', "authorization: Bearer $key")

    Write-Host '== A) Web UI 静态资源与 SPA 行为 =='

    $index = Invoke-Http "$base/"
    Assert-That 'GET / 返回前端页面' ($index.Code -eq 200 -and $index.Body -match '<div id="root">') "code=$($index.Code)"
    Assert-That 'GET / 的 Content-Type 为 text/html' ($index.Headers -match 'Content-Type:\s*text/html')
    Assert-That 'GET / 不被缓存（no-store）' ($index.Headers -match 'Cache-Control:\s*no-store')

    $spa = Invoke-Http "$base/logs"
    Assert-That '未知前端路由回退到 index.html（SPA）' ($spa.Code -eq 200 -and $spa.Body -match '<div id="root">') "code=$($spa.Code)"

    $assetPath = (Get-ChildItem (Join-Path $repoRoot 'internal\webui\dist\assets') -Filter '*.js' | Select-Object -First 1).Name
    $asset = Invoke-Http "$base/assets/$assetPath"
    Assert-That 'GET /assets/*.js 返回 200' ($asset.Code -eq 200) "code=$($asset.Code)"
    Assert-That '静态资源带长期缓存头（immutable）' ($asset.Headers -match 'immutable')

    $apiUnknown = Invoke-Http "$base/api/nope"
    Assert-That '/api 未知路径返回 404（不被 SPA 吞掉）' ($apiUnknown.Code -eq 404) "code=$($apiUnknown.Code)"
    $v1Unknown = Invoke-Http "$base/v1/nope"
    Assert-That '/v1 未知路径返回 404（不被 SPA 吞掉）' ($v1Unknown.Code -eq 404) "code=$($v1Unknown.Code)"

    Write-Host '== B) 控制面会话与健康检查 =='

    $session = Invoke-Http "$base/api/auth/session"
    Assert-That '会话：回环 + 未设密码 → 免登录' ($session.Body -match '"login_required":false' -and $session.Body -match '"authenticated":true')

    $health = Invoke-Http "$base/api/health"
    Assert-That 'GET /api/health 返回 200 且含版本' ($health.Code -eq 200 -and $health.Body -match '"status":"ok"')

    Write-Host '== C) 模拟 Web UI 操作序列 =='

    $createBody = @"
{"name":"界面新增供应商","openai_base_url":"http://127.0.0.1:$MockPort/v1",
 "anthropic_base_url":"http://127.0.0.1:$MockPort/v1",
 "api_key":"sk-ui-created-abcdefgh","models_manual":[],"auto_fetch_models":true,
 "priority":5,"timeout_seconds":60,"allow_internal":true,"enabled":true}
"@
    $created = Invoke-Http "$base/api/providers" -Method POST -Body $createBody -HeaderArgs @('-H', 'content-type: application/json')
    Assert-That 'POST /api/providers 创建成功（201）' ($created.Code -eq 201) "code=$($created.Code)"
    Assert-That '响应只回凭证掩码' ($created.Body -match '"api_key_hint":"sk-\*\*\*\*efgh"' -and $created.Body -notmatch 'sk-ui-created-abcdefgh')
    $newId = ([regex]::Match($created.Body, '"id":"([^"]+)"')).Groups[1].Value
    Assert-That '创建后自动拉取模型（model_count > 0）' (($created.Body -match '"model_count":(\d+)') -and ([int]$Matches[1] -gt 0))

    $test = Invoke-Http "$base/api/providers/$newId/test" -Method POST -HeaderArgs $authHeader
    Assert-That 'POST /api/providers/{id}/test 双协议探测通过' ($test.Code -eq 200 -and $test.Body -match '"ok":true')

    $models = Invoke-Http "$base/api/models" -HeaderArgs $authHeader
    Assert-That 'GET /api/models 含自动拉取到的模型' ($models.Body -match '"model":"mock-deepseek-v3"')
    Assert-That 'GET /api/models 标注来源（auto/manual）' ($models.Body -match '"source":"manual"' -and $models.Body -match '"source":"auto"')

    $chatBody = '{"model":"mock-deepseek-v3","messages":[{"role":"user","content":"hi"}]}'
    $chat = Invoke-Http "$base/v1/chat/completions" -Method POST -Body $chatBody -HeaderArgs $authHeader
    Assert-That '用界面新增的供应商直接完成透传（Agent 立即可用）' ($chat.Code -eq 200 -and $chat.Body -match 'mock') "code=$($chat.Code)"

    Start-Sleep -Milliseconds 800
    $logs = Invoke-Http "$base/api/logs?limit=20" -HeaderArgs $authHeader
    Assert-That 'GET /api/logs 可查到刚才的请求' ($logs.Code -eq 200 -and $logs.Body -match 'mock-deepseek-v3')

    $updateBody = '{"name":"改名后的供应商"}'
    $updated = Invoke-Http "$base/api/providers/$newId" -Method PUT -Body $updateBody -HeaderArgs @('-H', 'content-type: application/json')
    Assert-That 'PUT 更新名称成功且凭证掩码不变' ($updated.Code -eq 200 -and $updated.Body -match '改名后的供应商' -and $updated.Body -match 'sk-\*\*\*\*efgh') "code=$($updated.Code)"

    $settings = Invoke-Http "$base/api/settings" -HeaderArgs $authHeader
    Assert-That 'GET /api/settings 返回监听与 Key 掩码' ($settings.Code -eq 200 -and $settings.Body -match '"gateway_key_hint":"gw-')

    $deleted = Invoke-Http "$base/api/providers/$newId" -Method DELETE -HeaderArgs $authHeader
    Assert-That 'DELETE /api/providers/{id} 成功' ($deleted.Code -eq 200) "code=$($deleted.Code)"

    Write-Host '== D) 登录模式（ADMIN_PASSWORD）=='
    $gwB = Start-Gateway -DataDir $dataDirB -ConfigPath $configB -LogPath $logB -Port $AuthPort -AdminPassword 'p4-admin-pass'
    [void]$procs.Add($gwB)
    Start-Sleep -Seconds 3

    $authBase = "http://127.0.0.1:$AuthPort"
    $unauth = Invoke-Http "$authBase/api/providers"
    Assert-That '设置管理密码后未登录访问 /api → 401' ($unauth.Code -eq 401) "code=$($unauth.Code)"

    $indexAuth = Invoke-Http "$authBase/"
    Assert-That '登录页仍可加载前端资源（GET / 200）' ($indexAuth.Code -eq 200) "code=$($indexAuth.Code)"

    $loginBody = '{"password":"p4-admin-pass"}'
    $login = Invoke-Http "$authBase/api/auth/login" -Method POST -Body $loginBody -HeaderArgs @('-H', 'content-type: application/json')
    Assert-That 'POST /api/auth/login 成功并下发会话 Cookie' ($login.Code -eq 200 -and $login.Headers -match 'agoramodel_session=') "code=$($login.Code)"

    $cookieValue = ([regex]::Match($login.Headers, 'agoramodel_session=([^;\r\n]+)')).Groups[1].Value
    $authed = Invoke-Http "$authBase/api/providers" -HeaderArgs @('-H', "cookie: agoramodel_session=$cookieValue")
    Assert-That '携带会话 Cookie 后可访问 /api' ($authed.Code -eq 200) "code=$($authed.Code)"

    $wrong = Invoke-Http "$authBase/api/auth/login" -Method POST -Body '{"password":"nope"}' -HeaderArgs @('-H', 'content-type: application/json')
    Assert-That '错误密码被拒绝（401）' ($wrong.Code -eq 401) "code=$($wrong.Code)"
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
