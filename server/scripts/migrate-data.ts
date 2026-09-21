/**
 * PayIncus 数据迁移与数据库初始化入口脚本
 * 
 * 作用：
 * 1. 确保 Prisma 模式迁移已部署至最新版本（执行 prisma migrate deploy）；
 * 2. 检查并补全基础数据（默认管理员、超高配额、系统配置项、默认徽章等）；
 * 3. 规范化历史遗留网络模式与数据结构；
 * 4. 幂等执行，可在服务启动时、Docker 容器初始化时、或日常运维升级时安全重复执行。
 */

import '../src/config/env.js'
import { spawn } from 'node:child_process'
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { initPrismaDatabase } from '../src/db/init-prisma.js'
import { closePrismaDatabase, prisma } from '../src/db/prisma.js'

const __filename = fileURLToPath(import.meta.url)
const __dirname = dirname(__filename)
const serverDir = resolve(__dirname, '..')

function runCommand(command: string, args: string[], cwd: string): Promise<void> {
  return new Promise((resolvePromise, rejectPromise) => {
    const child = spawn(command, args, {
      cwd,
      stdio: 'inherit',
      env: process.env
    })

    child.on('error', (err) => {
      rejectPromise(err)
    })

    child.on('close', (code) => {
      if (code === 0) {
        resolvePromise()
      } else {
        rejectPromise(new Error(`Command ${command} ${args.join(' ')} exited with code ${code}`))
      }
    })
  })
}

async function main(): Promise<void> {
  console.log('🚀 [DataMigration] Starting PayIncus database migrations & initialization...')

  // 1. 执行 Prisma Schema 结构迁移（DDL）
  console.log('📦 [DataMigration] Step 1: Deploying Prisma schema migrations...')
  try {
    const localPrismaBin = resolve(serverDir, 'node_modules/.bin/prisma')
    await runCommand(localPrismaBin, ['migrate', 'deploy'], serverDir)
    console.log('✅ [DataMigration] Prisma schema migrations applied successfully.')
  } catch (err: any) {
    console.warn(`⚠️ [DataMigration] Direct prisma binary execution note: ${err?.message || err}. Trying pnpm exec...`)
    try {
      await runCommand('pnpm', ['exec', 'prisma', 'migrate', 'deploy'], serverDir)
      console.log('✅ [DataMigration] Prisma schema migrations applied successfully via pnpm.')
    } catch (pnpmErr: any) {
      console.error('❌ [DataMigration] Failed to run prisma migrate deploy:', pnpmErr)
      throw pnpmErr
    }
  }

  // 2. 初始化核心系统数据与管理员账户（DML）
  console.log('🔧 [DataMigration] Step 2: Initializing core system data, admin quota, and badges...')
  await initPrismaDatabase()

  // 3. 执行数据统计与就绪性检查
  console.log('🔍 [DataMigration] Step 3: Verifying database consistency...')
  const [usersCount, hostsCount, packagesCount, promosCount] = await Promise.all([
    prisma.user.count(),
    prisma.host.count(),
    prisma.package.count(),
    prisma.promoCode.count()
  ])

  console.log('📊 [DataMigration] Database state summary:')
  console.log(`   - Users: ${usersCount}`)
  console.log(`   - Hosts: ${hostsCount}`)
  console.log(`   - Packages: ${packagesCount}`)
  console.log(`   - Promo Codes: ${promosCount}`)

  console.log('🎉 [DataMigration] PayIncus data migrations and initialization completed successfully!')
}

main()
  .catch((err) => {
    console.error('❌ [DataMigration] Migration failed:', err)
    process.exit(1)
  })
  .finally(async () => {
    await closePrismaDatabase()
  })
