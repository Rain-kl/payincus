export interface TrafficHistoryItem {
  date: string
  rxTotal: string
  txTotal: string
  rxFormatted: string
  txFormatted: string
  total: string
  totalFormatted: string
}

export function fillContinuousDays(
  rawItems: TrafficHistoryItem[],
  daysOrRange: number | { start: string; end: string }
): TrafficHistoryItem[] {
  const itemMap = new Map<string, TrafficHistoryItem>()
  for (const item of rawItems) {
    itemMap.set(item.date, item)
  }

  const result: TrafficHistoryItem[] = []
  if (typeof daysOrRange === 'number') {
    const days = daysOrRange
    const now = new Date()
    for (let i = days - 1; i >= 0; i--) {
      const d = new Date(now.getTime() - i * 24 * 60 * 60 * 1000)
      const year = d.getFullYear()
      const month = String(d.getMonth() + 1).padStart(2, '0')
      const day = String(d.getDate()).padStart(2, '0')
      const dateStr = `${year}-${month}-${day}`
      const existing = itemMap.get(dateStr)
      result.push(existing || {
        date: dateStr,
        rxTotal: '0',
        txTotal: '0',
        rxFormatted: '0 B',
        txFormatted: '0 B',
        total: '0',
        totalFormatted: '0 B'
      })
    }
  } else {
    const { start, end } = daysOrRange
    if (!start) return rawItems
    const startDate = new Date(start)
    const endDate = end ? new Date(end) : new Date()
    const now = new Date()
    const effectiveEnd = endDate.getTime() > now.getTime() ? now : endDate
    const cur = new Date(startDate)
    while (cur <= effectiveEnd) {
      const year = cur.getFullYear()
      const month = String(cur.getMonth() + 1).padStart(2, '0')
      const day = String(cur.getDate()).padStart(2, '0')
      const dateStr = `${year}-${month}-${day}`
      const existing = itemMap.get(dateStr)
      result.push(existing || {
        date: dateStr,
        rxTotal: '0',
        txTotal: '0',
        rxFormatted: '0 B',
        txFormatted: '0 B',
        total: '0',
        totalFormatted: '0 B'
      })
      cur.setDate(cur.getDate() + 1)
    }
    if (result.length === 0) return rawItems
  }
  return result
}
