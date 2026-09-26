import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { RefreshCw } from 'lucide-react'
import { api } from '@/lib/api'
import type { LogItem } from '@/lib/types'
import { formatTime } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
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

function statusBadge(status: number) {
  if (status === 499) return <Badge variant="warning">499 客户端断开</Badge>
  if (status >= 500) return <Badge variant="danger">{status}</Badge>
  if (status >= 400) return <Badge variant="warning">{status}</Badge>
  if (status >= 200) return <Badge variant="success">{status}</Badge>
  return <Badge>{status}</Badge>
}

export function LogsPage() {
  const [failedOnly, setFailedOnly] = useState(true)
  const [model, setModel] = useState('')
  const [providerId, setProviderId] = useState('')
  const [limit, setLimit] = useState(50)
  const [offset, setOffset] = useState(0)
  const [autoRefresh, setAutoRefresh] = useState(true)
  const [expanded, setExpanded] = useState<string | null>(null)

  const providers = useQuery({ queryKey: ['providers'], queryFn: api.listProviders })

  const logs = useQuery({
    queryKey: ['logs', { failedOnly, model, providerId, limit, offset }],
    queryFn: () =>
      api.listLogs({
        failed: failedOnly ? 'true' : undefined,
        model: model.trim() || undefined,
        provider_id: providerId || undefined,
        limit,
        offset,
      }),
    refetchInterval: autoRefresh ? 5000 : false,
  })

  const items: LogItem[] = logs.data?.items ?? []
  const total = logs.data?.total ?? 0

  return (
    <div className="flex flex-col gap-5">
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-xl font-semibold text-slate-900">请求日志</h1>
          <p className="mt-1 text-sm text-slate-500">
            用于判断「是 Agent 发错了，还是上游拒了」。失败请求始终记录；错误信息已脱敏。
          </p>
        </div>
        <Button variant="outline" onClick={() => logs.refetch()} disabled={logs.isFetching}>
          <RefreshCw className={logs.isFetching ? 'h-4 w-4 animate-spin' : 'h-4 w-4'} />
          刷新
        </Button>
      </div>

      <Card>
        <CardHeader className="flex-row flex-wrap items-end justify-between gap-3 space-y-0">
          <div className="flex flex-wrap items-end gap-3">
            <div className="flex items-center gap-2 pb-1">
              <Switch id="failed-only" checked={failedOnly} onCheckedChange={setFailedOnly} />
              <Label htmlFor="failed-only">仅看失败</Label>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="log-model">模型</Label>
              <Input
                id="log-model"
                className="w-44"
                value={model}
                onChange={(event) => {
                  setModel(event.target.value)
                  setOffset(0)
                }}
                placeholder="精确匹配"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="log-provider">供应商</Label>
              <Select
                id="log-provider"
                className="w-44"
                value={providerId}
                onChange={(event) => {
                  setProviderId(event.target.value)
                  setOffset(0)
                }}
              >
                <option value="">全部</option>
                {(providers.data?.items ?? []).map((item) => (
                  <option key={item.id} value={item.id}>
                    {item.name || item.id}
                  </option>
                ))}
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="log-limit">每页</Label>
              <Select
                id="log-limit"
                className="w-24"
                value={String(limit)}
                onChange={(event) => {
                  setLimit(Number(event.target.value))
                  setOffset(0)
                }}
              >
                {[20, 50, 100, 200].map((value) => (
                  <option key={value} value={value}>
                    {value}
                  </option>
                ))}
              </Select>
            </div>
          </div>
          <div className="flex items-center gap-2 pb-1">
            <Switch id="auto-refresh" checked={autoRefresh} onCheckedChange={setAutoRefresh} />
            <Label htmlFor="auto-refresh">每 5 秒自动刷新</Label>
          </div>
        </CardHeader>

        <CardContent className="p-0">
          {logs.isLoading ? (
            <p className="px-5 py-8 text-sm text-slate-500">加载中…</p>
          ) : items.length === 0 ? (
            <p className="px-5 py-10 text-center text-sm text-slate-500">
              {failedOnly ? '暂无失败请求记录 —— 一切正常。' : '暂无日志记录。'}
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>时间</TableHead>
                  <TableHead>协议</TableHead>
                  <TableHead>模型</TableHead>
                  <TableHead>供应商</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>延迟</TableHead>
                  <TableHead>首字节</TableHead>
                  <TableHead>错误摘要</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((item) => (
                  <TableRow
                    key={item.request_id}
                    className="cursor-pointer"
                    onClick={() => setExpanded(expanded === item.request_id ? null : item.request_id)}
                  >
                    <TableCell className="whitespace-nowrap text-xs">{formatTime(item.ts)}</TableCell>
                    <TableCell className="text-xs">{item.inbound_protocol}</TableCell>
                    <TableCell className="font-mono text-xs">{item.model || '—'}</TableCell>
                    <TableCell className="text-xs">{item.provider_id || '—'}</TableCell>
                    <TableCell>{statusBadge(item.status_code)}</TableCell>
                    <TableCell className="text-xs">{item.latency_ms} ms</TableCell>
                    <TableCell className="text-xs">
                      {item.first_byte_ms ? `${item.first_byte_ms} ms` : '—'}
                    </TableCell>
                    <TableCell className="max-w-[420px] text-xs text-slate-600">
                      {expanded === item.request_id ? (
                        <div className="flex flex-col gap-1">
                          <span className="break-all font-mono text-[11px] text-slate-500">
                            {item.upstream_url || '（未到达上游）'}
                          </span>
                          <span className="break-all">{item.error_msg || '（无错误信息）'}</span>
                          <span className="text-[11px] text-slate-400">
                            request_id={item.request_id} · client={item.client_ip} · stream=
                            {String(item.stream)}
                          </span>
                        </div>
                      ) : (
                        <span className="line-clamp-1 break-all">{item.error_msg || '—'}</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
        <CardHeader className="flex-row items-center justify-between space-y-0 border-t border-slate-100">
          <CardDescription>
            共 {total} 条 · 当前显示第 {offset + 1} – {offset + items.length} 条
          </CardDescription>
          <div className="flex items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={offset === 0}
              onClick={() => setOffset(Math.max(0, offset - limit))}
            >
              上一页
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={offset + items.length >= total}
              onClick={() => setOffset(offset + limit)}
            >
              下一页
            </Button>
          </div>
        </CardHeader>
      </Card>
    </div>
  )
}
