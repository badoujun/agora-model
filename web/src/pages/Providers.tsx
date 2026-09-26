import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CloudDownload, Pencil, Plug, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { api, ApiError } from '@/lib/api'
import type { ProviderDTO, ProviderInput, TestResult } from '@/lib/types'
import { formatRelative } from '@/lib/utils'
import { useToast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

interface FormState {
  id?: string
  name: string
  openai_base_url: string
  anthropic_base_url: string
  openai_endpoint_override: string
  anthropic_endpoint_override: string
  api_key: string
  models_manual: string
  models_excluded: string
  auto_fetch_models: boolean
  priority: number
  timeout_seconds: number
  extra_headers: string
  extra_body: string
  allow_internal: boolean
  enabled: boolean
}

const EMPTY_FORM: FormState = {
  name: '',
  openai_base_url: '',
  anthropic_base_url: '',
  openai_endpoint_override: '',
  anthropic_endpoint_override: '',
  api_key: '',
  models_manual: '',
  models_excluded: '',
  auto_fetch_models: true,
  priority: 100,
  timeout_seconds: 120,
  extra_headers: '',
  extra_body: '',
  allow_internal: false,
  enabled: true,
}

function toForm(p: ProviderDTO): FormState {
  return {
    id: p.id,
    name: p.name,
    openai_base_url: p.openai_base_url ?? '',
    anthropic_base_url: p.anthropic_base_url ?? '',
    openai_endpoint_override: p.openai_endpoint_override ?? '',
    anthropic_endpoint_override: p.anthropic_endpoint_override ?? '',
    api_key: '',
    models_manual: (p.models_manual ?? []).join('\n'),
    models_excluded: (p.models_excluded ?? []).join('\n'),
    auto_fetch_models: p.auto_fetch_models,
    priority: p.priority,
    timeout_seconds: p.timeout_seconds,
    extra_headers: p.extra_headers && Object.keys(p.extra_headers).length ? JSON.stringify(p.extra_headers, null, 2) : '',
    extra_body: p.extra_body && Object.keys(p.extra_body).length ? JSON.stringify(p.extra_body, null, 2) : '',
    allow_internal: p.allow_internal,
    enabled: p.enabled,
  }
}

function splitLines(raw: string): string[] {
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
}

function parseJSONField(raw: string, label: string): Record<string, unknown> | undefined {
  const text = raw.trim()
  if (text === '') return undefined
  try {
    const parsed = JSON.parse(text)
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error('必须是 JSON 对象')
    }
    return parsed as Record<string, unknown>
  } catch (err) {
    throw new Error(`${label} 不是合法 JSON：${(err as Error).message}`)
  }
}

function toInput(form: FormState): ProviderInput {
  const headers = parseJSONField(form.extra_headers, '额外请求头')
  const body = parseJSONField(form.extra_body, '额外请求体')

  const input: ProviderInput = {
    name: form.name.trim(),
    openai_base_url: form.openai_base_url.trim(),
    anthropic_base_url: form.anthropic_base_url.trim(),
    openai_endpoint_override: form.openai_endpoint_override.trim(),
    anthropic_endpoint_override: form.anthropic_endpoint_override.trim(),
    models_manual: splitLines(form.models_manual),
    models_excluded: splitLines(form.models_excluded),
    auto_fetch_models: form.auto_fetch_models,
    priority: form.priority,
    timeout_seconds: form.timeout_seconds,
    allow_internal: form.allow_internal,
    enabled: form.enabled,
  }
  if (form.api_key.trim() !== '') {
    input.api_key = form.api_key.trim()
  }
  if (headers) {
    input.extra_headers = headers as Record<string, string>
  }
  if (body) {
    input.extra_body = body
  }
  return input
}

