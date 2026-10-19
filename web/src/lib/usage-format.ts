/** 把数字格式化为带单位的可读字符串（1234567 → "1.23M"）。 */
export function formatToken(n: number): string {
  if (n === 0) return '0'
  if (n < 0) return `-${formatToken(-n)}`
  if (n < 1000) return String(Math.round(n))
  if (n < 10_000) return `${(n / 1000).toFixed(2)}K`
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}K`
  if (n < 10_000_000) return `${(n / 1_000_000).toFixed(2)}M`
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  return `${(n / 1_000_000_000).toFixed(2)}B`
}

/** 仅用于 KPI 大字：超过 100 万时保留两位小数 + M。 */
export function formatBigToken(n: number): string {
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}K`
  return `${(n / 1_000_000).toFixed(1)}M`
}

/** 把 YYYY-MM-DD 渲染成"10月19日"形式。 */
export function formatShortDate(date: string): string {
  if (!date || date.length < 10) return date
  const [y, m, d] = date.slice(0, 10).split('-')
  void y
  return `${Number(m)}月${Number(d)}日`
}
