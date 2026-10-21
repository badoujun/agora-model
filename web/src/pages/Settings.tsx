import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Check,
  CloudUpload,
  Copy,
  Download,
  Plug,
  RefreshCw,
  Save,
  Upload,
} from 'lucide-react'
import { api, ApiError } from '@/lib/api'
import { formatTime } from '@/lib/utils'
import { useToast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { GatewayKeysCard } from '@/components/GatewayKeysCard'

export function SettingsPage() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: api.getSettings })
  const webdavConfig = useQuery({ queryKey: ['webdav-config'], queryFn: api.getWebDAVConfig })
  const webdavStatus = useQuery({
    queryKey: ['webdav-status'],
    queryFn: api.webdavStatus,
    refetchInterval: 15_000,
  })

  const [copied, setCopied] = useState<string | null>(null)
  // 接入片段当前使用的网关 Key（明文按需 reveal，只留在本页内存）
  const [snippetKeyId, setSnippetKeyId] = useState('')
  const [snippetPlain, setSnippetPlain] = useState('')

  // WebDAV 表单本地状态：只暴露远程根目录与凭证；文件名由服务端内置（agoramodel-providers.json）
  const [webdavUrl, setWebdavUrl] = useState('')
  const [webdavUsername, setWebdavUsername] = useState('')
  const [webdavPassword, setWebdavPassword] = useState('')
  const [webdavFormInitialized, setWebdavFormInitialized] = useState(false)

  // 同步 WebDAV 配置 → 表单：仅在第一次或服务端版本变化时同步，避免覆盖未保存输入
  if (webdavConfig.data && !webdavFormInitialized) {
    setWebdavUrl(webdavConfig.data.url)
    setWebdavUsername(webdavConfig.data.username)
    setWebdavFormInitialized(true)
  }

  const saveSettings = useMutation({
    mutationFn: (payload: { log_success?: boolean }) => api.updateSettings(payload),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['settings'] })
      toast.show('设置已保存', 'success')
    },
    onError: (err: Error) => toast.show(`保存失败：${err.message}`, 'error'),
  })

  const saveWebDAV = useMutation({
    mutationFn: () =>
      api.updateWebDAVConfig({
        url: webdavUrl.trim(),
        username: webdavUsername.trim(),
        // 编辑时清空密码 = 沿用既有；首次保存必须填
        password: webdavPassword,
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['webdav-config'] })
      setWebdavPassword('')
      setWebdavFormInitialized(false)
      toast.show(webdavUrl.trim() ? '已保存 WebDAV 配置' : '已清空 WebDAV 配置', 'success')
    },
    onError: (err: Error) => {
      const msg = err instanceof ApiError ? err.message : String(err)
      toast.show(`保存失败：${msg}`, 'error')
    },
  })

  const refreshStatus = () => {
    void queryClient.invalidateQueries({ queryKey: ['webdav-status'] })
  }

  // 仅校验「配置能连上远端」：不发本地数据、不动远端文件，成功失败都给明确反馈。
  const testConnection = useMutation({
    mutationFn: api.webdavTest,
    onSuccess: async (data) => {
      if (data.ok) {
        const remote = data.has_remote
          ? `，远端指纹 ${(data.remote_fingerprint ?? '').slice(0, 12)}（${data.remote_bytes ?? 0} 字节）`
          : '，远端尚未上传文件（首次推送前的合法状态）'
        toast.show(`连接成功${remote}`, 'success')
      } else {
        toast.show(`连接失败：${data.error ?? '未知错误'}`, 'error')
      }
    },
    onError: (err: Error) => toast.show(`连接失败：${err.message}`, 'error'),
  })

  const pushSync = useMutation({
    mutationFn: (force: boolean) => api.webdavPush({ force }),
    onSuccess: async (data) => {
      await queryClient.invalidateQueries({ queryKey: ['webdav-status'] })
      await queryClient.invalidateQueries({ queryKey: ['providers'] })
      toast.show(
        `已推送 ${data.bytes ?? 0} 字节（指纹 ${data.fingerprint.slice(0, 8)}…）`,
        'success',
      )
    },
    onError: async (err: Error) => {
      if (err instanceof ApiError && err.status === 409) {
        if (window.confirm('远端文件与本地不同，确认覆盖远端？')) {
          pushSync.mutate(true)
        }
        return
      }
      toast.show(`推送失败：${err.message}`, 'error')
    },
  })

  const pullSync = useMutation({
    mutationFn: (force: boolean) => api.webdavPull({ force }),
    onSuccess: async (data) => {
      await queryClient.invalidateQueries({ queryKey: ['webdav-status'] })
      await queryClient.invalidateQueries({ queryKey: ['providers'] })
      await queryClient.invalidateQueries({ queryKey: ['models'] })
      toast.show(`已拉取并导入 ${data.imported ?? 0} 个供应商`, 'success')
    },
    onError: async (err: Error) => {
      if (err instanceof ApiError && err.status === 409) {
        if (window.confirm('本地与远端不同，确认用远端覆盖本地？')) {
          pullSync.mutate(true)
        }
        return
      }
      toast.show(`拉取失败：${err.message}`, 'error')
    },
  })

  const copy = async (label: string, text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(label)
      window.setTimeout(() => setCopied(null), 1500)
    } catch {
      toast.show('复制失败，请手动选中文本复制', 'error')
    }
  }

  const data = settings.data
  const logSuccess = data?.log_success === 'true'

  // 接入片段使用的网关 Key：默认选中「默认 Key」（最早的启用 Key，由 /api/settings 给出）。
  const gatewayKeys = useQuery({
    queryKey: ['gateway-keys', { includeDeleted: false }],
    queryFn: () => api.listGatewayKeys(),
  })
  const activeKeys = (gatewayKeys.data?.items ?? []).filter((key) => !key.deleted && key.enabled)

  useEffect(() => {
    if (snippetKeyId || activeKeys.length === 0) return
    const preferred = data?.gateway_key_id
    const picked = activeKeys.find((key) => key.id === preferred) ?? activeKeys[0]
    setSnippetKeyId(picked.id)
  }, [snippetKeyId, activeKeys, data?.gateway_key_id])

  // 选中 Key 的明文按需取回（旧记录只存哈希时取不到，片段退化为占位符）。
  useEffect(() => {
    if (!snippetKeyId) {
      setSnippetPlain('')
      return
    }
    // 用户已切换 Key：先清掉上一个 Key 的明文，避免串号
    setSnippetPlain('')
    let cancelled = false
    void (async () => {
      try {
        const revealed = await api.revealGatewayKey(snippetKeyId)
        if (!cancelled) setSnippetPlain(revealed.gateway_key)
      } catch {
        if (!cancelled) setSnippetPlain('')
      }
    })()
    return () => {
      cancelled = true
    }
  }, [snippetKeyId])

  const selectedSnippetKey = activeKeys.find((key) => key.id === snippetKeyId)
  const keyForSnippet = snippetPlain || '<你的网关 Key>'

  const origin = window.location.origin
  const openaiSnippet = `OPENAI_BASE_URL=${origin}/v1\nOPENAI_API_KEY=${keyForSnippet}`

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">网关设置</h1>
        <p className="mt-1 text-sm text-slate-500">
          所有 Agent 共用同一个入口；可按 Agent / 使用人发放多个网关 Key，各自设置模型范围。
        </p>
      </div>

      <GatewayKeysCard selectedKeyId={snippetKeyId} onSelectKey={setSnippetKeyId} />

      <Card>
        <CardHeader>
          <CardTitle>接入地址</CardTitle>
          <CardDescription>把下面的环境变量填进任意支持自定义 Base URL 的 Agent。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="snippet-key">使用哪个网关 Key</Label>
            <Select
              id="snippet-key"
              className="w-full sm:w-80"
              value={snippetKeyId}
              onChange={(event) => setSnippetKeyId(event.target.value)}
            >
              {activeKeys.length === 0 ? <option value="">（没有启用的 Key）</option> : null}
              {activeKeys.map((key) => (
                <option key={key.id} value={key.id}>
                  {key.name}
                  {key.note ? `（${key.note}）` : ''}
                </option>
              ))}
            </Select>
            <p className="text-xs text-slate-500">
              {snippetKeyId && !snippetPlain
                ? '该 Key 的明文取不到（旧记录只存哈希或主密钥已更换）：片段里是占位符，请到上方该 Key 的行点「重置」生成新明文。'
                : '选中的 Key 明文会自动填入下面的片段；明文只留在当前页面内存中。'}
            </p>
            {selectedSnippetKey ? (
              <p className="text-xs text-slate-500">
                模型范围：{selectedSnippetKey.scopes.length === 0 ? '不限制（可调用全部模型）' : `${selectedSnippetKey.scopes.length} 个模型`}
              </p>
            ) : null}
          </div>
          <div className="rounded-md border border-slate-200 p-3">
            <div className="flex items-center justify-between">
              <p className="text-sm font-medium">OpenAI 协议（Codex / Cursor / 任意 OpenAI 兼容工具）</p>
              <Button variant="ghost" size="sm" onClick={() => copy('openai', openaiSnippet)}>
                {copied === 'openai' ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                复制
              </Button>
            </div>
            <pre className="mt-2 overflow-x-auto rounded bg-slate-900 p-3 font-mono text-xs text-slate-100">
              {openaiSnippet}
            </pre>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>运行参数</CardTitle>
          <CardDescription>监听地址与请求上限需要修改启动参数后重启；日志开关即时生效。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="flex items-center gap-3">
            <Switch
              id="log-success"
              checked={logSuccess}
              onCheckedChange={(checked) => saveSettings.mutate({ log_success: checked })}
            />
            <div>
              <Label htmlFor="log-success">记录成功请求日志</Label>
              <p className="text-xs text-slate-500">默认关闭以避免写放大；失败请求始终记录。</p>
            </div>
          </div>

          <div className="grid gap-2 rounded-md bg-slate-50 p-3 text-sm sm:grid-cols-2">
            <p>
              <span className="text-slate-500">监听地址：</span>
              <code className="font-mono">
                {data?.listen}:{data?.port}
              </code>
            </p>
            <p>
              <span className="text-slate-500">SSE 心跳阈值：</span>
              <code className="font-mono">{data?.sse_idle_seconds}s</code>
            </p>
            <p>
              <span className="text-slate-500">请求体上限：</span>
              <code className="font-mono">{data?.max_body_bytes} 字节</code>
            </p>
            <p>
              <span className="text-slate-500">当前页面版本：</span>
              <code className="font-mono">{import.meta.env.MODE}</code>
            </p>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <CloudUpload className="h-4 w-4" />
            WebDAV 配置同步
          </CardTitle>
          <CardDescription>
            把供应商配置（API Key 明文）推到自有 WebDAV 服务器，或从远端拉取。
            凭证以 AES 加密落库（与供应商 API Key、网关 Key 一致），明文不会出现在 GET 响应里。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="grid gap-1.5 sm:col-span-2">
              <Label htmlFor="webdav-url">远程根目录</Label>
              <Input
                id="webdav-url"
                value={webdavUrl}
                onChange={(event) => setWebdavUrl(event.target.value)}
                placeholder="https://dav.example.com/agora/"
              />
              <p className="text-xs text-slate-500">
                推送 / 拉取都会读写固定文件
                <code className="mx-1 rounded bg-slate-100 px-1">agoramodel-providers.json</code>
                （位于该目录下）。
              </p>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="webdav-user">用户名</Label>
              <Input
                id="webdav-user"
                value={webdavUsername}
                onChange={(event) => setWebdavUsername(event.target.value)}
                placeholder="可选"
              />
            </div>
            <div className="grid gap-1.5 sm:col-span-2">
              <Label htmlFor="webdav-pass">密码</Label>
              <Input
                id="webdav-pass"
                type="password"
                autoComplete="new-password"
                value={webdavPassword}
                onChange={(event) => setWebdavPassword(event.target.value)}
                placeholder={
                  webdavConfig.data?.has_password ? '留空表示不修改' : '首次配置必须填写'
                }
              />
              <p className="text-xs text-slate-500">
                {webdavConfig.data?.has_password
                  ? '已有密码；留空表示保留，填新值则覆盖。'
                  : '尚未配置密码。'}
              </p>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            <Button onClick={() => saveWebDAV.mutate()} disabled={saveWebDAV.isPending}>
              <Save className="h-4 w-4" />
              {saveWebDAV.isPending ? '保存中…' : '保存 WebDAV 配置'}
            </Button>
            <Button
              variant="outline"
              onClick={() => testConnection.mutate()}
              disabled={testConnection.isPending || !webdavConfig.data?.configured}
              title="用当前配置连接一次远端，校验 URL / 用户名 / 密码"
            >
              <Plug className="h-4 w-4" />
              {testConnection.isPending ? '连接中…' : '测试连接'}
            </Button>
            <Button variant="outline" onClick={refreshStatus} disabled={webdavStatus.isFetching}>
              <RefreshCw className="h-4 w-4" />
              刷新状态
            </Button>
          </div>

          {webdavStatus.data?.configured ? (
            <div className="rounded-md border border-slate-200 bg-slate-50 px-3 py-2 text-sm">
              <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
                <span>
                  远端：{webdavStatus.data.has_remote ? '已存在' : '尚未上传'}
                </span>
                <span>
                  本地指纹：
                  <code className="ml-1 rounded bg-white px-1 font-mono">
                    {webdavStatus.data.local_fingerprint.slice(0, 12) || '…'}
                  </code>
                </span>
                {webdavStatus.data.has_remote ? (
                  <span>
                    远端指纹：
                    <code className="ml-1 rounded bg-white px-1 font-mono">
                      {(webdavStatus.data.remote_fingerprint ?? '').slice(0, 12)}
                    </code>
                  </span>
                ) : null}
                <span
                  className={
                    webdavStatus.data.match
                      ? 'text-emerald-700'
                      : 'text-amber-700'
                  }
                >
                  {webdavStatus.data.match ? '✓ 一致，无需同步' : '⚠ 不同，需手动同步'}
                </span>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <Button
                  size="sm"
                  onClick={() => pushSync.mutate(false)}
                  disabled={pushSync.isPending}
                >
                  <Upload className="h-4 w-4" />
                  {pushSync.isPending ? '推送中…' : '推送本地 → 远端'}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => pullSync.mutate(false)}
                  disabled={pullSync.isPending}
                >
                  <Download className="h-4 w-4" />
                  {pullSync.isPending ? '拉取中…' : '拉取远端 → 本地'}
                </Button>
                {webdavStatus.data.last_synced_at ? (
                  <span className="text-xs text-slate-500">
                    最近同步：{formatTime(webdavStatus.data.last_synced_at)}
                  </span>
                ) : null}
              </div>
              <p className="mt-2 text-xs text-slate-500">
                push/pull 会比对本地与远端指纹；若不一致会先弹确认（覆盖/取消）。
              </p>
            </div>
          ) : (
            <p className="text-xs text-slate-500">填写服务器地址与密码后即可同步（推送/拉取）。</p>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
