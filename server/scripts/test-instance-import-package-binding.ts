import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { Prisma } from '@prisma/client'

process.env.DATABASE_URL ||= 'postgresql://test:test@127.0.0.1:5432/payincus_test'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(__dirname, '../..')

function readRepoFile(relPath: string): string {
  return fs.readFileSync(path.join(repoRoot, relPath), 'utf8')
}

async function expectBindingErrorCode(fn: () => Promise<unknown>, expectedCode: string): Promise<void> {
  let caught: unknown = null
  try {
    await fn()
  } catch (err) {
    caught = err
  }
  assert.ok(caught instanceof Error, `Expected error with code ${expectedCode}, got none`)
  assert.equal((caught as { code?: string }).code, expectedCode)
}

async function run() {
  const {
    IMPORT_PRESET_TTL_MS,
    PackageBindingError,
    pruneAndNormalizePresetStore,
    resolveExplicitExpiresAt,
    resolvePackageBindingForHost
  } = await import('../src/lib/instance-package-binding.js')

  const now = new Date('2026-11-14T12:00:00.000Z')

  // 1. resolveExplicitExpiresAt unit tests
  {
    const explicitIso = '2027-02-01T00:00:00.000Z'
    const resolvedIso = resolveExplicitExpiresAt({
      expiresAt: explicitIso,
      validDays: 10,
      defaultExpiresAt: null,
      now
    })
    assert.equal(resolvedIso?.toISOString(), explicitIso)

    const resolvedDays = resolveExplicitExpiresAt({
      validDays: 45,
      defaultExpiresAt: null,
      now
    })
    assert.equal(resolvedDays?.getTime(), now.getTime() + 45 * 24 * 60 * 60 * 1000)

    const fallbackDate = new Date('2026-12-14T12:00:00.000Z')
    assert.equal(
      resolveExplicitExpiresAt({ defaultExpiresAt: fallbackDate, now })?.toISOString(),
      fallbackDate.toISOString()
    )

    assert.throws(
      () => resolveExplicitExpiresAt({ expiresAt: 'not-a-date', defaultExpiresAt: null, now }),
      (err: unknown) => err instanceof PackageBindingError && err.code === 'INVALID_EXPIRES_AT'
    )
    assert.throws(
      () => resolveExplicitExpiresAt({ validDays: 0, defaultExpiresAt: null, now }),
      (err: unknown) => err instanceof PackageBindingError && err.code === 'INVALID_VALID_DAYS'
    )
    assert.throws(
      () => resolveExplicitExpiresAt({ validDays: 36501, defaultExpiresAt: null, now }),
      (err: unknown) => err instanceof PackageBindingError && err.code === 'INVALID_VALID_DAYS'
    )
  }

  // 2. pruneAndNormalizePresetStore unit tests
  {
    const nowMs = now.getTime()
    const staleDate = new Date(nowMs - IMPORT_PRESET_TTL_MS - 1000).toISOString()
    const freshDate = new Date(nowMs - 1000).toISOString()

    const empty = pruneAndNormalizePresetStore(null, nowMs)
    assert.deepEqual(empty, { version: 1, byTask: {}, byUnassigned: {} })

    const normalized = pruneAndNormalizePresetStore({
      byTask: {
        expiredTask: {
          hostId: 1,
          incusName: 'old-inst',
          preset: { packageId: 10, createdAt: staleDate }
        },
        badTask: { hostId: 0, preset: { packageId: 10 } },
        validTask: {
          hostId: 3,
          incusName: 'u3-migrated',
          filePath: '/home/mnt/share/u2-migrated.tar.gz',
          preset: {
            packageId: 12,
            planId: 34,
            packageName: 'Pro LXC',
            planName: 'Monthly',
            validDays: 30,
            userId: 7,
            createdAt: freshDate
          }
        }
      },
      byUnassigned: {
        '1:stale-vm': { packageId: 1, createdAt: staleDate },
        '3:u3-unassigned': {
          packageId: 12,
          planId: 34,
          packageName: 'Pro LXC',
          planName: 'Monthly',
          validDays: 60,
          createdAt: freshDate
        }
      }
    }, nowMs)

    assert.equal(normalized.byTask.expiredTask, undefined)
    assert.equal(normalized.byTask.badTask, undefined)
    assert.ok(normalized.byTask.validTask)
    assert.equal(normalized.byTask.validTask.preset.packageId, 12)
    assert.equal(normalized.byTask.validTask.preset.planId, 34)
    assert.equal(normalized.byTask.validTask.preset.userId, 7)

    assert.equal(normalized.byUnassigned['1:stale-vm'], undefined)
    assert.ok(normalized.byUnassigned['3:u3-unassigned'])
    assert.equal(normalized.byUnassigned['3:u3-unassigned'].validDays, 60)
  }

  // 3. resolvePackageBindingForHost end-to-end transaction tests (no DB required via explicit tx mock)
  {
    const samplePlans = [
      {
        id: 101,
        packageId: 50,
        name: '已停用老方案',
        description: null,
        gpuShared: null,
        gpuDeviceIds: [],
        cpu: 100,
        memory: 512,
        disk: 10240,
        portLimit: 10,
        snapshotLimit: 2,
        backupLimit: 1,
        siteLimit: 3,
        swapSize: 256,
        trafficLimit: BigInt(100 * 1024 * 1024 * 1024),
        trafficLimitSpeed: '100Mbit',
        price: new Prisma.Decimal(1990), // 19.90 元（分）
        billingCycle: 1,
        setupFee: new Prisma.Decimal(0),
        trafficResetPrice: null,
        isActive: false,
        isSoldOut: false,
        sortOrder: 0,
        slaGuarantee: null,
        createdAt: now
      },
      {
        id: 102,
        packageId: 50,
        name: '月付标准版',
        description: null,
        gpuShared: null,
        gpuDeviceIds: [],
        cpu: 200,
        memory: 1024,
        disk: 20480,
        portLimit: 25,
        snapshotLimit: 6,
        backupLimit: 4,
        siteLimit: 8,
        swapSize: 512,
        trafficLimit: BigInt(500 * 1024 * 1024 * 1024),
        trafficLimitSpeed: '300Mbit',
        price: new Prisma.Decimal(2900), // 29.00 元（分）
        billingCycle: 1,
        setupFee: new Prisma.Decimal(0),
        trafficResetPrice: null,
        isActive: true,
        isSoldOut: false,
        sortOrder: 1,
        slaGuarantee: null,
        createdAt: now
      },
      {
        id: 103,
        packageId: 50,
        name: '损坏限速方案',
        description: null,
        gpuShared: null,
        gpuDeviceIds: [],
        cpu: 200,
        memory: 1024,
        disk: 20480,
        portLimit: 25,
        snapshotLimit: 6,
        backupLimit: 4,
        siteLimit: 8,
        swapSize: 0,
        trafficLimit: BigInt(500 * 1024 * 1024 * 1024),
        trafficLimitSpeed: 'bad-speed',
        price: new Prisma.Decimal(2900),
        billingCycle: 1,
        setupFee: new Prisma.Decimal(0),
        trafficResetPrice: null,
        isActive: false,
        isSoldOut: false,
        sortOrder: 2,
        slaGuarantee: null,
        createdAt: now
      }
    ]

    const samplePkg = {
      id: 50,
      userId: 1,
      name: 'HK NAT LXC',
      description: null,
      cpuMax: 200,
      memoryMax: 2048,
      diskMax: 40960,
      bandwidthMax: null,
      networkMode: 'nat',
      instanceType: 'container',
      nodeSelectors: [],
      privileged: false,
      gpuShared: false,
      gpuDeviceIds: [],
      nested: false,
      active: true,
      portLimit: 20,
      snapshotLimit: 5,
      backupLimit: 3,
      siteLimit: 10,
      monthlyTrafficLimit: BigInt(200 * 1024 * 1024 * 1024),
      trafficResetPrice: new Prisma.Decimal(0),
      ioLimitMode: 'throughput',
      limitsRead: '150MB',
      limitsWrite: '150MB',
      limitsReadIops: 800,
      limitsWriteIops: 800,
      limitsIngress: '200Mbit',
      limitsEgress: '200Mbit',
      limitsProcesses: 500,
      limitsCpuPriority: 10,
      bootAutostart: true,
      bootAutostartPriority: 20,
      bootAutostartDelay: 15,
      bootHostShutdownTimeout: 30,
      globalShared: false,
      globalQuotaMultiplier: null,
      globalMaxInstances: null,
      requiredPackageId: null,
      allowInstanceDeletion: true,
      releaseChannelId: null,
      egressMode: 'host',
      egressProfileId: null,
      createdAt: now,
      plans: samplePlans
    }

    const mockTx = {
      $executeRaw: async () => 1,
      package: {
        findUnique: async ({ where }: { where: { id: number } }) => {
          return where.id === 50 ? samplePkg : null
        }
      },
      packageHost: {
        findUnique: async () => ({ trafficMultiplier: new Prisma.Decimal(2) })
      },
      egressProfile: {
        findUnique: async () => null
      }
    } as unknown as Parameters<typeof resolvePackageBindingForHost>[0]

    // 3a. Explicit free instance mode (`packageId: null`)
    const freeResult = await resolvePackageBindingForHost(mockTx, { hostId: 3, packageId: null, now })
    assert.equal(freeResult.packageId, null)
    assert.equal(freeResult.packagePlanId, null)
    assert.equal(freeResult.billingPrice, null)
    assert.equal(freeResult.expiresAt, null)

    // 3b. Package chosen without explicit planId -> auto-binds first active plan (#102, not #101)
    const defaultPlanResult = await resolvePackageBindingForHost(mockTx, {
      hostId: 3,
      packageId: 50,
      instanceType: 'container',
      now
    })
    assert.equal(defaultPlanResult.packageId, 50)
    assert.equal(defaultPlanResult.packagePlanId, 102)
    assert.equal(defaultPlanResult.packageName, 'HK NAT LXC')
    assert.equal(defaultPlanResult.planName, '月付标准版')
    assert.equal(Number(defaultPlanResult.billingPrice), 29)
    assert.equal(defaultPlanResult.billingCycle, 1)
    assert.ok(defaultPlanResult.expiresAt instanceof Date && defaultPlanResult.expiresAt.getTime() > now.getTime())
    assert.equal(defaultPlanResult.portLimit, 25)
    assert.equal(defaultPlanResult.snapshotLimit, 6)
    assert.equal(defaultPlanResult.backupLimit, 4)
    assert.equal(defaultPlanResult.siteLimit, 8)
    assert.equal(defaultPlanResult.swapEnabled, true)
    assert.equal(defaultPlanResult.swapSize, 512)
    assert.equal(defaultPlanResult.limitsIngress, '300Mbit')
    assert.equal(defaultPlanResult.limitsEgress, '300Mbit')
    assert.equal(defaultPlanResult.monthlyTrafficLimit, BigInt(1000 * 1024 * 1024 * 1024))
    assert.equal(defaultPlanResult.egressMode, 'host')

    // 3c. Explicit package-only free tier (`packageId: 50, planId: null`)
    const packageOnlyResult = await resolvePackageBindingForHost(mockTx, {
      hostId: 3,
      packageId: 50,
      planId: null,
      now
    })
    assert.equal(packageOnlyResult.packageId, 50)
    assert.equal(packageOnlyResult.packagePlanId, null)
    assert.equal(packageOnlyResult.billingPrice, null)
    assert.equal(packageOnlyResult.expiresAt, null)
    assert.equal(packageOnlyResult.portLimit, 20)
    assert.equal(packageOnlyResult.monthlyTrafficLimit, BigInt(400 * 1024 * 1024 * 1024))

    // 3d. Custom validDays overrides default plan expiry while keeping plan price/quotas
    const customDaysResult = await resolvePackageBindingForHost(mockTx, {
      hostId: 3,
      packageId: 50,
      planId: 102,
      validDays: 90,
      now
    })
    assert.equal(customDaysResult.packagePlanId, 102)
    assert.equal(customDaysResult.expiresAt?.getTime(), now.getTime() + 90 * 24 * 60 * 60 * 1000)

    // 3e. Rejection guards
    await expectBindingErrorCode(
      () => resolvePackageBindingForHost(mockTx, { hostId: 3, packageId: null, planId: 102, now }),
      'PACKAGE_REQUIRED_FOR_PLAN'
    )
    await expectBindingErrorCode(
      () => resolvePackageBindingForHost(mockTx, { hostId: 3, packageId: 999, now }),
      'PACKAGE_NOT_FOUND'
    )
    await expectBindingErrorCode(
      () => resolvePackageBindingForHost(mockTx, { hostId: 3, packageId: 50, instanceType: 'vm', now }),
      'PACKAGE_INSTANCE_TYPE_MISMATCH'
    )
    await expectBindingErrorCode(
      () => resolvePackageBindingForHost(mockTx, { hostId: 3, packageId: 50, planId: 888, now }),
      'PLAN_NOT_IN_PACKAGE'
    )
    await expectBindingErrorCode(
      () => resolvePackageBindingForHost(mockTx, { hostId: 3, packageId: 50, planId: 103, now }),
      'INVALID_PLAN_SPEED'
    )
  }

  // 4. Route & UI source guard assertions
  {
    const transferRoute = readRepoFile('server/src/routes/admin-host-instance-transfer.ts')
    assert.match(transferRoute, /\/api\/admin\/hosts\/:hostId\/import-packages/)
    assert.match(transferRoute, /\/api\/admin\/hosts\/:hostId\/instances\/:instanceId\/package/)
    assert.match(transferRoute, /onRequest:\s*\[app\.authenticate,\s*app\.requireAdmin\]/)
    assert.match(transferRoute, /validatePackageBindingSelection\(/)
    assert.match(transferRoute, /savePendingImportTaskPreset\(/)
    assert.match(transferRoute, /finalizeImportedInstancePreset\(/)
    assert.match(transferRoute, /registerImportedHostInstance\(/)

    const instancesRoute = readRepoFile('server/src/routes/instances.ts')
    assert.match(instancesRoute, /packageId:\s*instance\.package_id\s*\|\|\s*null/)

    const hostTab = readRepoFile('client/src/components/host/HostInstancesTab.vue')
    assert.match(hostTab, /<PackageBindingFields/)
    assert.match(hostTab, /loadImportPackages\(props\.hostId\)/)
    assert.match(hostTab, /isPackageSelectionReady\(importPackageChoice\.value\)/)
    assert.match(hostTab, /isPackageSelectionReady\(assignPackageChoice\.value/)
    assert.match(hostTab, /openBindPackageModal/)

    const bindingFields = readRepoFile('client/src/components/host/PackageBindingFields.vue')
    assert.match(bindingFields, /importPackagePlaceholder/)
    assert.match(bindingFields, /importPackageNoneOption/)
    assert.match(bindingFields, /importPackageRequiredWarning/)
  }

  console.log('✓ instance import package binding tests passed')
}

run().catch((err) => {
  console.error(err)
  process.exit(1)
})
