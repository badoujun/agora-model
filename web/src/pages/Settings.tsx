import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Copy, KeyRound, RefreshCw } from 'lucide-react'
import { api } from '@/lib/api'
import { formatTime } from '@/lib/utils'
import { useToast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'

export function SettingsPage() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: api.getSettings })

  const [newKey, setNewKey] = useState<string | null>(null)
  const [acknowledged, setAcknowledged] = useState(false)
  const [refreshSeconds, setRefreshSeconds] = useState('')
  const [copied, setCopied] = useState<string | null>(null)

  const resetKey = useMutation({
    mutationFn: api.resetGatewayKey,
    onSuccess: async (data) => {
      setNewKey(data.gateway_key)
      setAcknowledged(false)
      await queryClient.invalidateQueries({ queryKey: ['settings'] })
      toast.show('已生成新的网关 Key：旧 Key 立即失效，请更新所有 Agent 配置', 'success')
    },
    onError: (err: Error) => toast.show(`重置失败：${err.message}`, 'error'),
  })

  const saveSettings = useMutation({
    mutationFn: (payload: { model_refresh_seconds?: number; log_success?: boolean }) =>
      api.updateSettings(payload),
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

  const origin = window.location.origin
  const openaiSnippet = `OPENAI_BASE_URL=${origin}/v1\nOPENAI_API_KEY=<你的网关 Key>`
  const anthropicSnippet = `ANTHROPIC_BASE_URL=${origin}\nANTHROPIC_API_KEY=<你的网关 Key>`

  const data = settings.data
  const currentRefresh = refreshSeconds !== '' ? refreshSeconds : (data?.model_refresh_seconds ?? '600')
  const logSuccess = data?.log_success === 'true'

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
          <CardDescription>明文只在生成时显示一次；数据库里只保存哈希与掩码。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <code className="rounded bg-slate-100 px-2 py-1 font-mono text-sm">
              {data?.gateway_key_hint || '（尚未生成）'}
            </code>
            <span className="text-xs text-slate-500">
              创建于 {formatTime(data?.gateway_key_created_at)} · 最近使用 {formatTime(data?.gateway_key_last_used)}
            </span>
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
              {resetKey.isPending ? <RefreshCw className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />}
              重置网关 Key
            </Button>
          </div>

          {newKey ? (
            <div className="rounded-md border border-amber-200 bg-amber-50 p-3">
              <p className="text-sm font-medium text-amber-900">请立即保存这个 Key（只显示这一次）</p>
              <div className="mt-2 flex items-center gap-2">
                <code className="flex-1 break-all rounded bg-white px-2 py-1 font-mono text-sm">{newKey}</code>
                <Button variant="outline" size="sm" onClick={() => copy('new-key', newKey)}>
                  {copied === 'new-key' ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                  复制
                </Button>
              </div>
              <label className="mt-2 flex items-center gap-2 text-sm text-amber-900">
                <input type="checkbox" checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} />
                我已保存，可以隐藏
              </label>
              {acknowledged ? (
                <Button variant="ghost" size="sm" className="mt-1" onClick={() => setNewKey(null)}>
                  隐藏
                </Button>
              ) : null}
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>接入地址</CardTitle>
          <CardDescription>把下面的环境变量填进任意 Agent（只要它支持自定义 Base URL）。</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2">
          <div className="rounded-md border border-slate-200 p-3">
            <div className="flex items-center justify-between">
              <p className="text-sm font-medium">Anthropic 协议（Claude Code 等）</p>
              <Button variant="ghost" size="sm" onClick={() => copy('anthropic', anthropicSnippet)}>
                {copied === 'anthropic' ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                复制
              </Button>
            </div>
            <pre className="mt-2 overflow-x-auto rounded bg-slate-900 p-3 font-mono text-xs text-slate-100">
              {anthropicSnippet}
            </pre>
          </div>
          <div className="rounded-md border border-slate-200 p-3">
            <div className="flex items-center justify-between">
              <p className="text-sm font-medium">OpenAI 协议（Codex / Cursor 等）</p>
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
          <CardDescription>监听地址与请求上限需要修改启动参数后重启；刷新间隔与日志开关即时生效。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="refresh-interval">模型刷新间隔（秒）</Label>
              <div className="flex items-center gap-2">
                <Input
                  id="refresh-interval"
                  type="number"
                  min={30}
                  value={currentRefresh}
                  onChange={(event) => setRefreshSeconds(event.target.value)}
                />
                <Button
                  variant="outline"
                  onClick={() => {
                    const value = Number(currentRefresh)
                    if (!Number.isFinite(value) || value < 30) {
                      toast.show('刷新间隔不能小于 30 秒', 'error')
                      return
                    }
                    saveSettings.mutate({ model_refresh_seconds: value })
                  }}
                  disabled={saveSettings.isPending}
                >
                  保存
                </Button>
              </div>
            </div>
            <div className="flex items-center gap-3 pt-6">
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
