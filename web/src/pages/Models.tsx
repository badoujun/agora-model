import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
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
  const [showNamespaced, setShowNamespaced] = useState(false)
  const [filter, setFilter] = useState('')

  const models = useQuery({ queryKey: ['models'], queryFn: api.listModels })

  const items = models.data?.items ?? []

  const rows = useMemo(() => {
    const keyword = filter.trim().toLowerCase()
    const matched = items.filter(
      (item) =>
        !keyword ||
        item.model.toLowerCase().includes(keyword) ||
        item.upstream_model.toLowerCase().includes(keyword) ||
        item.provider_name.toLowerCase().includes(keyword),
    )

    if (showNamespaced) {
      return matched.map((item) => ({ ...item, display: `${item.provider_name}/${item.model}` }))
    }

    // 裸名视图：同一对外模型名只显示一行，优先默认供应商（名称排序最靠前者）
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
  const aliasCount = useMemo(() => items.filter((item) => Boolean(item.alias)).length, [items])

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">模型列表</h1>
        <p className="mt-1 text-sm text-slate-500">
          这里列出各供应商在编辑页勾选启用的模型。请求时可直接用裸模型名（同名时按供应商名称升序选择），
          或用 <code className="rounded bg-slate-100 px-1">供应商名/模型名</code> 强制指定供应商；
          配置了别称的模型，别称即对外模型名。
        </p>
      </div>

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-3 space-y-0">
          <div>
            <CardTitle>已启用模型</CardTitle>
            <CardDescription>
              共 {bareCount} 个对外模型名、{items.length} 条供应商映射
              {aliasCount > 0 ? ` · ${aliasCount} 条配置了别称` : ''}
              {models.dataUpdatedAt ? ` · 更新于 ${new Date(models.dataUpdatedAt).toLocaleTimeString('zh-CN')}` : ''}
            </CardDescription>
          </div>
          <div className="flex items-center gap-3">
            <div className="flex items-center gap-2">
              <Switch checked={showNamespaced} onCheckedChange={setShowNamespaced} id="ns" />
              <Label htmlFor="ns">显示 供应商名/模型名</Label>
            </div>
            <Input
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
              placeholder="搜索模型或供应商"
              className="w-52"
            />
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {models.isLoading ? (
            <p className="px-5 py-8 text-sm text-slate-500">加载中…</p>
          ) : rows.length === 0 ? (
            <div className="px-5 py-10 text-center text-sm text-slate-500">
              <p>还没有可用模型。</p>
              <p className="mt-1">
                请到「供应商管理」编辑供应商，点击「拉取模型」并勾选要启用的模型。
              </p>
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>模型</TableHead>
                  <TableHead>上游模型</TableHead>
                  <TableHead>供应商</TableHead>
                  <TableHead>默认路由</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={`${row.provider_id}/${row.model}`}>
                    <TableCell className="font-mono text-xs">
                      {row.display}
                      {row.alias ? (
                        <Badge variant="info" className="ml-2">
                          别称
                        </Badge>
                      ) : null}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-slate-500">{row.upstream_model}</TableCell>
                    <TableCell>{row.provider_name}</TableCell>
                    <TableCell>
                      {row.default ? (
                        <Badge variant="success">默认</Badge>
                      ) : (
                        <span className="text-xs text-slate-400">—</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
