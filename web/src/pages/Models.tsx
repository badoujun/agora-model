import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, RefreshCw, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import { useToast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

export function ModelsPage() {
  const toast = useToast()
  const queryClient = useQueryClient()
  const [showNamespaced, setShowNamespaced] = useState(false)
  const [filter, setFilter] = useState('')
  const [newProvider, setNewProvider] = useState('')
  const [newModel, setNewModel] = useState('')

  const models = useQuery({ queryKey: ['models'], queryFn: api.listModels })
  const providers = useQuery({ queryKey: ['providers'], queryFn: api.listProviders })

  const providerNames = useMemo(() => {
    const map = new Map<string, string>()
    for (const item of providers.data?.items ?? []) {
      map.set(item.id, item.name || item.id)
    }
    return map
  }, [providers.data])

  const invalidate = async () => {
    await queryClient.invalidateQueries({ queryKey: ['models'] })
    await queryClient.invalidateQueries({ queryKey: ['providers'] })
  }

  const refresh = useMutation({
    mutationFn: api.refreshModels,
    onSuccess: async () => {
      await invalidate()
      toast.show('已触发全量模型刷新', 'success')
    },
    onError: (err: Error) => toast.show(`刷新失败：${err.message}`, 'error'),
  })

  const addManual = useMutation({
    mutationFn: () => api.addManualModel(newProvider, newModel.trim()),
    onSuccess: async () => {
      setNewModel('')
      await invalidate()
      toast.show('已添加手动模型', 'success')
    },
    onError: (err: Error) => toast.show(`添加失败：${err.message}`, 'error'),
  })

  const removeManual = useMutation({
    mutationFn: (payload: { providerId: string; model: string }) =>
      api.removeManualModel(payload.providerId, payload.model),
    onSuccess: async () => {
      await invalidate()
      toast.show('已移除手动模型', 'success')
    },
    onError: (err: Error) => toast.show(`移除失败：${err.message}`, 'error'),
  })

  const items = models.data?.items ?? []
  const providerOptions = providers.data?.items ?? []

  const rows = useMemo(() => {
    const keyword = filter.trim().toLowerCase()
    const matched = items.filter((item) => !keyword || item.model.toLowerCase().includes(keyword))

    if (showNamespaced) {
      return matched.map((item) => ({ ...item, display: `${item.provider_id}/${item.model}` }))
    }

    // 裸名视图：同一模型只显示一行，优先默认供应商（priority 最小者）
    const seen = new Set<string>()
    const out: Array<(typeof matched)[number] & { display: string }> = []
    for (const item of matched) {
      if (item.default && !seen.has(item.model)) {
        seen.add(item.model)
        out.push({ ...item, display: item.model })
      }
    }
    for (const item of matched) {
      if (!seen.has(item.model)) {
        seen.add(item.model)
        out.push({ ...item, display: item.model })
      }
    }
    return out
  }, [items, filter, showNamespaced])

  const bareCount = useMemo(() => new Set(items.map((item) => item.model)).size, [items])

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">模型列表</h1>
        <p className="mt-1 text-sm text-slate-500">
          聚合自所有启用供应商的可用模型：(手动 ∪ 自动拉取) − 排除列表。请求时可直接使用裸模型名，
          或用 <code className="rounded bg-slate-100 px-1">provider/model</code> 强制指定供应商。
        </p>
      </div>

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-3 space-y-0">
          <div>
            <CardTitle>聚合结果</CardTitle>
            <CardDescription>
              共 {bareCount} 个模型 ID、{items.length} 条供应商映射
              {models.dataUpdatedAt ? ` · 更新于 ${new Date(models.dataUpdatedAt).toLocaleTimeString('zh-CN')}` : ''}
            </CardDescription>
          </div>
          <div className="flex items-center gap-3">
            <div className="flex items-center gap-2">
              <Switch checked={showNamespaced} onCheckedChange={setShowNamespaced} id="ns" />
              <Label htmlFor="ns">显示 provider/model</Label>
            </div>
            <Input
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
              placeholder="搜索模型名"
              className="w-48"
            />
            <Button variant="outline" onClick={() => refresh.mutate()} disabled={refresh.isPending}>
              <RefreshCw className={refresh.isPending ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />
              全量刷新
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {models.isLoading ? (
            <p className="px-5 py-8 text-sm text-slate-500">加载中…</p>
          ) : rows.length === 0 ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">
              <p>还没有可用模型。</p>
              <p className="mt-1">
                请先到「供应商管理」添加供应商并点击「拉取模型」，或在此手动添加一个模型。
              </p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>模型</TableHead>
                  <TableHead>供应商</TableHead>
                  <TableHead>默认路由</TableHead>
                  <TableHead>来源</TableHead>
                  <TableHead className="text-right">操作</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={`${row.provider_id}/${row.model}`}>
                    <TableCell className="font-mono text-xs">{row.display}</TableCell>
                    <TableCell>{providerNames.get(row.provider_id) ?? row.provider_id}</TableCell>
                    <TableCell>
                      {row.default ? (
                        <Badge variant="success">默认</Badge>
                      ) : (
                        <span className="text-xs text-slate-400">—</span>
                      )}
                    </TableCell>
                    <TableCell>
                      {row.source === 'manual' ? (
                        <Badge variant="info">手动</Badge>
                      ) : (
                        <Badge>自动</Badge>
                      )}
                    </TableCell>
                    <TableCell className="text-right">
                      {row.source === 'manual' ? (
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => removeManual.mutate({ providerId: row.provider_id, model: row.model })}
                        >
                          <Trash2 className="h-4 w-4" />
                          移除
                        </Button>
                      ) : (
                        <span
                          className="text-xs text-slate-400"
                          title="自动拉取的模型由上游客商维护，如需排除请在供应商的「排除列表」中配置"
                        >
                          由上游客商维护
                        </span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>手动添加模型</CardTitle>
          <CardDescription>
            用于上游客商没有提供 <code className="rounded bg-slate-100 px-1">/models</code> 接口，
            或需要声明上游未列出的模型名时。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-end gap-3">
          <div className="flex w-56 flex-col gap-1.5">
            <Label htmlFor="manual-provider">供应商</Label>
            <Select
              id="manual-provider"
              value={newProvider}
              onChange={(event) => setNewProvider(event.target.value)}
            >
              <option value="">请选择…</option>
              {providerOptions.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.name || item.id}
                </option>
              ))}
            </Select>
          </div>
          <div className="flex w-72 flex-col gap-1.5">
            <Label htmlFor="manual-model">模型名</Label>
            <Input
              id="manual-model"
              value={newModel}
              onChange={(event) => setNewModel(event.target.value)}
              placeholder="例如 gpt-4o"
            />
          </div>
          <Button
            onClick={() => addManual.mutate()}
            disabled={addManual.isPending || !newProvider || newModel.trim() === ''}
          >
            <Plus className="h-4 w-4" />
            添加
          </Button>
        </CardContent>
      </Card>
    </div>
  )
}
