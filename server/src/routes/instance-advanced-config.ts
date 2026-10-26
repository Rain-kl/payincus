import type { FastifyInstance, FastifyReply } from 'fastify'
import * as db from '../db/index.js'
import { createLog } from '../db/logs.js'
import { getIncusClient } from '../lib/incus/index.js'
import { applyAdvancedConfig, type AdvancedConfig } from '../lib/incus/instance-advanced-config.js'
import { prisma } from '../db/prisma.js'
import { withLock } from '../lib/distributed-lock.js'
import { getSystemConfig } from '../db/system-config.js'
import { computeEffectiveBandwidth, normalizePlanTrafficLimitSpeed, TRAFFIC_OVERAGE_THROTTLE_CONFIG_KEY, DEFAULT_TRAFFIC_OVERAGE_THROTTLE_SPEED } from '../services/traffic-bandwidth.js'
import { apiError, ErrorCode } from '../lib/errors.js'

const dependencies = {
  ...db, getIncusClient, createLog, applyAdvancedConfig, withLock, getSystemConfig,
  getBandwidthState: (id: number) => prisma.instance.findUnique({ where: { id }, include: { package: true, packagePlan: true } })
}
const active = new Set<number>()
export function registerAdvancedConfigRoute(
  fastify: FastifyInstance,
  conflict: (reply: FastifyReply, id: number) => Promise<boolean>,
  transfer: (id: number, reply: FastifyReply) => Promise<boolean>,
  deps = dependencies
) {
  fastify.patch<{ Params: { id: string }; Body: AdvancedConfig }>('/:id/advanced-config', {
    onRequest: [fastify.authenticate],
    schema: { body: {
      type: 'object', additionalProperties: false, minProperties: 1,
      allOf: [{ not: { required: ['limitsRead', 'limitsReadIops'] } }, { not: { required: ['limitsWrite', 'limitsWriteIops'] } }],
      properties: {
        limitsRead: { type: ['string', 'null'], pattern: '^([0-9]+(\\.[0-9]+)?)(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)$' },
        limitsWrite: { type: ['string', 'null'], pattern: '^([0-9]+(\\.[0-9]+)?)(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)$' },
        limitsReadIops: { type: ['integer', 'null'], minimum: 0, maximum: 2147483647 },
        limitsWriteIops: { type: ['integer', 'null'], minimum: 0, maximum: 2147483647 },
        limitsIngress: { type: ['string', 'null'], pattern: '^([1-9][0-9]*)(Mbit|Gbit|Mbps|Gbps)$' },
        limitsEgress: { type: ['string', 'null'], pattern: '^([1-9][0-9]*)(Mbit|Gbit|Mbps|Gbps)$' },
        limitsProcesses: { type: ['integer', 'null'], minimum: 0, maximum: 2147483647 },
        limitsCpuPriority: { type: ['integer', 'null'], minimum: 0, maximum: 10 },
        bootAutostart: { type: ['boolean', 'null'] },
        bootAutostartPriority: { type: ['integer', 'null'], minimum: 0, maximum: 100 },
        bootAutostartDelay: { type: ['integer', 'null'], minimum: 0, maximum: 600 },
        bootHostShutdownTimeout: { type: ['integer', 'null'], minimum: 0, maximum: 600 }
      }
    } }
  }, async (request, reply) => {
    if (!Object.keys(request.body).length) return reply.code(400).send(apiError(ErrorCode.INVALID_PARAMS))
    const id = /^[1-9]\d*$/.test(request.params.id) ? Number(request.params.id) : NaN
    if (!Number.isSafeInteger(id)) return reply.code(400).send(apiError(ErrorCode.INVALID_ID))
    const instance = await deps.getInstanceById(id)
    if (!instance) return reply.code(404).send(apiError(ErrorCode.INSTANCE_NOT_FOUND))
    const host = await deps.getHostById(instance.host_id)
    if (!host) return reply.code(404).send(apiError(ErrorCode.HOST_NOT_FOUND))
    if (request.user.role !== 'admin' && host.user_id !== request.user.id) return reply.code(403).send(apiError(ErrorCode.FORBIDDEN))
    if (!['running', 'stopped'].includes(instance.status)) return reply.code(409).send(apiError(ErrorCode.INSTANCE_STATUS_INVALID))
    if (active.has(id)) return reply.code(409).send({ error: 'Configuration update in progress' })
    active.add(id)
    try {
      if (await transfer(id, reply) || await conflict(reply, id)) return
      const apply = async () => {
        const defaults = instance.package_id ? await deps.getPackageById(instance.package_id) : null
        const inherited: Record<string, unknown> = defaults ? { ...defaults } : {}
        const changes = { ...request.body }
        let effective: { ingress?: string; egress?: string } | undefined
        if (changes.limitsIngress !== undefined || changes.limitsEgress !== undefined) {
          const state = await deps.getBandwidthState(id)
          if (!state) throw new Error('Instance unavailable')
          for (const key of ['limitsIngress', 'limitsEgress'] as const) {
            if (typeof changes[key] === 'string') changes[key] = String(changes[key]).replace(/Mbps$/i, 'Mbit').replace(/Gbps$/i, 'Gbit').replace(/Kbps$/i, 'kbit')
          }
          const normalSpeed = normalizePlanTrafficLimitSpeed(state.packagePlan?.trafficLimitSpeed)
          if (normalSpeed) { inherited.limits_ingress = normalSpeed; inherited.limits_egress = normalSpeed }
          const overage = normalizePlanTrafficLimitSpeed(await deps.getSystemConfig(TRAFFIC_OVERAGE_THROTTLE_CONFIG_KEY)) ?? DEFAULT_TRAFFIC_OVERAGE_THROTTLE_SPEED
          const limits = computeEffectiveBandwidth({
            ...state,
            configuredLimitsIngress: changes.limitsIngress !== undefined ? changes.limitsIngress as string | null : state.configuredLimitsIngress,
            configuredLimitsEgress: changes.limitsEgress !== undefined ? changes.limitsEgress as string | null : state.configuredLimitsEgress
          }, overage)
          effective = {}
          if (changes.limitsIngress !== undefined) effective.ingress = limits.ingress ?? String(inherited.limits_ingress ?? '300Mbit')
          if (changes.limitsEgress !== undefined) effective.egress = limits.egress ?? String(inherited.limits_egress ?? '300Mbit')
        }
        const client = await deps.getIncusClient(host)
        await deps.applyAdvancedConfig(client, instance.incus_id, {
          ...changes, ...(effective?.ingress !== undefined ? { limitsIngress: effective.ingress } : {}),
          ...(effective?.egress !== undefined ? { limitsEgress: effective.egress } : {})
        }, inherited, AbortSignal.timeout(30000))
        await deps.updateInstanceConfig(id, changes as Parameters<typeof db.updateInstanceConfig>[1], effective)
      }
      if (request.body.limitsIngress !== undefined || request.body.limitsEgress !== undefined) {
        const result = await deps.withLock(`bandwidth:instance:${id}`, apply, { expireMs: 90000, waitTimeoutMs: 1000 })
        if (!result.success) throw new Error('Bandwidth update in progress')
      } else { await apply() }
      await deps.createLog(request.user.id, 'instance', 'instance.update_config', `Updated advanced configuration for instance "${instance.name}"`, 'success', { instanceId: id })
      return { message: 'Instance configuration applied', applied: true }
    } catch (error) {
      request.log.error({ err: error, instanceId: id }, 'Instance configuration update failed')
      return reply.code(502).send(apiError(ErrorCode.CONFIG_UPDATE_FAILED))
    } finally { active.delete(id) }
  })
}
