import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, CloudDownload, Pencil, Plug, Plus, RefreshCw, Trash2 } from 'lucide-react'
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
  openai_endpoint_override: string
  api_key: string
  /** 候选与已选模型的展示顺序（去重） */
  modelOrder: string[]
  /** 上游模型名 -> 是否勾选启用 */
  selected: Record<string, boolean>
  /** 上游模型名 -> 别称（空串表示不设别称） */
  aliases: Record<string, string>
  timeout_seconds: number
  extra_headers: string
  extra_body: string
  allow_internal: boolean
  enabled: boolean
}

const EMPTY_FORM: FormState = {
  name: '',
  openai_base_url: '',
  openai_endpoint_override: '',
  api_key: '',
  modelOrder: [],
  selected: {},
  aliases: {},
  timeout_seconds: 120,
  extra_headers: '',
  extra_body: '',
  allow_internal: false,
  enabled: true,
}

function mergeModels(...lists: string[][]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const list of lists) {
    for (const raw of list) {
      const model = raw.trim()
      if (model === '' || seen.has(model)) continue
      seen.add(model)
      out.push(model)
    }
  }
  return out
}

function toForm(p: ProviderDTO): FormState {
  const selected: Record<string, boolean> = {}
  for (const model of p.models_selected ?? []) {
    selected[model] = true
  }
  return {
    id: p.id,
    name: p.name,
    openai_base_url: p.openai_base_url ?? '',
    openai_endpoint_override: p.openai_endpoint_override ?? '',
    api_key: '',
    modelOrder: mergeModels(p.models_selected ?? [], p.candidate_models ?? []),
    selected,
    aliases: { ...(p.model_aliases ?? {}) },
    timeout_seconds: p.timeout_seconds,
    extra_headers: p.extra_headers && Object.keys(p.extra_headers).length ? JSON.stringify(p.extra_headers, null, 2) : '',
    extra_body: p.extra_body && Object.keys(p.extra_body).length ? JSON.stringify(p.extra_body, null, 2) : '',
    allow_internal: p.allow_internal,
    enabled: p.enabled,
  }
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

  const modelsSelected = form.modelOrder.filter((model) => form.selected[model])
  const aliases: Record<string, string> = {}
  for (const model of modelsSelected) {
    const alias = (form.aliases[model] ?? '').trim()
    if (alias !== '') aliases[model] = alias
  }

  const input: ProviderInput = {
    name: form.name.trim(),
    openai_base_url: form.openai_base_url.trim(),
    openai_endpoint_override: form.openai_endpoint_override.trim(),
    models_selected: modelsSelected,
    model_aliases: aliases,
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
  const [modelFilter, setModelFilter] = useState('')
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
    onSuccess: async (saved, variables) => {
      await invalidateAll()
      setForm((prev) => ({ ...toForm(saved), aliases: { ...prev.aliases } }))
      if (variables.id) {
        setDialogOpen(false)
        toast.show(`已保存供应商「${saved.name || saved.id}」`, 'success')
        return
      }
      // 新建成功后保持编辑状态，方便立即拉取并勾选模型
      toast.show(`已创建「${saved.name || saved.id}」，正在拉取模型…`, 'success')
      fetchModels.mutate(saved.id)
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
      setForm((prev) => ({
        ...prev,
        modelOrder: mergeModels(prev.modelOrder, updated.candidate_models ?? []),
        // 首次拉取：把候选全部勾上，用户再按需取消
        selected: Object.keys(prev.selected).length === 0
          ? Object.fromEntries((updated.candidate_models ?? []).map((m) => [m, true]))
          : prev.selected,
      }))
      await invalidateAll()
      toast.show(`已拉取 ${updated.candidate_models?.length ?? 0} 个候选模型，请勾选要启用的模型`, 'success')
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
    setModelFilter('')
    setFormError('')
    setDialogOpen(true)
  }

  const openEdit = (provider: ProviderDTO) => {
    setForm(toForm(provider))
    setModelFilter('')
    setFormError('')
    setDialogOpen(true)
  }

  const selectedCount = useMemo(
    () => form.modelOrder.filter((model) => form.selected[model]).length,
    [form.modelOrder, form.selected],
  )

  const visibleModels = useMemo(() => {
    const keyword = modelFilter.trim().toLowerCase()
    if (!keyword) return form.modelOrder
    return form.modelOrder.filter((model) => model.toLowerCase().includes(keyword))
  }, [form.modelOrder, modelFilter])

  const toggleModel = (model: string, checked: boolean) => {
    setForm((prev) => ({ ...prev, selected: { ...prev.selected, [model]: checked } }))
  }

  const setAlias = (model: string, value: string) => {
    setForm((prev) => ({ ...prev, aliases: { ...prev.aliases, [model]: value } }))
  }

  const submit = () => {
    setFormError('')
    if (form.name.trim() === '') {
      setFormError('请填写名称')
      return
    }
    if (form.openai_base_url.trim() === '' && form.openai_endpoint_override.trim() === '') {
      setFormError('请填写 OpenAI Base URL（或端点覆盖）')
      return
    }
    if (!form.id && form.api_key.trim() === '') {
      setFormError('新建供应商必须填写 API Key')
      return
    }
    if (form.timeout_seconds <= 0) {
      setFormError('超时时间必须为正整数')
      return
    }
    const aliases = Object.entries(form.aliases)
      .filter(([model, alias]) => form.selected[model] && alias.trim() !== '')
      .map(([, alias]) => alias.trim().toLowerCase())
    if (new Set(aliases).size !== aliases.length) {
      setFormError('别称不能重复（同一个供应商内每个模型要有唯一的对外名称）')
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
            填写名称、OpenAI 兼容地址与 API Key，在编辑页拉取并勾选要启用的模型——所有 Agent 立即生效。
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
          <CardContent>
            <div className="rounded-md border border-slate-200 bg-white px-3 py-2 text-sm">
              <div className="flex items-center gap-2">
                <Badge variant={testResult.result.ok ? 'success' : 'danger'}>
                  {testResult.result.ok ? '通过' : '失败'}
                </Badge>
                {testResult.result.status_code ? (
                  <span className="text-xs text-slate-500">
                    HTTP {testResult.result.status_code} · {testResult.result.latency_ms} ms
                    {testResult.result.model_count ? ` · 上游 ${testResult.result.model_count} 个模型` : ''}
                  </span>
                ) : null}
              </div>
              <p className="mt-1 break-all text-xs text-slate-500">{testResult.result.message}</p>
            </div>
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>已配置的供应商</CardTitle>
          <CardDescription>
            共 {items.length} 个（启用 {summary.enabled} 个）· 已启用模型 {summary.models} 条映射
          </CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {providers.isLoading ? (
            <p className="px-5 py-8 text-sm text-slate-500">加载中…</p>
          ) : items.length === 0 ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">
              <p>还没有供应商。</p>
              <p className="mt-1">点击右上角「新增供应商」，填写上游地址与 API Key 即可开始使用。</p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>名称</TableHead>
                  <TableHead>OpenAI 地址</TableHead>
                  <TableHead>已启用模型</TableHead>
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
                    <TableCell className="max-w-[260px] truncate font-mono text-xs">
                      {provider.openai_base_url || <span className="text-slate-400">未配置</span>}
                    </TableCell>
                    <TableCell>{provider.model_count}</TableCell>
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
        description="上游只需提供 OpenAI 兼容接口；模型在此处拉取后勾选，未勾选的模型不会对外暴露。"
        footer={
          <>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>
              取消
            </Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? '保存中…' : form.id ? '保存' : '保存并拉取模型'}
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
            <p className="text-xs text-slate-500">
              名称即模型命名空间前缀（<code className="rounded bg-slate-100 px-1">{`${form.name || '名称'}/模型名`}</code>），需唯一。
            </p>
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
          </div>

          <div className="grid gap-2 rounded-md border border-slate-200 p-3">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <p className="text-sm font-medium text-slate-800">模型</p>
                <p className="text-xs text-slate-500">
                  已勾选 {selectedCount} 个 / 候选 {form.modelOrder.length} 个。别称即对外模型名，转发时自动换回上游模型名。
                </p>
              </div>
              <div className="flex items-center gap-2">
                <Input
                  value={modelFilter}
                  onChange={(event) => setModelFilter(event.target.value)}
                  placeholder="搜索模型"
                  className="w-40"
                />
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => form.id && fetchModels.mutate(form.id)}
                  disabled={!form.id || fetchModels.isPending}
                  title={form.id ? '从上游 /models 拉取候选模型' : '请先保存供应商，再拉取模型'}
                >
                  {fetchModels.isPending ? (
                    <RefreshCw className="h-4 w-4 animate-spin" />
                  ) : (
                    <CloudDownload className="h-4 w-4" />
                  )}
                  拉取模型
                </Button>
              </div>
            </div>

            {!form.id ? (
              <p className="text-xs text-amber-700">保存后即可拉取上游模型并勾选。</p>
            ) : null}

            {form.modelOrder.length === 0 ? (
              <p className="py-4 text-center text-xs text-slate-500">
                还没有模型。点击「拉取模型」从上游 <code className="rounded bg-slate-100 px-1">/models</code> 获取候选列表。
              </p>
            ) : (
              <div className="max-h-72 overflow-y-auto rounded-md border border-slate-100">
                {visibleModels.length === 0 ? (
                  <p className="px-3 py-4 text-center text-xs text-slate-500">没有匹配的模型</p>
                ) : (
                  visibleModels.map((model) => {
                    const checked = Boolean(form.selected[model])
                    return (
                      <div
                        key={model}
                        className="flex items-center gap-3 border-b border-slate-100 px-3 py-2 last:border-b-0"
                      >
                        <label className="flex flex-1 items-center gap-2">
                          <input
                            type="checkbox"
                            className="h-4 w-4 rounded border-slate-300"
                            checked={checked}
                            onChange={(event) => toggleModel(model, event.target.checked)}
                          />
                          <span className="font-mono text-xs text-slate-700">{model}</span>
                        </label>
                        <Input
                          value={form.aliases[model] ?? ''}
                          onChange={(event) => setAlias(model, event.target.value)}
                          disabled={!checked}
                          placeholder="别称（可选）"
                          className="w-44"
                        />
                      </div>
                    )
                  })
                )}
              </div>
            )}

            {form.modelOrder.length > 0 ? (
              <div className="flex items-center gap-3 text-xs text-slate-500">
                <button
                  type="button"
                  className="underline"
                  onClick={() =>
                    setForm((prev) => ({
                      ...prev,
                      selected: Object.fromEntries(prev.modelOrder.map((model) => [model, true])),
                    }))
                  }
                >
                  全选
                </button>
                <button
                  type="button"
                  className="underline"
                  onClick={() => setForm((prev) => ({ ...prev, selected: {} }))}
                >
                  清空
                </button>
                {selectedCount > 0 ? (
                  <span className="flex items-center gap-1 text-emerald-700">
                    <Check className="h-3 w-3" />
                    对外模型：{' '}
                    <code className="rounded bg-slate-100 px-1">
                      {form.modelOrder
                        .filter((model) => form.selected[model])
                        .map((model) => (form.aliases[model] ?? '').trim() || model)
                        .join(', ')}
                    </code>
                  </span>
                ) : null}
              </div>
            ) : null}
          </div>

          <div className="flex flex-wrap items-center gap-6">
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
                  <Label htmlFor="p-endpoint">OpenAI 端点覆盖</Label>
                  <Input
                    id="p-endpoint"
                    value={form.openai_endpoint_override}
                    onChange={(event) => setForm({ ...form, openai_endpoint_override: event.target.value })}
                    placeholder="路径非标准时填写完整 URL"
                  />
                  <p className="text-xs text-slate-500">填写后不再支持拉取模型（无法推导 /models）。</p>
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
