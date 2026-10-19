import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { BarChart3, CalendarRange } from 'lucide-react'
import { api } from '@/lib/api'
import type { UsageDailyRow } from '@/lib/types'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
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

/** 把每日行按日期索引，并按区间补齐缺失日期（total=0）。 */
function buildSeries(
  daily: UsageDailyRow[],
  range: DateRange | null,
): { date: string; total: number }[] {
  if (!range) return []
  const byDate = new Map(daily.map((d) => [d.date, d]))
  return enumerateDates(range.from, range.to).map((date) => ({
    date,
    total: byDate.get(date)?.total ?? 0,
  }))
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

/* ------------------------------------------------------------------ 组件：热力图 */

/**
 * 活跃热力图（GitHub 风格贡献图）：
 * - 数据范围：今天往前推半年（约 26 周）
 * - 横轴：每个 ISO 周（周一为首）一列，列数 = 半年内 ISO 周数
 * - 纵轴：周一~周日共 7 行
 * - 每格固定 w-4 h-4；超出窗口的格子显示浅灰占位 + 不显示 tooltip
 * - 顶部月份标签横跨该月首个 ISO 周所在的列
 */
function HeatmapByMonth({ daily, endDate }: { daily: { date: string; total: number }[]; endDate: string }) {
  // 把 daily 索引到 date 上
  const dayMap = new Map<string, number>()
  for (const row of daily) {
    dayMap.set(row.date, row.total)
  }

  const max = Math.max(1, ...daily.map((c) => c.total))
  const logMax = Math.log2(max + 1)
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
                const total = dayMap.get(dateISO)
                if (total === undefined) {
                  return (
                    <div key={dIdx} className="aspect-square w-full rounded-sm bg-slate-50" />
                  )
                }
                if (total === 0) {
                  return (
                    <div
                      key={dIdx}
                      title={`${dateISO} · 0`}
                      className="aspect-square w-full rounded-sm border border-slate-100"
                      style={{ backgroundColor: 'rgba(16, 185, 129, 0.05)' }}
                    />
                  )
                }
                const alpha = Math.max(0.15, Math.log2(total + 1) / logMax)
                return (
                  <div
                    key={dIdx}
                    title={`${dateISO} · ${formatToken(total)}`}
                    className="aspect-square w-full rounded-sm border border-slate-100"
                    style={{ backgroundColor: `rgba(16, 185, 129, ${alpha})` }}
                  />
                )
              })}
            </div>
          ))}
        </div>
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

function TrendBars({ trend }: { trend: { date: string; total: number }[] }) {
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
              title={`${t.date} · ${formatToken(t.total)}`}
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

  const usage = useQuery({
    queryKey: ['usage'],
    queryFn: () => api.usage({ granularity: 'daily' }),
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

  // 模型占比：按窗口 daily 的总 token 等比缩放基线数字，
  // 这样累计窗口看到的是基线全量，30 天窗口看到的是基线 × (30 天 / 全量)。
  const models = useMemo(() => {
    if (!data) return []
    const baselineTotal = data.models.reduce((s, m) => s + m.total, 0)
    const windowTotal = buildSeries(data.daily, modelsRange).reduce((s, d) => s + d.total, 0)
    if (baselineTotal === 0 || windowTotal === 0) return []
    const ratio = windowTotal / baselineTotal
    return data.models.map((m) => ({
      ...m,
      total: Math.round(m.total * ratio),
    }))
  }, [data, modelsRange])

  if (usage.isLoading || !data) {
    return <p className="text-sm text-slate-500">加载中…</p>
  }

  return (
    <div className="flex flex-col gap-5">
      {/* 页面顶部 */}
      <div>
        <h1 className="flex items-center gap-2 text-xl font-semibold text-slate-900">
          <BarChart3 className="h-5 w-5" />
          使用统计
        </h1>
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
                <CardDescription>汇总占比中的 token 总数</CardDescription>
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
                daily={data.daily.map((d) => ({ date: d.date, total: d.total }))}
                endDate={anchor}
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
          <TrendBars trend={trend} />
        </CardContent>
      </Card>
      </div>
    </div>
  )
}
