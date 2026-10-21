import { useCallback, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3, CalendarRange } from 'lucide-react'
import { api } from '@/lib/api'
import type { UsageDailyRow, UsageModelShare } from '@/lib/types'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Select } from '@/components/ui/select'
import { cn } from '@/lib/utils'
import { formatToken } from '@/lib/usage-format'

/* ------------------------------------------------------------------ 类型 */

/** 模块级时间窗口：预设（按天数相对今天）或自定义（from/to 绝对日期）。 */
type WindowPreset = 'today' | '7d' | '30d' | 'all' | 'custom'

interface WindowState {
  preset: WindowPreset
  /** 自定义起止日期（YYYY-MM-DD）；preset === 'custom' 时使用。 */
  from?: string
  to?: string
}

/* ------------------------------------------------------------------ 工具 */

/** 日期区间（YYYY-MM-DD，闭区间）。 */
interface DateRange {
  from: string
  to: string
}

/** 服务端统计按 UTC 日期聚合，这里用同样的口径取"今天"。 */
function todayISO(): string {
  return new Date().toISOString().slice(0, 10)
}

function addDays(date: string, days: number): string {
  const d = new Date(`${date}T00:00:00Z`)
  d.setUTCDate(d.getUTCDate() + days)
  return d.toISOString().slice(0, 10)
}

/** 生成 [from, to] 内的全部日期（含首尾）；非法区间返回空数组。 */
function enumerateDates(from: string, to: string): string[] {
  const start = new Date(`${from}T00:00:00Z`)
  const end = new Date(`${to}T00:00:00Z`)
  if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime()) || start > end) return []
  const out: string[] = []
  for (const cursor = new Date(start); cursor <= end; cursor.setUTCDate(cursor.getUTCDate() + 1)) {
    out.push(cursor.toISOString().slice(0, 10))
  }
  return out
}

/**
 * 把 WindowState 解析成绝对日期区间。
 * - 相对窗口按"今天"反推，与数据里是否有这一天无关（空日子也要出点位）
 * - all 从最早有数据的一天开始
 */
function resolveWindow(window: WindowState, anchor: string, earliest: string): DateRange | null {
  if (window.preset === 'custom') {
    if (!window.from || !window.to || window.from > window.to) return null
    return { from: window.from, to: window.to }
  }
  if (window.preset === 'today') return { from: anchor, to: anchor }
  if (window.preset === 'all') return { from: earliest <= anchor ? earliest : anchor, to: anchor }
  const days = window.preset === '7d' ? 7 : 30
  return { from: addDays(anchor, -(days - 1)), to: anchor }
}

/**
 * 把每日行按日期索引，并按区间补齐缺失日期（零值行）。
 * 补齐的行只有 date（breakdown 缺省），悬浮弹窗按"当天无请求"渲染。
 */
function buildSeries(daily: UsageDailyRow[], range: DateRange | null): UsageDailyRow[] {
  if (!range) return []
  const byDate = new Map(daily.map((d) => [d.date, d]))
  return enumerateDates(range.from, range.to).map(
    (date): UsageDailyRow =>
      byDate.get(date) ?? {
        date,
        total: 0,
        input_cached: 0,
        input_uncached: 0,
        output: 0,
        requests: 0,
      },
  )
}

/* ------------------------------------------------------------------ 组件：窗口选择 */

/**
 * 通用窗口选择器：图标按钮 + 下拉。
 * - options：可选预设（不含今日 → excludeToday）
 * - onChange：把外部状态写回（state + setter 模式）
 */
