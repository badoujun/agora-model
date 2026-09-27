import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Check, Copy, HardDrive } from 'lucide-react'
import { api } from '@/lib/api'
import { useToast } from '@/components/ui/toast'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

/** 数据位置页面：按当前系统展示本程序在磁盘上产生的数据。 */
export function DataLocationPage() {
  const toast = useToast()
  const [copied, setCopied] = useState<string | null>(null)
  const locations = useQuery({ queryKey: ['data-locations'], queryFn: api.getDataLocations })

  const copy = async (label: string, text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(label)
      window.setTimeout(() => setCopied(null), 1500)
    } catch {
      toast.show('复制失败，请手动选中文本复制', 'error')
    }
  }

  const data = locations.data

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">数据位置</h1>
        <p className="mt-1 text-sm text-slate-500">
          本程序的所有数据都保存在本机，不上传云端；卸载前备份「数据目录」即可完整迁移。
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <HardDrive className="h-4 w-4" />
            运行环境
          </CardTitle>
          <CardDescription>路径来自本程序的真实启动参数，未做任何猜测。</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-2 text-sm">
          {locations.isLoading ? (
            <p className="text-slate-500">加载中…</p>
          ) : locations.isError ? (
            <p className="text-red-600">
              读取失败：{locations.error instanceof Error ? locations.error.message : '未知错误'}
            </p>
          ) : (
            <div className="grid gap-2 sm:grid-cols-2">
              <p>
                <span className="text-slate-500">当前系统：</span>
                <span className="font-medium">
                  {data?.os_label}（{data?.os}）
                </span>
              </p>
              <p>
                <span className="text-slate-500">程序版本：</span>
                <code className="font-mono">v{data?.version}</code>
              </p>
              <p className="sm:col-span-2">
                <span className="text-slate-500">程序可执行文件：</span>
                <code className="break-all font-mono text-xs">{data?.executable || '—'}</code>
              </p>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>产生的数据</CardTitle>
          <CardDescription>
            共 {data?.items.length ?? 0} 项；带「已存在」标记的表示当前磁盘上确实有该文件。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {locations.isLoading ? (
            <p className="text-sm text-slate-500">加载中…</p>
          ) : (
            (data?.items ?? []).map((item) => (
              <div key={item.key} className="rounded-md border border-slate-200 p-3">
                <div className="flex flex-wrap items-start justify-between gap-2">
                  <div className="min-w-0">
                    <p className="text-sm font-medium text-slate-900">{item.label}</p>
                    <p className="mt-0.5 text-xs text-slate-500">{item.note}</p>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={item.kind === 'stdout' || !item.path}
                    onClick={() => copy(item.key, item.path)}
                  >
                    {copied === item.key ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                    复制路径
                  </Button>
                </div>

                {item.kind === 'stdout' ? (
                  <p className="mt-2 rounded bg-slate-50 px-2 py-1 font-mono text-xs text-slate-500">
                    标准输出（无文件路径）
                  </p>
                ) : (
                  <code className="mt-2 block break-all rounded bg-slate-50 px-2 py-1 font-mono text-xs">
                    {item.path}
                  </code>
                )}

                {item.kind === 'stdout' ? null : (
                  <div className="mt-1.5 flex items-center gap-2 text-xs text-slate-500">
                    <Badge variant={item.exists ? 'success' : 'default'}>
                      {item.exists ? '已存在' : '尚未创建'}
                    </Badge>
                    {item.size_label ? <span>占用 {item.size_label}</span> : null}
                  </div>
                )}
              </div>
            ))
          )}
        </CardContent>
      </Card>
    </div>
  )
}
