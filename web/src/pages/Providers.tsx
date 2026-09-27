import { useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Check,
  CloudDownload,
  Download,
  Pencil,
  Plug,
  Plus,
  RefreshCw,
  Trash2,
  Upload,
} from 'lucide-react'
import { api, ApiError } from '@/lib/api'
import type { DraftFetchInput, ProviderDTO, ProviderInput, TestResult } from '@/lib/types'
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
  /** 供应商官网地址（仅展示用） */
  website_url: string
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
  website_url: '',
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
    website_url: p.website_url ?? '',
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
    // 始终传官网地址：空串表示清空
    website_url: form.website_url.trim(),
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

/** 未保存的供应商只能按表单里填写的信息拉取：单独组装草稿请求体。 */
function toDraftInput(form: FormState): DraftFetchInput {
  const headers = parseJSONField(form.extra_headers, '额外请求头') as Record<string, string> | undefined
  return {
    openai_base_url: form.openai_base_url.trim(),
    openai_endpoint_override: form.openai_endpoint_override.trim(),
    api_key: form.api_key.trim(),
    extra_headers: headers,
    timeout_seconds: form.timeout_seconds,
    allow_internal: form.allow_internal,
  }
}

/** 导出文件名里的时间戳（本地时区，便于区分多次导出）。 */
function fileStamp(): string {
  const now = new Date()
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}`
}

export function ProvidersPage() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const fileInputRef = useRef<HTMLInputElement>(null)
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
      setDialogOpen(false)
      toast.show(
        variables.id
          ? `已保存供应商「${saved.name || saved.id}」`
          : `已创建「${saved.name || saved.id}」${saved.model_count === 0 ? '：还没有启用模型，可在编辑页拉取并勾选' : ''}`,
        'success',
      )
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

  // 拉取模型：已保存的供应商写候选缓存；未保存的走草稿接口，不落库
  const fetchModels = useMutation({
    mutationFn: async (state: FormState) => {
      if (state.id) {
        const updated = await api.fetchModels(state.id)
        return { candidates: updated.candidate_models ?? [], saved: true }
      }
      const draft = await api.fetchDraftModels(toDraftInput(state))
      return { candidates: draft.candidate_models ?? [], saved: false }
    },
    onSuccess: async ({ candidates, saved }) => {
      setForm((prev) => ({
        ...prev,
        modelOrder: mergeModels(prev.modelOrder, candidates),
        // 首次拉取：把候选全部勾上，用户再按需取消
        selected:
          Object.keys(prev.selected).length === 0
            ? Object.fromEntries(candidates.map((m) => [m, true]))
            : prev.selected,
      }))
      if (saved) {
        await invalidateAll()
      }
      toast.show(
        saved
          ? `已拉取 ${candidates.length} 个候选模型，请勾选要启用的模型`
          : `已拉取 ${candidates.length} 个候选模型（尚未保存，勾选后点「保存」写入）`,
        'success',
      )
    },
    onError: (err: Error) => toast.show(`拉取模型失败：${err.message}`, 'error'),
  })

  const exportProviders = useMutation({
    mutationFn: api.exportProviders,
    onSuccess: (payload) => {
      const blob = new Blob([JSON.stringify(payload, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = `agoramodel-providers-${fileStamp()}.json`
      document.body.appendChild(link)
      link.click()
      link.remove()
      URL.revokeObjectURL(url)
      toast.show(
        `已导出 ${payload.providers.length} 个供应商；文件含 API Key 明文，请按凭证妥善保管`,
        'success',
      )
    },
    onError: (err: Error) => toast.show(`导出失败：${err.message}`, 'error'),
  })

  const importProviders = useMutation({
    mutationFn: async (file: File) => {
      const text = await file.text()
      let parsed: unknown
      try {
        parsed = JSON.parse(text)
      } catch {
        throw new Error('文件不是合法 JSON')
      }
      const list = (parsed as { providers?: unknown } | null)?.providers
      if (!Array.isArray(list) || list.length === 0) {
        throw new Error('文件里没有 providers 数组（请使用本页导出的文件）')
      }
      return api.importProviders({ providers: list as ProviderInput[] })
    },
    onSuccess: async (result) => {
      await invalidateAll()
      toast.show(`已导入 ${result.imported} 个供应商`, 'success')
    },
    onError: (err: Error) => toast.show(`导入失败：${err.message}`, 'error'),
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

  /** 拉取模型与保存是两个独立动作：先做与后端一致的本地校验。 */
  const startFetchModels = () => {
    setFormError('')
    if (form.openai_base_url.trim() === '' && form.openai_endpoint_override.trim() === '') {
      setFormError('请先填写 OpenAI Base URL，再拉取模型')
      return
    }
    if (!form.id && form.api_key.trim() === '') {
      setFormError('请先填写 API Key，再拉取模型')
      return
    }
    fetchModels.mutate(form)
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
        <div className="flex items-center gap-2">
          <input
            ref={fileInputRef}
            type="file"
            accept="application/json,.json"
            className="hidden"
            onChange={(event) => {
              const file = event.target.files?.[0]
              event.target.value = ''
              if (file) importProviders.mutate(file)
            }}
          />
          <Button
            variant="outline"
            onClick={() => fileInputRef.current?.click()}
            disabled={importProviders.isPending}
          >
            <Upload className="h-4 w-4" />
            {importProviders.isPending ? '导入中…' : '导入'}
          </Button>
          <Button
            variant="outline"
            onClick={() => {
              if (
                window.confirm(
                  '导出文件包含所有供应商的 API Key 明文，请自行妥善保管。确认导出？',
                )
              ) {
                exportProviders.mutate()
              }
            }}
            disabled={exportProviders.isPending}
          >
            <Download className="h-4 w-4" />
            {exportProviders.isPending ? '导出中…' : '导出'}
          </Button>
          <Button onClick={openCreate}>
            <Plus className="h-4 w-4" />
            新增供应商
          </Button>
        </div>
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
                  <TableHead>官网</TableHead>
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
                    <TableCell className="max-w-[240px] truncate font-mono text-xs">
                      {provider.openai_base_url || <span className="text-slate-400">未配置</span>}
                    </TableCell>
                    <TableCell className="max-w-[180px] truncate text-xs">
                      {provider.website_url ? (
                        <a
                          href={provider.website_url}
                          target="_blank"
                          rel="noreferrer"
                          className="text-sky-700 underline"
                          title={provider.website_url}
                        >
                          {provider.website_url}
                        </a>
                      ) : (
                        <span className="text-slate-400">—</span>
                      )}
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
        description="上游只需提供 OpenAI 兼容接口；模型可在保存前先拉取勾选，保存后立即对外生效。"
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
            <p className="text-xs text-slate-500">
              名称即模型命名空间前缀（<code className="rounded bg-slate-100 px-1">{`${form.name || '名称'}/模型名`}</code>），需唯一。
            </p>
          </div>

          <div className="grid gap-1.5">
            <Label htmlFor="p-website">供应商官网地址</Label>
            <Input
              id="p-website"
              value={form.website_url}
              onChange={(event) => setForm({ ...form, website_url: event.target.value })}
              placeholder="https://example.com"
            />
            <p className="text-xs text-slate-500">
              用于记录供应商后台/定价页地址，仅在列表页展示为可点击链接，不参与转发。
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
                  onClick={startFetchModels}
                  disabled={fetchModels.isPending}
                  title="按上方填写的 Base URL 与 API Key 拉取上游 /models"
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
              <p className="text-xs text-amber-700">
                无需先保存：填好 Base URL 与 API Key 即可点「拉取模型」，勾选后再点底部「保存」写入。
              </p>
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