function WindowPicker({
  state,
  onChange,
  excludeToday = false,
}: {
  state: WindowState
  onChange: (next: WindowState) => void
  excludeToday?: boolean
}) {
  const [open, setOpen] = useState(false)
  const allOptions: { value: WindowPreset; label: string }[] = useMemo(
    () => [
      { value: '7d', label: '最近 7 天' },
      { value: '30d', label: '最近 30 天' },
      { value: 'all', label: '累计' },
      { value: 'custom', label: '自定义…' },
    ],
    [],
  )
  const options = excludeToday
    ? allOptions
    : [{ value: 'today' as WindowPreset, label: '今日' }, ...allOptions]

  const currentLabel =
    state.preset === 'custom' && state.from && state.to
      ? `${state.from} → ${state.to}`
      : options.find((o) => o.value === state.preset)?.label ?? ''

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 rounded-md border border-slate-200 bg-white px-2 py-1 text-xs text-slate-600 hover:bg-slate-50"
        title="切换时间窗口"
      >
        <CalendarRange className="h-3.5 w-3.5 text-slate-400" />
        <span>{currentLabel}</span>
      </button>
      {open ? (
        <div
          className="absolute right-0 top-full z-10 mt-1 w-56 rounded-md border border-slate-200 bg-white py-1 shadow-md"
          onMouseLeave={() => setOpen(false)}
        >
          {options.map((opt) => (
            <button
              key={opt.value}
              type="button"
              onClick={() => {
                if (opt.value === 'custom') {
                  // 默认填上最近 30 天
                  const today = todayISO()
                  onChange({
                    preset: 'custom',
                    from: addDays(today, -29),
                    to: today,
                  })
                } else {
                  onChange({ preset: opt.value })
                }
                setOpen(false)
              }}
              className={cn(
                'block w-full px-3 py-1.5 text-left text-xs hover:bg-slate-100',
                opt.value === state.preset ? 'font-medium text-slate-900' : 'text-slate-600',
              )}
            >
              {opt.label}
            </button>
          ))}
          {state.preset === 'custom' ? (
            <div className="border-t border-slate-100 px-3 py-2">
              <div className="mb-1.5 flex items-center gap-2 text-[11px] text-slate-500">
                <label className="flex flex-1 items-center gap-1">
                  从
                  <input
                    type="date"
                    max={state.to}
                    value={state.from ?? ''}
                    onChange={(e) =>
                      onChange({ ...state, from: e.target.value || undefined })
                    }
                    className="flex-1 rounded border border-slate-200 px-1 py-0.5 text-xs"
                  />
                </label>
                <label className="flex flex-1 items-center gap-1">
                  到
                  <input
                    type="date"
                    min={state.from}
                    max={todayISO()}
                    value={state.to ?? ''}
                    onChange={(e) =>
                      onChange({ ...state, to: e.target.value || undefined })
                    }
                    className="flex-1 rounded border border-slate-200 px-1 py-0.5 text-xs"
                  />
                </label>
              </div>
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

/* ------------------------------------------------------------------ 组件：悬浮弹窗 */

/** 悬浮弹窗的锚点（客户区坐标）与该日数据。 */
interface HoverState {
  x: number
  y: number
  row: UsageDailyRow
}

const TOOLTIP_WIDTH = 432

/**
 * 热力图 / 趋势图共用的悬浮弹窗：
 * - 顶部：日期 + 当天 token 合计
 * - 表格：供应商/模型、输入(命中缓存)、输入(未命中缓存)、输出
 * - 明细按当天总量降序（服务端已排序），底部给一行合计
 *
 * 用 fixed + 鼠标坐标定位；靠近视口底部时向上翻转，右侧空间不足时左移。
 * pointer-events-none 保证弹窗不会挡住鼠标、引发闪烁。
 */
function UsageTooltip({ hover }: { hover: HoverState | null }) {
  if (!hover) return null
  const { row } = hover
  const flipUp = hover.y > window.innerHeight * 0.6
  const left = Math.max(8, Math.min(hover.x + 14, window.innerWidth - TOOLTIP_WIDTH - 8))
  const style = flipUp
    ? { left, top: hover.y - 14, transform: 'translateY(-100%)' }
    : { left, top: hover.y + 14 }

  const items = row.breakdown ?? []

  return (
    <div
      className="pointer-events-none fixed z-50 max-h-[70vh] overflow-hidden rounded-md border border-slate-200 bg-white px-2.5 py-2 text-slate-700 shadow-lg"
      style={{ width: TOOLTIP_WIDTH, ...style }}
    >
      <div className="mb-1.5 flex items-baseline justify-between gap-2 border-b border-slate-100 pb-1.5">
        <span className="text-xs font-medium text-slate-900">{row.date}</span>
        <span className="text-[11px] text-slate-500 tabular-nums">
          合计 {formatToken(row.total)}
        </span>
      </div>
      {items.length === 0 ? (
        <p className="text-[11px] text-slate-400">当天无请求</p>
      ) : (
        <div className="max-h-[calc(70vh-3rem)] overflow-y-auto">
          <table className="w-full table-fixed text-[11px]">
            <colgroup>
              <col />
              <col className="w-[5.2rem]" />
              <col className="w-[5.8rem]" />
              <col className="w-[3.6rem]" />
            </colgroup>
            <thead>
              <tr className="whitespace-nowrap text-[10px] leading-tight text-slate-400">
                <th className="pb-1 text-left font-normal">供应商 / 模型</th>
                <th className="pb-1 pl-1 text-right font-normal">输入(命中缓存)</th>
                <th className="pb-1 pl-1 text-right font-normal">输入(未命中缓存)</th>
                <th className="pb-1 pl-1 text-right font-normal">输出</th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={`${item.provider}/${item.model}`} className="align-top">
                  <td className="py-0.5 pr-1">
                    <div className="break-all text-[10px] text-slate-400">{item.provider}</div>
                    <div className="break-all font-mono text-[11px] text-slate-700">
                      {item.model}
                    </div>
                  </td>
                  <td className="py-0.5 pl-1 text-right tabular-nums">
                    {formatToken(item.input_cached)}
                  </td>
                  <td className="py-0.5 pl-1 text-right tabular-nums">
                    {formatToken(item.input_uncached)}
                  </td>
                  <td className="py-0.5 pl-1 text-right tabular-nums">
                    {formatToken(item.output)}
                  </td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr className="border-t border-slate-100 font-medium text-slate-900">
                <td className="pt-1 text-[10px] text-slate-500">合计</td>
                <td className="pt-1 pl-1 text-right tabular-nums">
                  {formatToken(row.input_cached)}
                </td>
                <td className="pt-1 pl-1 text-right tabular-nums">
                  {formatToken(row.input_uncached)}
                </td>
                <td className="pt-1 pl-1 text-right tabular-nums">{formatToken(row.output)}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      )}
    </div>
  )
}

/* ------------------------------------------------------------------ 组件：热力图 */

/**
 * 热力图色阶：下标 = 档位，0 为"无用量"，1~5 对应 less → more。
 * 分级按当日消耗的总 token 数：0–5M / 5–10M / 10–50M / 50–100M / >100M。
 */
const HEATMAP_COLORS = ['#ebedf0', '#c6f6d5', '#9ae6b4', '#68d391', '#38a169', '#276749']
const HEATMAP_HINTS = ['无用量', '≤ 5M', '≤ 10M', '≤ 50M', '≤ 100M', '> 100M']

/** 当日总 token → 热力图档位（0 表示无用量，1~5 依次对应 less → more）。 */
function heatLevel(total: number): number {
  if (total <= 0) return 0
  if (total <= 5_000_000) return 1
  if (total <= 10_000_000) return 2
  if (total <= 50_000_000) return 3
  if (total <= 100_000_000) return 4
  return 5
}

/**
 * 活跃热力图（GitHub 风格贡献图）：
 * - 数据范围：今天往前推半年（约 26 周）
 * - 横轴：每个 ISO 周（周一为首）一列，列数 = 半年内 ISO 周数
 * - 纵轴：周一~周日共 7 行
 * - 每格固定 w-4 h-4；超出窗口的格子显示浅灰占位 + 不显示 tooltip
 * - 顶部月份标签横跨该月首个 ISO 周所在的列
 * - 颜色分 5 档（less → more），按当日消耗的 token 总数落入上面的分级区间
 * - 悬浮显示明细弹窗（供应商/模型 + 输入命中缓存/未命中/输出）
 */
function HeatmapByMonth({
  daily,
  endDate,
  onHover,
  onLeave,
}: {
  daily: UsageDailyRow[]
  endDate: string
  onHover: (hover: HoverState) => void
  onLeave: () => void
}) {
  // 把 daily 索引到 date 上
  const dayMap = new Map<string, UsageDailyRow>()
  for (const row of daily) {
    dayMap.set(row.date, row)
  }

  const weekdayLabels = ['一', '二', '三', '四', '五', '六', '日']

  // 数据范围：今天往前半年（≈ 26 周 = 182 天）
  const endDateObj = new Date(`${endDate}T00:00:00Z`)
  const HALF_YEAR_DAYS = 182
  const startDateObj = new Date(endDateObj)
  startDateObj.setUTCDate(startDateObj.getUTCDate() - (HALF_YEAR_DAYS - 1))

  // 起点 ISO 周一
  const startIsoWeekday = ((startDateObj.getUTCDay() + 6) % 7) + 1 // 1..7
  const startMonday = new Date(startDateObj)
  startMonday.setUTCDate(startMonday.getUTCDate() - (startIsoWeekday - 1))
  // 截止 ISO 周一（endDate 所在的 ISO 周）
  const endIsoWeekday = ((endDateObj.getUTCDay() + 6) % 7) + 1
  const endMonday = new Date(endDateObj)
  endMonday.setUTCDate(endMonday.getUTCDate() - (endIsoWeekday - 1))

  // 收集从 startMonday 到 endMonday 之间的所有 ISO 周
  interface WeekColumn {
    mondayISO: string
    days: string[]
    monthLabel?: string
  }
  const weeks: WeekColumn[] = []
  let lastMonth = -1
  const cursor = new Date(startMonday)
  while (cursor.getTime() <= endMonday.getTime()) {
    const mondayISO = cursor.toISOString().slice(0, 10)
    const days: string[] = []
    for (let i = 0; i < 7; i++) {
      const d = new Date(cursor)
      d.setUTCDate(d.getUTCDate() + i)
      days.push(d.toISOString().slice(0, 10))
    }
    const month = cursor.getUTCMonth()
    const weekColumn: WeekColumn = { mondayISO, days }
    if (month !== lastMonth) {
      weekColumn.monthLabel = `${month + 1}月`
      lastMonth = month
    }
    weeks.push(weekColumn)
    cursor.setUTCDate(cursor.getUTCDate() + 7)
  }

  return (
    <div className="flex w-full flex-col gap-1">
      {/* 横轴：月份标签 */}
      <div className="flex w-full gap-1 pl-6">
        {weeks.map((w) => (
          <div
            key={w.mondayISO}
            className="flex min-w-0 flex-1 flex-col items-center text-[10px] leading-none"
          >
            <span className={w.monthLabel ? 'text-slate-500' : 'text-transparent'}>
              {w.monthLabel ?? '·'}
            </span>
          </div>
        ))}
      </div>
      {/* 主体：左纵轴（周一~周日 7 行）+ N 列（每 ISO 周一列）× 7 行 网格 */}
      <div className="flex w-full gap-2">
        <div className="flex shrink-0 flex-col gap-1">
          {weekdayLabels.map((w) => (
            <div
              key={w}
              className="flex min-h-[1rem] flex-1 items-center justify-end pr-1 text-[10px] text-slate-400"
            >
              {w}
            </div>
          ))}
        </div>
        <div className="flex min-w-0 flex-1 gap-1">
          {weeks.map((w) => (
            <div key={w.mondayISO} className="flex min-w-0 flex-1 flex-col gap-1">
              {w.days.map((dateISO, dIdx) => {
                const row = dayMap.get(dateISO)
                if (row === undefined) {
                  return (
                    <div key={dIdx} className="aspect-square w-full rounded-sm bg-slate-50" />
                  )
                }
                const level = heatLevel(row.total)
                return (
                  <div
                    key={dIdx}
                    onMouseEnter={(e) => onHover({ x: e.clientX, y: e.clientY, row })}
                    onMouseLeave={onLeave}
                    className="aspect-square w-full rounded-sm border border-slate-100"
                    style={{ backgroundColor: HEATMAP_COLORS[level] }}
                  />
                )
              })}
            </div>
          ))}
        </div>
      </div>
      {/* 图例：less → more 五档 */}
      <div className="flex items-center justify-end gap-1 pt-0.5 text-[10px] text-slate-400">
        <span>少</span>
        {HEATMAP_COLORS.slice(1).map((color, i) => (
          <span
            key={color}
            className="h-2.5 w-2.5 rounded-sm border border-slate-100"
            style={{ backgroundColor: color }}
            title={HEATMAP_HINTS[i + 1]}
          />
        ))}
        <span>多</span>
      </div>
    </div>
  )
}

/* ------------------------------------------------------------------ 组件：趋势柱状 */

/** 横轴最多显示的刻度数，避免日期标签互相重叠。 */
const AXIS_MAX_LABELS = 8

/** 横轴刻度：M/D（完整日期放在柱子的 tooltip 里）。 */
function formatAxisDate(date: string): string {
  const [, m, d] = date.split('-')
  return `${Number(m)}/${Number(d)}`
}

function TrendBars({
  rows,
  onHover,
  onLeave,
}: {
  rows: UsageDailyRow[]
  onHover: (hover: HoverState) => void
  onLeave: () => void
}) {
  const trend = rows
  if (trend.length === 0) {
    return <p className="text-xs text-slate-400">所选窗口内暂无数据。</p>
  }
  const max = Math.max(1, ...trend.map((t) => t.total))
  // 柱子数量随窗口变化；刻度按"不超过 AXIS_MAX_LABELS 个"的均匀间隔抽取，
  // 且始终显示最后一天（今天的点位不能丢）。
  const step = Math.max(1, Math.ceil(trend.length / AXIS_MAX_LABELS))
  const lastIdx = trend.length - 1
  // 累计窗口可能有上百个点位，间距收窄，避免柱子被 gap 挤出容器
  const gapClass = trend.length > 60 ? 'gap-px' : 'gap-1'
  return (
    <div className="flex h-full min-h-[8rem] flex-col">
      <div className={cn('flex min-h-0 flex-1 items-end', gapClass)}>
        {trend.map((t) => {
          const pct = t.total === 0 ? 4 : Math.max(8, (t.total / max) * 100)
          const isPeak = t.total === max && t.total > 0
          return (
            <div
              key={t.date}
              onMouseEnter={(e) => onHover({ x: e.clientX, y: e.clientY, row: t })}
              onMouseLeave={onLeave}
              className="min-w-0 flex-1 rounded-t"
              style={{
                height: `${pct}%`,
                backgroundColor: isPeak ? '#f59e0b' : '#10b981',
                opacity: t.total === 0 ? 0.2 : 0.85,
              }}
            />
          )
        })}
      </div>
      {/* 横轴：与柱子一一对应的日期刻度（0 值的日期同样保留点位） */}
      <div className={cn('mt-1 flex border-t border-slate-100 pt-1', gapClass)}>
        {trend.map((t, i) => (
          <div
            key={t.date}
            className="min-w-0 flex-1 whitespace-nowrap text-center text-[10px] leading-none text-slate-400"
          >
            {i === lastIdx || (lastIdx - i) % step === 0 ? formatAxisDate(t.date) : ''}
          </div>
        ))}
      </div>
    </div>
  )
}

/* ------------------------------------------------------------------ 主页面 */

export function UsagePage() {
  // 每个模块自己管窗口状态，互不影响
  const [modelsWindow, setModelsWindow] = useState<WindowState>({ preset: '30d' })
  const [trendWindow, setTrendWindow] = useState<WindowState>({ preset: '30d' })
  // 统计范围：'' = 全部网关 Key，否则只统计该 Key
  const [gatewayKeyId, setGatewayKeyId] = useState('')
  // 热力图 / 趋势图共用的悬浮弹窗（悬停时写坐标与当天数据，移出清空）
  const [hover, setHover] = useState<HoverState | null>(null)
  const clearHover = useCallback(() => setHover(null), [])

  // 全量网关 Key（含已删除）：历史用量可能属于已删除的 Key，也要能查
  const gatewayKeys = useQuery({
    queryKey: ['gateway-keys', { includeDeleted: true }],
    queryFn: () => api.listGatewayKeys({ includeDeleted: true }),
  })

  const usage = useQuery({
    queryKey: ['usage', { gatewayKeyId }],
    queryFn: () =>
      api.usage({
        granularity: 'daily',
        gateway_key_id: gatewayKeyId || undefined,
      }),
  })

  const data = usage.data

  // 窗口锚点：今天（空日子也要出点位），以及数据中最早的一天（累计窗口起点）
  const anchor = todayISO()
  const earliest = data?.daily[0]?.date ?? anchor

  const modelsRange = useMemo(
    () => (data ? resolveWindow(modelsWindow, anchor, earliest) : null),
    [data, modelsWindow, anchor, earliest],
  )
  const trendRange = useMemo(
    () => (data ? resolveWindow(trendWindow, anchor, earliest) : null),
    [data, trendWindow, anchor, earliest],
  )

  // 趋势：按所选窗口逐日展开，没有请求的日期补 0（仍占据 x 轴点位）
  const trend = useMemo(() => (data ? buildSeries(data.daily, trendRange) : []), [data, trendRange])

  // 模型占比：只统计所选窗口内的真实用量 —— 把窗口内每天明细里的
  // (供应商, 模型) 逐条累加（token = 输入 + 输出），占比按窗口内总量计算。
  // 不再拿全量基线做等比缩放，避免出现"看起来是真实数字"的估算值。
  const { models, windowTotal } = useMemo(() => {
    if (!data || !modelsRange) return { models: [] as UsageModelShare[], windowTotal: 0 }

    const acc = new Map<string, UsageModelShare>()
    let total = 0

    for (const row of data.daily) {
      if (row.date < modelsRange.from || row.date > modelsRange.to) continue
      for (const item of row.breakdown ?? []) {
        const tokens = item.input_cached + item.input_uncached + item.output
        if (tokens === 0) continue
        const key = `${item.provider}\u0000${item.model}`
        const cur =
          acc.get(key) ??
          ({
            name: item.model,
            provider: item.provider,
            total: 0,
            percent: 0,
            input_cached: 0,
            input_uncached: 0,
            output: 0,
            requests: 0,
          } satisfies UsageModelShare)
        cur.total += tokens
        cur.input_cached += item.input_cached
        cur.input_uncached += item.input_uncached
        cur.output += item.output
        acc.set(key, cur)
        total += tokens
      }
    }

    const list = [...acc.values()].sort((a, b) => b.total - a.total)
    for (const m of list) {
      m.percent = total > 0 ? (m.total / total) * 100 : 0
    }
    return { models: list, windowTotal: total }
  }, [data, modelsRange])

  if (usage.isLoading || !data) {
    return <p className="text-sm text-slate-500">加载中…</p>
  }

  return (
    <div className="flex flex-col gap-5">
      {/* 页面顶部 */}
      <div className="flex flex-wrap items-end justify-between gap-3">
        <h1 className="flex items-center gap-2 text-xl font-semibold text-slate-900">
          <BarChart3 className="h-5 w-5" />
          使用统计
        </h1>
        <div className="grid gap-1.5">
          <Label htmlFor="usage-gateway-key">网关 API Key</Label>
          <Select
            id="usage-gateway-key"
            className="w-64"
            value={gatewayKeyId}
            onChange={(event) => setGatewayKeyId(event.target.value)}
          >
            <option value="">全部网关 Key</option>
            {(gatewayKeys.data?.items ?? []).map((key) => (
              <option key={key.id} value={key.id}>
                {key.name}
                {key.note ? `（${key.note}）` : ''}
                {key.deleted ? ' · 已删除' : key.enabled ? '' : ' · 已停用'}
              </option>
            ))}
          </Select>
          <p className="text-[11px] text-slate-500">
            选择单个 Key 后，模型占比、活跃热力图、使用趋势与 KPI 都只统计它。
          </p>
        </div>
      </div>

      {/* 上下两部分各占 1/2 高度（外层用 grid 强制均分） */}
      <div className="grid min-h-[calc(100vh-7rem)] grid-rows-2 gap-5">
        {/* 上半部分：左 1/3 模型使用占比 / 右 2/3 活跃热力图 */}
        <div className="flex min-h-0 flex-col gap-5 xl:flex-row">
        <div className="flex flex-col gap-5 xl:w-1/3">
          {/* 模型使用占比 */}
          <Card className="flex min-h-0 flex-1 flex-col overflow-hidden">
            <CardHeader className="flex-row items-center justify-between space-y-0">
              <div>
                <CardTitle>模型使用占比</CardTitle>
                <CardDescription>
                  {models.length === 0
                    ? '所选窗口内暂无 token 用量'
                    : `窗口内 token 总数 ${formatToken(windowTotal)}`}
                </CardDescription>
              </div>
              <WindowPicker state={modelsWindow} onChange={setModelsWindow} />
            </CardHeader>
            <CardContent className="flex flex-col gap-3">
              {models.length === 0 ? (
                <p className="text-xs text-slate-400">所选窗口内暂无数据。</p>
              ) : (
                models.map((m) => (
                  <div key={m.name} className="flex flex-col gap-1">
                    <div className="flex items-center justify-between text-xs">
                      <span className="flex items-center gap-2 text-slate-700">
                        <span className="text-slate-400">{m.provider}</span>
                        <span className="text-slate-300">·</span>
                        <span className="font-mono">{m.name}</span>
                      </span>
                      <span className="font-medium text-slate-900 tabular-nums">
                        {m.percent.toFixed(1)}%
                      </span>
                    </div>
                    <div className="h-2 overflow-hidden rounded bg-slate-100">
                      <div
                        className="h-full rounded bg-emerald-500"
                        style={{ width: `${m.percent}%` }}
                      />
                    </div>
                    <div className="text-right text-[11px] text-slate-500 tabular-nums">
                      {formatToken(m.total)}
                    </div>
                  </div>
                ))
              )}
            </CardContent>
          </Card>
        </div>

        {/* 上半部分右列：活跃热力图 */}
        <div className="flex flex-col gap-5 xl:w-2/3">
          {/* 活跃热力图 */}
          <Card className="flex min-h-0 flex-1 flex-col overflow-hidden">
            <CardHeader>
              <CardTitle>活跃热力图</CardTitle>
            </CardHeader>
            <CardContent className="min-h-0 flex-1">
              <HeatmapByMonth
                daily={data.daily}
                endDate={anchor}
                onHover={setHover}
                onLeave={clearHover}
              />
            </CardContent>
          </Card>
        </div>
        </div>

      {/* 下半部分：使用趋势（独占整行） */}
      <Card className="flex min-h-0 flex-col overflow-hidden">
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle>使用趋势</CardTitle>
            <CardDescription>橙色为窗口期内峰值</CardDescription>
          </div>
          <WindowPicker
            state={trendWindow}
            onChange={setTrendWindow}
            excludeToday
          />
        </CardHeader>
        <CardContent className="min-h-0 flex-1">
          <TrendBars rows={trend} onHover={setHover} onLeave={clearHover} />
        </CardContent>
      </Card>
      </div>

      {/* 悬浮弹窗：渲染在最外层，避免被 Card 的 overflow-hidden 裁掉 */}
      <UsageTooltip hover={hover} />
    </div>
  )
}
