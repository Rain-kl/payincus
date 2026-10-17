export function storageHeadroomMB(totalMB: number): number {
  return Math.min(10240, Math.max(1024, Math.ceil(totalMB / 100)))
}

/** Logical capacity which is promised but not yet physically occupied. */
export function unconsumedStorageMB(
  rows: Array<{ incusId: string; disk: number }>,
  volumeUsedMB: Map<string, number>,
  physicallyUsedMB: number
): number {
  const allocated = rows.reduce((sum, i) => sum + i.disk, 0)
  const credited = rows.reduce((sum, i) => sum + Math.min(i.disk, volumeUsedMB.get(i.incusId) || 0), 0)
  return Math.max(0, allocated - Math.min(credited, physicallyUsedMB))
}
