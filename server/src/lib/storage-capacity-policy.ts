export function storageHeadroomMB(totalMB: number): number {
  return Math.min(10240, Math.max(1024, Math.ceil(totalMB / 100)))
}

export function storageReportMaxAgeMs(report: unknown): number {
  const value = Number((report as { metrics?: { heartbeatIntervalSeconds?: number } } | null)?.metrics?.heartbeatIntervalSeconds)
  const interval = Number.isFinite(value) && value >= 5 && value <= 3600 ? value : 10
  return Math.max(120000, interval * 3000)
}
