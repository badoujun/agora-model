import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Check, Copy, KeyRound, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import { copyText } from '@/lib/clipboard'
import type { GatewayKeyDTO, GatewayKeyScope, ModelItem } from '@/lib/types'
import { formatTime } from '@/lib/utils'
import { formatToken } from '@/lib/usage-format'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { useToast } from '@/components/ui/toast'

/** 模型范围内的一条：供应商 id + 上游模型名（与后端存储口径一致）。 */
function scopeKey(providerID: string, upstreamModel: string): string {
  return `${providerID}\u0000${upstreamModel}`
}

/** 把范围数组转成 Set，便于勾选判断。 */
function toScopeMap(scopes: GatewayKeyScope[]): Set<string> {
  return new Set(scopes.map((item) => scopeKey(item.provider_id, item.model)))
}

/**
 * 模型范围选择器：按供应商分组勾选「对外可用的模型」。
 *
 * 提交时映射回上游模型名（后端存的是上游名，别名改动不会让范围失效）。
 * 不勾选任何项 = 不限制，可调用全部模型——这是默认值，也是升级后的既有行为。
 */
function ScopePicker({
  models,
  selected,
  onChange,
}: {
  models: ModelItem[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
}) {
  const [keyword, setKeyword] = useState('')

  // provider_id -> 供应商名 -> 模型列表（同一供应商的模型按后端返回顺序）
  const groups = useMemo(() => {
    const byProvider = new Map<string, { name: string; models: ModelItem[] }>()
    for (const item of models) {
      let group = byProvider.get(item.provider_id)
      if (!group) {
        group = { name: item.provider_name || item.provider_id, models: [] }
        byProvider.set(item.provider_id, group)
      }
      group.models.push(item)
    }
    return [...byProvider.entries()]
  }, [models])

  const kw = keyword.trim().toLowerCase()
  const toggle = (key: string) => {
    const next = new Set(selected)
    if (next.has(key)) next.delete(key)
    else next.add(key)
    onChange(next)
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <Input
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
          placeholder="搜索模型名"
          className="h-8 flex-1"
        />
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => {
            const next = new Set(selected)
            for (const [, group] of groups) {
              for (const item of group.models) {
                if (kw && !item.model.toLowerCase().includes(kw)) continue
                next.add(scopeKey(item.provider_id, item.upstream_model))
              }
            }
            onChange(next)
          }}
        >
          全选
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={() => onChange(new Set())}>
          清空
        </Button>
      </div>

      <p className="text-xs text-slate-500">
        {selected.size === 0
          ? '未勾选任何模型 = 不限制，该 Key 可调用全部模型。'
          : `已勾选 ${selected.size} 个模型；未勾选的模型对该 Key 返回 403。`}
      </p>

      <div className="max-h-64 overflow-y-auto rounded-md border border-slate-200">
        {groups.length === 0 ? (
          <p className="px-3 py-4 text-xs text-slate-400">
            还没有启用的模型，请先在「供应商管理」里勾选模型。
          </p>
        ) : (
          groups.map(([providerID, group]) => {
            const visible = kw
              ? group.models.filter((m) => m.model.toLowerCase().includes(kw))
              : group.models
            if (visible.length === 0) return null
            return (
              <div key={providerID} className="border-b border-slate-100 last:border-b-0">
                <p className="bg-slate-50 px-3 py-1.5 text-xs font-medium text-slate-600">
                  {group.name}
                </p>
                <div className="grid gap-1 px-3 py-2 sm:grid-cols-2">
                  {visible.map((item) => {
                    const key = scopeKey(item.provider_id, item.upstream_model)
                    return (
                      <label
                        key={key}
                        className="flex cursor-pointer items-center gap-2 text-xs text-slate-700"
                      >
                        <input
                          type="checkbox"
                          className="h-3.5 w-3.5 rounded border-slate-300"
                          checked={selected.has(key)}
                          onChange={() => toggle(key)}
                        />
                        <span className="truncate font-mono">{item.model}</span>
                        {item.alias ? (
                          <span className="truncate text-[11px] text-slate-400">
                            ({item.upstream_model})
                          </span>
                        ) : null}
                      </label>
                    )
                  })}
                </div>
              </div>
            )
          })
        )}
      </div>
    </div>
  )
}

/** 把勾选集合转回提交用的范围数组。 */
function scopeMapToArray(selected: Set<string>): GatewayKeyScope[] {
  const out: GatewayKeyScope[] = []
  for (const key of selected) {
    const idx = key.indexOf('\u0000')
    if (idx <= 0) continue
    out.push({ provider_id: key.slice(0, idx), model: key.slice(idx + 1) })
  }
  return out
}

