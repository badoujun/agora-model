import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

export function ModelsPage() {
  const [filter, setFilter] = useState('')

  const models = useQuery({ queryKey: ['models'], queryFn: api.listModels })

  const items = models.data?.items ?? []

  const rows = useMemo(() => {
    const keyword = filter.trim().toLowerCase()
    return items.filter(
      (item) =>
        !keyword ||
        item.model.toLowerCase().includes(keyword) ||
        item.upstream_model.toLowerCase().includes(keyword) ||
        item.provider_name.toLowerCase().includes(keyword),
    )
  }, [items, filter])

  const aliasCount = useMemo(() => items.filter((item) => Boolean(item.alias)).length, [items])

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">模型列表</h1>
        <p className="mt-1 text-sm text-slate-500">
          这里列出各供应商在编辑页勾选启用的模型。模型统一以{' '}
          <code className="rounded bg-slate-100 px-1">供应商名/模型名</code> 形式展示和调用；
          配置了别称的模型，别称即对外模型名（网关转发前会换回上游真实模型名）。
        </p>
      </div>

      <Card>
        <CardHeader className="flex-row items-center justify-between gap-3 space-y-0">
          <div>
            <CardTitle>已启用模型</CardTitle>
            <CardDescription>
              共 {items.length} 条供应商 × 模型映射
              {aliasCount > 0 ? ` · ${aliasCount} 条配置了别称` : ''}
              {models.dataUpdatedAt ? ` · 更新于 ${new Date(models.dataUpdatedAt).toLocaleTimeString('zh-CN')}` : ''}
            </CardDescription>
          </div>
          <Input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder="搜索模型或供应商"
            className="w-52"
          />
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
                  <TableHead>模型（供应商名/模型名）</TableHead>
                  <TableHead>上游模型</TableHead>
                  <TableHead>供应商</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => (
                  <TableRow key={row.model}>
                    <TableCell className="font-mono text-xs">
                      {row.model}
                      {row.alias ? (
                        <Badge variant="info" className="ml-2">
                          别称
                        </Badge>
                      ) : null}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-slate-500">{row.upstream_model}</TableCell>
                    <TableCell>{row.provider_name}</TableCell>
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
