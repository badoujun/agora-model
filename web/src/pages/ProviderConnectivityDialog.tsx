import { useMemo, useState } from 'react'
import { Loader2, Send } from 'lucide-react'
import type { ConnectivityTestResult } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Select } from '@/components/ui/select'
import { cn } from '@/lib/utils'

/** 连通性测试的默认测试消息（与后端默认文案保持一致）。 */
export const DEFAULT_CONNECTIVITY_MESSAGE = '你好，请回复“连接成功”。'

/** 模型到展示名的映射：配置了别名时显示「别名（上游名）」。 */
export function modelLabel(model: string, aliases: Record<string, string>): string {
  const alias = (aliases[model] ?? '').trim()
  return alias === '' ? model : `${alias}（${model}）`
}

/** 把上游原始响应体格式化成便于阅读的 JSON；不是合法 JSON 时原样返回。 */
function prettyJSON(raw: string): string {
  const text = raw.trim()
  if (text === '') return ''
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return raw
  }
}

export interface ConnectivityDialogProps {
  open: boolean
  onClose: () => void
  /** 供应商展示名（标题副文案用） */
  providerName: string
  /** 被测试的 Key 掩码（已保存用库内掩码，未保存用明文掩码） */
  keyHint: string
  /** 已勾选的上游模型名（顺序即展示顺序） */
  models: string[]
  /** 上游模型名 -> 对外别名 */
  aliases: Record<string, string>
  /** 发起一次真实调用；由调用方决定走已保存接口还是草稿接口 */
  onSend: (input: { model: string; message: string }) => Promise<ConnectivityTestResult>
}

/**
 * 单 Key 连通性测试弹窗。
 *
 * 与「测试」（只探测 /models）的区别：这里会真的发起一次 chat/completions，
 * 产生实打实的 token 消耗，因此顶部有明确的计费提示，右侧展示上游原始响应。
 */
export function ConnectivityDialog({
  open,
  onClose,
  providerName,
  keyHint,
  models,
  aliases,
  onSend,
}: ConnectivityDialogProps) {
  const [model, setModel] = useState(() => models[0] ?? '')
  const [message, setMessage] = useState(DEFAULT_CONNECTIVITY_MESSAGE)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<ConnectivityTestResult | null>(null)

  const debugText = useMemo(() => {
    if (!result) return ''
    return prettyJSON(result.response_raw ?? '')
  }, [result])

  const send = async () => {
    setError('')
    if (model.trim() === '') {
      setError('请先选择要测试的模型（需先在供应商配置里勾选）')
      return
    }
    setPending(true)
    try {
      const payload = await onSend({ model, message })
      setResult(payload)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="API Key 连通性测试"
      description={providerName}
      className="max-w-5xl"
    >
      <div className="grid gap-4">
        {/* 计费提示：让用户明确知道这会花钱 */}
        <div className="flex items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-900">
          <span aria-hidden className="mt-px">⚠️</span>
          <span>
            本次测试将以该 API Key 发起一次真实模型调用，会产生实际计费/配额消耗（约数 tokens）。
            测试结果与模型实际返回一致，请注意用量。
          </span>
        </div>

        <div className="grid gap-4 md:grid-cols-[minmax(0,340px)_minmax(0,1fr)]">
          {/* 左栏：调用参数 */}
          <div className="flex flex-col gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="conn-key">API Key</Label>
              <input
                id="conn-key"
                readOnly
                value={keyHint || '（未填写）'}
                className="h-9 w-full rounded-md border border-slate-200 bg-slate-50 px-2.5 font-mono text-sm text-slate-600"
              />
            </div>

            <div className="grid gap-1.5">
              <Label htmlFor="conn-protocol">调用协议</Label>
              <Select id="conn-protocol" value="openai" disabled>
                <option value="openai">OpenAI Chat Completions</option>
              </Select>
            </div>

            <div className="grid gap-1.5">
              <Label htmlFor="conn-model">选择模型</Label>
              <Select
                id="conn-model"
                value={model}
                onChange={(event) => setModel(event.target.value)}
                disabled={models.length === 0}
                title={models.length === 0 ? '该供应商还没有勾选任何模型' : undefined}
              >
                {models.length === 0 ? <option value="">（没有已勾选的模型）</option> : null}
                {models.map((item) => (
                  <option key={item} value={item}>
                    {modelLabel(item, aliases)}
                  </option>
                ))}
              </Select>
              {models.length === 0 ? (
                <p className="text-[11px] text-amber-700">
                  该供应商还没有勾选模型：请先在编辑页拉取并勾选模型后再测。
                </p>
              ) : null}
            </div>

            <div className="grid gap-1.5">
              <Label htmlFor="conn-message">测试消息</Label>
              <textarea
                id="conn-message"
                rows={6}
                value={message}
                onChange={(event) => setMessage(event.target.value)}
                className={cn(
                  'w-full resize-y rounded-md border border-slate-300 px-3 py-2 text-sm shadow-sm',
                  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-slate-400',
                )}
                placeholder={DEFAULT_CONNECTIVITY_MESSAGE}
              />
            </div>

            {error ? <p className="text-xs text-red-600">{error}</p> : null}

            <Button
              variant="destructive"
              className="mt-auto w-full"
              onClick={() => void send()}
              disabled={pending || models.length === 0}
            >
              {pending ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
              {pending ? '请求中…' : '发送请求'}
            </Button>
          </div>

          {/* 右栏：调试信息 */}
          <div className="flex min-w-0 flex-col gap-1.5">
            <div className="flex items-center justify-between">
              <Label>调试信息</Label>
              {result ? (
                <div className="flex items-center gap-2 text-[11px] text-slate-500">
                  <span className={result.ok ? 'text-emerald-700' : 'text-red-600'}>
                    {result.ok ? '成功' : '失败'}
                  </span>
                  {result.status_code ? <span>HTTP {result.status_code}</span> : null}
                  <span>{result.latency_ms} ms</span>
                  {result.usage ? (
                    <span>
                      tokens {result.usage.input_tokens + result.usage.output_tokens}
                    </span>
                  ) : null}
                </div>
              ) : null}
            </div>
            <pre className="h-[420px] overflow-auto rounded-md border border-slate-800 bg-slate-900 p-3 text-[11px] leading-relaxed text-slate-100">
              {debugText
                ? debugText
                : result
                  ? result.message || '（上游没有返回响应体）'
                  : '尚未发送请求：点击左下角「发送请求」后，这里显示上游原始响应。'}
            </pre>
            {result?.response_truncated ? (
              <p className="text-[11px] text-amber-700">响应体较大，已截断显示（日志中记录完整状态码与耗时）。</p>
            ) : null}
            {result ? (
              <p className="break-all text-[11px] text-slate-400">
                request_id={result.request_id} · model={result.model}
                {result.usage
                  ? ` · 输入 ${result.usage.input_tokens}（缓存 ${result.usage.cached_input_tokens}）/ 输出 ${result.usage.output_tokens}`
                  : ''}
              </p>
            ) : null}
          </div>
        </div>
      </div>
    </Dialog>
  )
}