export function ProvidersPage() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const [dialogOpen, setDialogOpen] = useState(false)
  const [form, setForm] = useState<FormState>(EMPTY_FORM)
  const [formError, setFormError] = useState('')
  const [testResult, setTestResult] = useState<{ provider: ProviderDTO; result: TestResult } | null>(null)

  const providers = useQuery({ queryKey: ['providers'], queryFn: api.listProviders })

  const invalidateAll = async () => {
    await queryClient.invalidateQueries({ queryKey: ['providers'] })
    await queryClient.invalidateQueries({ queryKey: ['models'] })
  }

  const save = useMutation({
    mutationFn: async (state: FormState) => {
      const input = toInput(state)
      return state.id ? api.updateProvider(state.id, input) : api.createProvider(input)
    },
    onSuccess: async (saved) => {
      setDialogOpen(false)
      await invalidateAll()
      toast.show(`已保存供应商「${saved.name || saved.id}」`, 'success')
    },
    onError: (err: Error) => setFormError(err instanceof ApiError ? err.message : String(err)),
  })

  const remove = useMutation({
    mutationFn: api.deleteProvider,
    onSuccess: async () => {
      await invalidateAll()
      toast.show('已删除供应商', 'success')
    },
    onError: (err: Error) => toast.show(`删除失败：${err.message}`, 'error'),
  })

  const fetchModels = useMutation({
    mutationFn: api.fetchModels,
    onSuccess: async (updated) => {
      await invalidateAll()
      toast.show(`已拉取模型：${updated.model_count} 个`, 'success')
    },
    onError: (err: Error) => toast.show(`拉取模型失败：${err.message}`, 'error'),
  })

  const test = useMutation({
    mutationFn: (provider: ProviderDTO) => api.testProvider(provider.id).then((result) => ({ provider, result })),
    onSuccess: (payload) => setTestResult(payload),
    onError: (err: Error) => toast.show(`连接测试失败：${err.message}`, 'error'),
  })

  const items = providers.data?.items ?? []
  const summary = useMemo(() => {
    const enabled = items.filter((item) => item.enabled).length
    const models = items.reduce((total, item) => total + item.model_count, 0)
    return { enabled, models }
  }, [items])

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setFormError('')
    setDialogOpen(true)
  }

  const openEdit = (provider: ProviderDTO) => {
    setForm(toForm(provider))
    setFormError('')
    setDialogOpen(true)
  }

  const submit = () => {
    setFormError('')
    if (form.name.trim() === '') {
      setFormError('请填写名称')
      return
    }
    if (form.openai_base_url.trim() === '' && form.anthropic_base_url.trim() === '') {
      setFormError('至少需要填写一个协议地址（OpenAI 或 Anthropic）')
      return
    }
    if (!form.id && form.api_key.trim() === '') {
      setFormError('新建供应商必须填写 API Key')
      return
    }
    if (form.priority <= 0 || form.timeout_seconds <= 0) {
      setFormError('优先级与超时时间必须为正整数')
      return
    }
    save.mutate(form)
  }

  return (
    <div className="flex flex-col gap-5">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-xl font-semibold text-slate-900">供应商管理</h1>
          <p className="mt-1 text-sm text-slate-500">
            新增供应商只需填写名称、两个协议地址与 API Key——所有 Agent 立即生效，无需为每个 Agent 重复配置。
          </p>
        </div>
        <Button onClick={openCreate}>
          <Plus className="h-4 w-4" />
          新增供应商
        </Button>
      </div>

      {testResult ? (
        <Card className="border-sky-200 bg-sky-50/60">
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <div>
              <CardTitle>连接测试：{testResult.provider.name || testResult.provider.id}</CardTitle>
              <CardDescription>
                {new Date(testResult.result.checked_at).toLocaleString('zh-CN', { hour12: false })}
              </CardDescription>
            </div>
            <Button variant="ghost" size="sm" onClick={() => setTestResult(null)}>
              关闭
            </Button>
          </CardHeader>
          <CardContent className="grid gap-2 sm:grid-cols-2">
            {(
              [
                ['OpenAI', testResult.result.openai],
                ['Anthropic', testResult.result.anthropic],
              ] as const
            ).map(([label, probe]) => (
              <div key={label} className="rounded-md border border-slate-200 bg-white px-3 py-2 text-sm">
                <div className="flex items-center gap-2">
                  <Badge variant={probe.ok ? 'success' : 'danger'}>{probe.ok ? '通过' : '失败'}</Badge>
                  <span className="font-medium">{label}</span>
                  {probe.status_code ? (
                    <span className="text-xs text-slate-500">
                      HTTP {probe.status_code} · {probe.latency_ms} ms
                      {probe.model_count ? ` · ${probe.model_count} 个模型` : ''}
                    </span>
                  ) : null}
                </div>
                <p className="mt-1 break-all text-xs text-slate-500">{probe.message}</p>
              </div>
            ))}
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>已配置的供应商</CardTitle>
          <CardDescription>
            共 {items.length} 个（启用 {summary.enabled} 个）· 聚合模型 {summary.models} 条映射
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {providers.isLoading ? (
            <p className="px-5 py-8 text-sm text-slate-500">加载中…</p>
          ) : items.length === 0 ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">
              <p>还没有供应商。</p>
              <p className="mt-1">点击右上角「新增供应商」，填写双协议地址与 API Key 即可开始使用。</p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>名称</TableHead>
                  <TableHead>OpenAI 地址</TableHead>
                  <TableHead>Anthropic 地址</TableHead>
                  <TableHead>模型</TableHead>
                  <TableHead>优先级</TableHead>
                  <TableHead>最近拉取</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead className="text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((provider) => (
                  <TableRow key={provider.id}>
                    <TableCell>
                      <div className="font-medium">{provider.name || provider.id}</div>
                      <div className="font-mono text-[11px] text-slate-400">{provider.id}</div>
                    </TableCell>
                    <TableCell className="max-w-[220px] truncate font-mono text-xs">
                      {provider.openai_base_url || <span className="text-slate-400">未配置</span>}
                    </TableCell>
                    <TableCell className="max-w-[220px] truncate font-mono text-xs">
                      {provider.anthropic_base_url || <span className="text-slate-400">未配置</span>}
                    </TableCell>
                    <TableCell>{provider.model_count}</TableCell>
                    <TableCell>{provider.priority}</TableCell>
                    <TableCell className="text-xs">
                      {provider.last_fetch_error ? (
                        <span className="text-red-600" title={provider.last_fetch_error}>
                          失败 · {formatRelative(provider.last_fetch_at)}
                        </span>
                      ) : (
                        formatRelative(provider.last_fetch_at)
                      )}
                    </TableCell>
                    <TableCell>
                      {provider.enabled ? (
                        <Badge variant="success">启用</Badge>
                      ) : (
                        <Badge>停用</Badge>
                      )}
                      {provider.allow_internal ? (
                        <Badge variant="warning" className="ml-1">
                          内网
                        </Badge>
                      ) : null}
                    </TableCell>
                    <TableCell>
                      <div className="flex items-center justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => test.mutate(provider)}
                          disabled={test.isPending}
                        >
                          <Plug className="h-4 w-4" />
                          测试
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => fetchModels.mutate(provider.id)}
                          disabled={fetchModels.isPending}
                        >
                          {fetchModels.isPending ? (
                            <RefreshCw className="h-4 w-4 animate-spin" />
                          ) : (
                            <CloudDownload className="h-4 w-4" />
                          )}
                          拉取模型
                        </Button>
                        <Button variant="ghost" size="sm" onClick={() => openEdit(provider)}>
                          <Pencil className="h-4 w-4" />
                          编辑
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-red-600 hover:bg-red-50"
                          onClick={() => {
                            if (window.confirm(`确认删除供应商「${provider.name || provider.id}」？`)) {
                              remove.mutate(provider.id)
                            }
                          }}
                        >
                          <Trash2 className="h-4 w-4" />
                          删除
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Dialog
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        title={form.id ? '编辑供应商' : '新增供应商'}
        description="上游只需支持 OpenAI 或 Anthropic 之一即可接入；两者都支持时可在同一条配置里并行提供。"
        footer={
          <>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>
              取消
            </Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? '保存中…' : '保存'}
            </Button>
          </>
        }
      >
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="p-name">名称 *</Label>
            <Input
              id="p-name"
              value={form.name}
              onChange={(event) => setForm({ ...form, name: event.target.value })}
              placeholder="例如：供应商A"
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="p-openai">OpenAI Base URL</Label>
              <Input
                id="p-openai"
                value={form.openai_base_url}
                onChange={(event) => setForm({ ...form, openai_base_url: event.target.value })}
                placeholder="https://api.example.com/v1"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="p-anthropic">Anthropic Base URL</Label>
              <Input
                id="p-anthropic"
                value={form.anthropic_base_url}
                onChange={(event) => setForm({ ...form, anthropic_base_url: event.target.value })}
                placeholder="https://api.example.com"
              />
            </div>
          </div>

          <div className="grid gap-1.5">
            <Label htmlFor="p-key">API Key {form.id ? '' : '*'}</Label>
            <Input
              id="p-key"
              type="password"
              autoComplete="new-password"
              value={form.api_key}
              onChange={(event) => setForm({ ...form, api_key: event.target.value })}
              placeholder={form.id ? '留空表示不修改当前凭证' : '供应商的真实 API Key'}
            />
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="p-models">手动模型（每行一个）</Label>
              <textarea
                id="p-models"
                rows={4}
                className="w-full rounded-md border border-slate-300 px-3 py-2 font-mono text-xs shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
                value={form.models_manual}
                onChange={(event) => setForm({ ...form, models_manual: event.target.value })}
                placeholder={'gpt-4o\nclaude-sonnet-4-5'}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="p-excluded">排除模型（每行一个）</Label>
              <textarea
                id="p-excluded"
                rows={4}
                className="w-full rounded-md border border-slate-300 px-3 py-2 font-mono text-xs shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
                value={form.models_excluded}
                onChange={(event) => setForm({ ...form, models_excluded: event.target.value })}
                placeholder="不希望出现在聚合列表里的模型"
              />
            </div>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="p-priority">优先级（越小越优先）</Label>
              <Input
                id="p-priority"
                type="number"
                min={1}
                value={form.priority}
                onChange={(event) => setForm({ ...form, priority: Number(event.target.value) })}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="p-timeout">超时（秒）</Label>
              <Input
                id="p-timeout"
                type="number"
                min={1}
                value={form.timeout_seconds}
                onChange={(event) => setForm({ ...form, timeout_seconds: Number(event.target.value) })}
              />
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-6">
            <div className="flex items-center gap-2">
              <Switch
                id="p-auto"
                checked={form.auto_fetch_models}
                onCheckedChange={(checked) => setForm({ ...form, auto_fetch_models: checked })}
              />
              <Label htmlFor="p-auto">自动拉取模型</Label>
            </div>
            <div className="flex items-center gap-2">
              <Switch
                id="p-enabled"
                checked={form.enabled}
                onCheckedChange={(checked) => setForm({ ...form, enabled: checked })}
              />
              <Label htmlFor="p-enabled">启用</Label>
            </div>
            <div className="flex items-center gap-2">
              <Switch
                id="p-internal"
                checked={form.allow_internal}
                onCheckedChange={(checked) => setForm({ ...form, allow_internal: checked })}
              />
              <Label htmlFor="p-internal">允许内网地址</Label>
            </div>
          </div>

          <details className="rounded-md border border-slate-200 px-3 py-2">
            <summary className="cursor-pointer text-sm font-medium text-slate-700">高级选项</summary>
            <div className="mt-3 grid gap-4">
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="grid gap-1.5">
                  <Label htmlFor="p-openai-endpoint">OpenAI 端点覆盖</Label>
                  <Input
                    id="p-openai-endpoint"
                    value={form.openai_endpoint_override}
                    onChange={(event) =>
                      setForm({ ...form, openai_endpoint_override: event.target.value })
                    }
                    placeholder="路径非标准时填写完整 URL"
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="p-anthropic-endpoint">Anthropic 端点覆盖</Label>
                  <Input
                    id="p-anthropic-endpoint"
                    value={form.anthropic_endpoint_override}
                    onChange={(event) =>
                      setForm({ ...form, anthropic_endpoint_override: event.target.value })
                    }
                    placeholder="路径非标准时填写完整 URL"
                  />
                </div>
              </div>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="grid gap-1.5">
                  <Label htmlFor="p-headers">额外请求头（JSON）</Label>
                  <textarea
                    id="p-headers"
                    rows={4}
                    className="w-full rounded-md border border-slate-300 px-3 py-2 font-mono text-xs shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
                    value={form.extra_headers}
                    onChange={(event) => setForm({ ...form, extra_headers: event.target.value })}
                    placeholder={'{\n  "X-Tenant": "me"\n}'}
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="p-body">额外请求体（JSON）</Label>
                  <textarea
                    id="p-body"
                    rows={4}
                    className="w-full rounded-md border border-slate-300 px-3 py-2 font-mono text-xs shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400"
                    value={form.extra_body}
                    onChange={(event) => setForm({ ...form, extra_body: event.target.value })}
                    placeholder={'{\n  "temperature": 0.1\n}'}
                  />
                </div>
              </div>
            </div>
          </details>

          {formError ? <p className="text-sm text-red-600">{formError}</p> : null}
        </div>
      </Dialog>
    </div>
  )
}