/** 范围摘要：不限制 / N 个模型。 */
function scopeSummary(key: GatewayKeyDTO): string {
  if (!key.scopes || key.scopes.length === 0) return '不限制'
  const providers = new Set(key.scopes.map((s) => s.provider_id))
  return `${key.scopes.length} 个模型 · ${providers.size} 个供应商`
}

/** 行内展示的 4 项累计用量（全部历史）。 */
function usageChips(key: GatewayKeyDTO): { label: string; value: number }[] {
  return [
    { label: '总 token', value: key.usage?.total_tokens ?? 0 },
    { label: '输入(命中缓存)', value: key.usage?.input_cached ?? 0 },
    { label: '输入(未命中缓存)', value: key.usage?.input_uncached ?? 0 },
    { label: '输出', value: key.usage?.output ?? 0 },
  ]
}

/**
 * 网关 API Key 管理卡片。
 *
 * 支持同时启用多个 Key，每个 Key 可设置别名、备注（用途/使用人）、启停、
 * 软删除与模型范围。明文可按需查看/复制，重置只换明文、不动其他配置。
 */
export function GatewayKeysCard({
  selectedKeyId,
  onSelectKey,
}: {
  selectedKeyId: string
  onSelectKey: (id: string) => void
}) {
  const toast = useToast()
  const queryClient = useQueryClient()

  const [includeDeleted, setIncludeDeleted] = useState(false)
  const [copiedId, setCopiedId] = useState<string | null>(null)
  /** id -> 已取到的明文（仅本页内存，刷新即失效） */
  const [plainById, setPlainById] = useState<Record<string, string>>({})

  const keys = useQuery({
    queryKey: ['gateway-keys', { includeDeleted }],
    queryFn: () => api.listGatewayKeys({ includeDeleted }),
  })
  const models = useQuery({ queryKey: ['models'], queryFn: api.listModels })

  const [dialogOpen, setDialogOpen] = useState(false)
  /** null 表示新建；否则是正在编辑的 Key */
  const [editing, setEditing] = useState<GatewayKeyDTO | null>(null)
  const [form, setForm] = useState({ name: '', note: '', enabled: true })
  const [formScopes, setFormScopes] = useState<Set<string>>(new Set())
  const [formError, setFormError] = useState('')

  const items = keys.data?.items ?? []
  const activeItems = items.filter((item) => !item.deleted)

  const invalidateKeys = async () => {
    await queryClient.invalidateQueries({ queryKey: ['gateway-keys'] })
    await queryClient.invalidateQueries({ queryKey: ['settings'] })
  }

  const openCreate = () => {
    setEditing(null)
    setForm({ name: '', note: '', enabled: true })
    setFormScopes(new Set())
    setFormError('')
    setDialogOpen(true)
  }

  const openEdit = (key: GatewayKeyDTO) => {
    setEditing(key)
    setForm({ name: key.name, note: key.note ?? '', enabled: key.enabled })
    setFormScopes(toScopeMap(key.scopes ?? []))
    setFormError('')
    setDialogOpen(true)
  }

  const saveKey = useMutation({
    mutationFn: async () => {
      const payload = {
        name: form.name.trim(),
        note: form.note.trim(),
        enabled: form.enabled,
        scopes: scopeMapToArray(formScopes),
      }
      if (editing) return api.updateGatewayKey(editing.id, payload)
      return api.createGatewayKey(payload)
    },
    onSuccess: async (result) => {
      await invalidateKeys()
      const created = 'gateway_key' in result ? result : null
      if (created?.gateway_key) {
        setPlainById((prev) => ({ ...prev, [created.key!.id]: created.gateway_key }))
        onSelectKey(created.key!.id)
        toast.show('已创建网关 Key：明文已填入「接入地址」片段', 'success')
      } else {
        toast.show('已保存', 'success')
      }
      setDialogOpen(false)
    },
    onError: (err: Error) => setFormError(err.message),
  })

  const toggleEnabled = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api.updateGatewayKey(id, { enabled }),
    onSuccess: async () => {
      await invalidateKeys()
    },
    onError: (err: Error) => toast.show(`切换失败：${err.message}`, 'error'),
  })

  const removeKey = useMutation({
    mutationFn: (id: string) => api.deleteGatewayKey(id),
    onSuccess: async () => {
      await invalidateKeys()
      toast.show('已删除该网关 Key：立即失效，历史日志仍可显示其名称', 'success')
    },
    onError: (err: Error) => toast.show(`删除失败：${err.message}`, 'error'),
  })

  const resetKey = useMutation({
    mutationFn: (id: string) => api.resetGatewayKeyById(id),
    onSuccess: async (data, id) => {
      setPlainById((prev) => ({ ...prev, [id]: data.gateway_key }))
      await invalidateKeys()
      toast.show('已重置该 Key 的明文：旧明文立即失效，其他配置保持不变', 'success')
    },
    onError: (err: Error) => toast.show(`重置失败：${err.message}`, 'error'),
  })

  const copy = async (id: string, text: string) => {
    if (!(await copyText(text))) {
      toast.show('复制失败，请手动选中文本复制', 'error')
      return
    }
    setCopiedId(id)
    window.setTimeout(() => setCopiedId(null), 1500)
  }

  /** 查看明文：已取过则直接显示，否则按需请求（旧记录只存哈希时 409）。 */
  const reveal = async (key: GatewayKeyDTO) => {
    if (plainById[key.id]) return
    try {
      const data = await api.revealGatewayKey(key.id)
      setPlainById((prev) => ({ ...prev, [key.id]: data.gateway_key }))
    } catch (err) {
      toast.show(`无法查看明文：${(err as Error).message}`, 'error')
    }
  }

  const canSubmit = form.name.trim() !== ''

  return (
    <Card>
      <CardHeader className="flex-row flex-wrap items-start justify-between gap-3 space-y-0">
        <div>
          <CardTitle className="flex items-center gap-2">
            <KeyRound className="h-4 w-4" />
            网关 API Key
          </CardTitle>
          <CardDescription>
            可同时启用多个 Key，分别发给不同的 Agent / 使用人；每个 Key 可限定能调用哪些模型。
            明文以 AES 加密保存在本机数据库，仅同源控制台可查看。
          </CardDescription>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex items-center gap-1.5">
            <Switch
              id="show-deleted-keys"
              checked={includeDeleted}
              onCheckedChange={setIncludeDeleted}
            />
            <Label htmlFor="show-deleted-keys" className="text-xs text-slate-500">
              显示已删除
            </Label>
          </div>
          <Button size="sm" onClick={openCreate}>
            <Plus className="h-4 w-4" />
            新建 Key
          </Button>
        </div>
      </CardHeader>

      <CardContent className="flex flex-col gap-2">
        {keys.isLoading ? (
          <p className="py-6 text-sm text-slate-500">加载中…</p>
        ) : items.length === 0 ? (
          <p className="py-6 text-center text-sm text-slate-500">还没有网关 Key，点「新建 Key」创建。</p>
        ) : (
          items.map((key) => {
            const plain = plainById[key.id]
            const isSelected = key.id === selectedKeyId
            return (
              <div
                key={key.id}
                className={`flex flex-col gap-2 rounded-md border px-3 py-2.5 ${
                  isSelected ? 'border-slate-400 bg-slate-50' : 'border-slate-200'
                }`}
              >
                <div className="flex flex-wrap items-center gap-2">
                  <Switch
                    checked={key.enabled}
                    disabled={key.deleted}
                    onCheckedChange={(checked) => toggleEnabled.mutate({ id: key.id, enabled: checked })}
                  />
                  <span className="text-sm font-medium text-slate-900">{key.name}</span>
                  {key.note ? <span className="text-xs text-slate-500">· {key.note}</span> : null}
                  {key.deleted ? (
                    <Badge variant="danger">已删除</Badge>
                  ) : key.enabled ? (
                    <Badge variant="success">启用中</Badge>
                  ) : (
                    <Badge variant="warning">已停用</Badge>
                  )}
                  <Badge variant="info">{scopeSummary(key)}</Badge>
                  <span className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-slate-500">
                    {usageChips(key).map((chip) => (
                      <span
                        key={chip.label}
                        title={`${chip.label}：${chip.value.toLocaleString()} tokens（全部历史累计）`}
                      >
                        {chip.label}{' '}
                        <span className="font-medium tabular-nums text-slate-700">{formatToken(chip.value)}</span>
                      </span>
                    ))}
                  </span>
                  <div className="ml-auto flex items-center gap-1.5">
                    {!key.deleted ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => onSelectKey(key.id)}
                        title="把该 Key 用于下方接入片段"
                      >
                        {isSelected ? <Check className="h-4 w-4" /> : null}
                        {isSelected ? '接入中' : '用于接入'}
                      </Button>
                    ) : null}
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => void reveal(key)}
                      disabled={!key.revealable && !plain}
                      title={key.revealable ? '查看并复制明文' : '明文不可还原，请重置'}
                    >
                      {copiedId === key.id ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                      查看/复制
                    </Button>
                    {!key.deleted ? (
                      <>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => openEdit(key)}
                          title="修改别名、备注与模型范围"
                        >
                          编辑
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={resetKey.isPending}
                          onClick={() => {
                            if (
                              window.confirm(
                                `重置「${key.name}」的明文？旧明文会立即失效，需要更新使用该 Key 的 Agent。`,
                              )
                            ) {
                              resetKey.mutate(key.id)
                            }
                          }}
                          title="只换明文，保留别名/备注/状态/模型范围"
                        >
                          <RefreshCw className="h-4 w-4" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={removeKey.isPending}
                          onClick={() => {
                            if (
                              window.confirm(
                                `删除「${key.name}」？该 Key 会立即失效；记录会保留以便历史日志与统计显示名称。`,
                              )
                            ) {
                              removeKey.mutate(key.id)
                            }
                          }}
                          title="软删除（立即失效，历史仍可读）"
                        >
                          <Trash2 className="h-4 w-4 text-red-500" />
                        </Button>
                      </>
                    ) : null}
                  </div>
                </div>

                <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-slate-500">
                  <span className="font-mono">{key.key_hint || '—'}</span>
                  <span>创建于 {formatTime(key.created_at)}</span>
                  <span>最近使用 {formatTime(key.last_used_at)}</span>
                  {key.deleted ? <span>删除于 {formatTime(key.deleted_at)}</span> : null}
                </div>

                {plain ? (
                  <div className="flex items-center gap-2">
                    <code className="min-w-0 flex-1 break-all rounded bg-slate-100 px-2 py-1 font-mono text-xs">
                      {plain}
                    </code>
                    <Button variant="outline" size="sm" onClick={() => void copy(key.id, plain)}>
                      {copiedId === key.id ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                      复制
                    </Button>
                  </div>
                ) : null}

                {!key.deleted && !key.revealable && !plain ? (
                  <p className="rounded-md border border-amber-200 bg-amber-50 px-2 py-1.5 text-[11px] text-amber-900">
                    该 Key 的明文无法还原（旧版本只存了哈希，或主密钥已更换）。点右侧「重置」即可生成可复制的新明文。
                  </p>
                ) : null}
              </div>
            )
          })
        )}

        {activeItems.length === 0 && items.length > 0 ? (
          <p className="text-xs text-amber-700">当前没有任何启用的 Key，所有 Agent 都会收到 401。</p>
        ) : null}
      </CardContent>

      <Dialog
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        title={editing ? `编辑网关 Key「${editing.name}」` : '新建网关 Key'}
        description="别名用于在日志与统计里辨认来源；备注可写用途或使用人。"
        className="max-w-3xl"
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialogOpen(false)}>
              取消
            </Button>
            <Button
              onClick={() => {
                if (!canSubmit) {
                  setFormError('请填写别名')
                  return
                }
                saveKey.mutate()
              }}
              disabled={saveKey.isPending}
            >
              {saveKey.isPending ? '保存中…' : editing ? '保存' : '创建'}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="grid gap-1.5">
              <Label htmlFor="gk-name">别名（必填）</Label>
              <Input
                id="gk-name"
                value={form.name}
                onChange={(event) => setForm((prev) => ({ ...prev, name: event.target.value }))}
                placeholder="例如：Claude Code / Cursor"
                maxLength={64}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="gk-note">备注（用途 / 使用人）</Label>
              <Input
                id="gk-note"
                value={form.note}
                onChange={(event) => setForm((prev) => ({ ...prev, note: event.target.value }))}
                placeholder="例如：张三的笔记本，仅用于代码补全"
                maxLength={256}
              />
            </div>
          </div>

          <div className="flex items-center gap-2">
            <Switch
              id="gk-enabled"
              checked={form.enabled}
              onCheckedChange={(checked) => setForm((prev) => ({ ...prev, enabled: checked }))}
            />
            <div>
              <Label htmlFor="gk-enabled">创建后立即启用</Label>
              <p className="text-xs text-slate-500">
                停用的 Key 请求会收到 401；不能停用最后一个启用的 Key。
              </p>
            </div>
          </div>

          <div className="grid gap-1.5">
            <Label>模型范围</Label>
            <ScopePicker
              models={models.data?.items ?? []}
              selected={formScopes}
              onChange={setFormScopes}
            />
          </div>

          {formError ? (
            <p className="rounded-md border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-700">
              {formError}
            </p>
          ) : null}
        </div>
      </Dialog>
    </Card>
  )
}
