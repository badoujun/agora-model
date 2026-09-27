import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Copy, KeyRound, RefreshCw } from 'lucide-react'
import { api } from '@/lib/api'
import { formatTime } from '@/lib/utils'
import { useToast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

export function SettingsPage() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: api.getSettings })

  const [newKey, setNewKey] = useState<string | null>(null)
  const [copied, setCopied] = useState<string | null>(null)

  const resetKey = useMutation({
    mutationFn: api.resetGatewayKey,
    onSuccess: async (data) => {
      setNewKey(data.gateway_key)
      await queryClient.invalidateQueries({ queryKey: ['settings'] })
      toast.show('已生成新的网关 Key：旧 Key 立即失效，请更新所有 Agent 配置', 'success')
    },
    onError: (err: Error) => toast.show(`重置失败：${err.message}`, 'error'),
  })
  const saveSettings = useMutation({
    mutationFn: (payload: { log_success?: boolean }) => api.updateSettings(payload),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['settings'] })
      toast.show('设置已保存', 'success')
    },
    onError: (err: Error) => toast.show(`保存失败：${err.message}`, 'error'),
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
  // 当前网关 Key 的明文（可随时查看/复制）；旧版本的 Key 只能重置
  const currentKey = data?.gateway_key || ''
  // 接入示例里的 Key 直接带上明文，方便一键复制
  const keyForSnippet = currentKey || '<你的网关 Key>'

  const origin = window.location.origin
  const openaiSnippet = `OPENAI_BASE_URL=${origin}/v1\nOPENAI_API_KEY=${keyForSnippet}`

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">网关设置</h1>
        <p className="mt-1 text-sm text-slate-500">
          所有 Agent 共用同一个入口与同一个 Key；新增供应商只需在「供应商管理」里填一次。
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <KeyRound className="h-4 w-4" />
            网关 API Key
          </CardTitle>
          <CardDescription>
            所有 Agent 共用这一个 Key；明文以 AES 加密保存在本机数据库中，仅同源控制台可查看，可随时复制。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <code className="min-w-0 flex-1 break-all rounded bg-slate-100 px-2 py-1 font-mono text-sm">
              {currentKey || '（尚未生成）'}
            </code>
            <Button
              variant="outline"
              size="sm"
              onClick={() => copy('current-key', currentKey)}
              disabled={!currentKey}
              title={currentKey ? '复制网关 Key' : '暂无可复制的明文'}
            >
              {copied === 'current-key' ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
              复制
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                if (window.confirm('重置后旧 Key 会立即失效，需要更新所有 Agent 配置。确认继续？')) {
                  resetKey.mutate()
                }
              }}
              disabled={resetKey.isPending}
            >
              <RefreshCw className="h-4 w-4" />
              重置网关 Key
            </Button>
          </div>
          <span className="text-xs text-slate-500">
            创建于 {formatTime(data?.gateway_key_created_at)} · 最近使用 {formatTime(data?.gateway_key_last_used)}
            {data?.gateway_key_hint ? ` · 掩码 ${data.gateway_key_hint}` : ''}
          </span>

          {data && data.gateway_key_hint && !data.gateway_key_revealable ? (
            <p className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-900">
              无法还原这个 Key 的明文：可能是旧版本创建的 Key（数据库里只保存了哈希），或主密钥被更换后无法解密已保存的密文。
              点「重置网关 Key」即可生成一个可随时复制的新 Key。
            </p>
          ) : null}

          {newKey ? (
            <div className="rounded-md border border-amber-200 bg-amber-50 p-3">
              <p className="text-sm font-medium text-amber-900">已生成新的网关 Key（旧 Key 已失效）</p>
              <div className="mt-2 flex items-center gap-2">
                <code className="min-w-0 flex-1 break-all rounded bg-white px-2 py-1 font-mono text-sm">{newKey}</code>
                <Button variant="outline" size="sm" onClick={() => copy('new-key', newKey)}>
                  {copied === 'new-key' ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                  复制
                </Button>
                <Button variant="ghost" size="sm" onClick={() => setNewKey(null)}>
                  知道了
                </Button>
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>接入地址</CardTitle>
          <CardDescription>把下面的环境变量填进任意支持自定义 Base URL 的 Agent。</CardDescription>
        </CardHeader>
        <CardContent>
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
    </div>
  )
}
