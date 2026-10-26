import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { prisma } from '../src/db/prisma.js'
import { getInstanceConfig, updateInstanceConfig } from '../src/db/instances.js'
import { reconcileEffectiveBandwidth } from '../src/services/traffic-bandwidth.js'

if (!/\/(node_configuration_validation|node_security_validation)(\?|$)/.test(process.env.DATABASE_URL ?? '')) throw new Error('Requires a dedicated node configuration test database')
const user = await prisma.user.create({ data: { username: `config-${randomUUID()}`, passwordHash: 'disabled-fixture-account' } })
const host = await prisma.host.create({ data: { userId: user.id, name: 'configuration fixture', url: 'https://127.0.0.1:1', status: 'offline' } })
const pkg = await prisma.package.create({ data: { userId: user.id, name: 'configuration fixture', cpuMax: 200, memoryMax: 2048, diskMax: 10240 } })
const instance = await prisma.instance.create({ data: { userId: user.id, hostId: host.id, packageId: pkg.id, name: 'configuration fixture', incusId: 'config-fixture', image: 'fixture', cpu: 100, memory: 512, disk: 1024, status: 'running' } })
try {
  await updateInstanceConfig(instance.id, { limitsIngress: '500Mbit', limitsEgress: '600Mbit', limitsProcesses: 4000, limitsCpuPriority: 0, bootAutostart: false, bootAutostartDelay: 0 }, { ingress: '500Mbit', egress: '600Mbit' })
  let row = await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })
  assert.equal(row.configuredLimitsIngress, '500Mbit')
  assert.equal(row.limitsIngress, '500Mbit')
  let config = await getInstanceConfig(instance.id)
  assert.equal(config?.limits_ingress, '500Mbit')
  assert.equal(config?.limits_cpu_priority, 0)
  assert.equal(config?.boot_autostart, false)
  await prisma.instance.update({ where: { id: instance.id }, data: { trafficStatus: 'LIMITED' } })
  await reconcileEffectiveBandwidth(instance.id)
  row = await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })
  assert.equal(row.limitsIngress, '1Mbit')
  assert.equal(row.configuredLimitsIngress, '500Mbit')
  assert.equal((await getInstanceConfig(instance.id))?.limits_ingress, '500Mbit')
  await prisma.instance.update({ where: { id: instance.id }, data: { trafficStatus: 'NORMAL' } })
  await reconcileEffectiveBandwidth(instance.id)
  row = await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })
  assert.equal(row.limitsIngress, '500Mbit')
  assert.equal(row.limitsEgress, '600Mbit')
  await updateInstanceConfig(instance.id, { limitsIngress: null, limitsEgress: null, bootAutostart: null })
  await reconcileEffectiveBandwidth(instance.id)
  row = await prisma.instance.findUniqueOrThrow({ where: { id: instance.id } })
  config = await getInstanceConfig(instance.id)
  assert.equal(config?.limits_ingress, null)
  assert.equal(config?.boot_autostart, null)
  assert.equal(row.limitsIngress, '300Mbit')
  console.log('PostgreSQL advanced settings persistence, zero/false, independent bandwidth intent, overage/recovery and reset passed')
} finally {
  await prisma.instance.delete({ where: { id: instance.id } })
  await prisma.package.delete({ where: { id: pkg.id } })
  await prisma.host.delete({ where: { id: host.id } })
  await prisma.user.delete({ where: { id: user.id } })
  await prisma.$disconnect()
}
