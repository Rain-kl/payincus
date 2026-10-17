import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)

const packagesSource = readFileSync(resolve(__dirname, '../src/db/packages.ts'), 'utf8')
const readinessSource = readFileSync(resolve(__dirname, '../src/scripts/verify-production-db-readiness.ts'), 'utf8')

function includes(source: string, pattern: string, label: string): void {
  assert.ok(source.includes(pattern), `Missing ${label}: ${pattern}`)
}

function functionSection(source: string, start: string, end: string): string {
  const startIndex = source.indexOf(start)
  assert.notEqual(startIndex, -1, `Missing section start: ${start}`)
  if (!end) {
    return source.slice(startIndex)
  }
  const endIndex = source.indexOf(end, startIndex + start.length)
  assert.notEqual(endIndex, -1, `Missing section end: ${end}`)
  return source.slice(startIndex, endIndex)
}

const singleSoldOutSection = functionSection(
  packagesSource,
  'export async function checkPackageSoldOut(packageId: number): Promise<boolean> {',
  '/**\n * 批量检查多个套餐的售罄状态'
)

const batchSoldOutSection = functionSection(
  packagesSource,
  'export async function checkPackagesSoldOut(packageIds: number[]): Promise<Map<number, boolean>> {',
  ''
)

for (const [section, label] of [
  [singleSoldOutSection, 'single package sold-out check'],
  [batchSoldOutSection, 'batch package sold-out check']
] as const) {
  includes(section, 'storageSize: true', `${label} must select host storage size`)
  includes(section, 'select: { cpu: true, memory: true, disk: true }', `${label} must select instance disk usage`)
  includes(section, 'disk: true', `${label} must select plan disk requirement`)
  includes(section, 'let minDisk: number', `${label} must compute minimum disk requirement`)
  includes(section, 'minDisk = Math.min(...availablePlans.map(p => p.disk))', `${label} must use available plan disk`)
  includes(section, 'minDisk = 512', `${label} must keep free package disk default aligned with create defaults`)
  includes(section, 'getStorageCapacity(host.id, { packageId, requiredMB: minDisk })', `${label} must check the actual selected storage pool`)
  includes(section, 'const diskAvailable = storage.availableMB', `${label} must use physical capacity minus reservations`)
  assert.ok(!section.includes('(host.storageSize || 0) * 1024 - diskUsed'), 'Initialization size must never be a sales limit')
  includes(section, 'diskAvailable >= minDisk', `${label} must require enough disk before marking in stock`)
}

includes(readinessSource, 'storageSize: true', 'production readiness must select host storage size')
includes(readinessSource, 'select: { cpu: true, memory: true, disk: true }', 'production readiness must select instance disk usage')
includes(readinessSource, 'select: { cpu: true, memory: true, disk: true, isSoldOut: true }', 'production readiness must select plan disk requirement')
includes(readinessSource, 'const minDisk = availablePlans.length > 0 ? Math.min(...availablePlans.map(plan => plan.disk)) : 512', 'production readiness must compute minimum disk requirement')
includes(readinessSource, 'getStorageCapacity(host.id, { packageId: pkg.id, requiredMB: minDisk })', 'production readiness must use live storage pool capacity')
includes(readinessSource, 'minimum CPU/memory/disk requirement', 'production readiness warning must name disk capacity')

console.log('package sold-out capacity guard tests passed')
