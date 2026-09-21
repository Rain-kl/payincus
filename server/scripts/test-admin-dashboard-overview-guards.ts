import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const repoRoot = resolve(__dirname, '../..')

function readRepoFile(path: string): string {
  return readFileSync(resolve(repoRoot, path), 'utf8')
}

// 1. 静态断言：server/src/routes/admin-statistics.ts 必须包含营销与最近实例查询
const statsRouteSource = readRepoFile('server/src/routes/admin-statistics.ts')

assert.ok(
  statsRouteSource.includes('prisma.lottery.findMany') &&
    statsRouteSource.includes('isActive: true'),
  'admin-statistics.ts must query active lotteries'
)

assert.ok(
  statsRouteSource.includes('prisma.dailyCheckin.count'),
  'admin-statistics.ts must count today daily checkins'
)

assert.ok(
  statsRouteSource.includes('prisma.instance.findMany'),
  'admin-statistics.ts must query recent instances'
)

assert.ok(
  statsRouteSource.includes('marketing:') &&
    statsRouteSource.includes('activeLotteries') &&
    statsRouteSource.includes('totalActiveLotteries') &&
    statsRouteSource.includes('checkin:'),
  'admin-statistics.ts response must include marketing object with activeLotteries and checkin'
)

assert.ok(
  statsRouteSource.includes('recentInstances:'),
  'admin-statistics.ts response must include recentInstances'
)

console.log('admin dashboard overview guards passed')
process.exit(0)
