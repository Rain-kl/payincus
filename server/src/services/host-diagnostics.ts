import { prisma } from '../db/prisma.js'
import { IncusApiError, IncusClient, nodeErrorDetail } from '../lib/incus/incus-client.js'
import { hostTunnelManager } from '../lib/incus/tunnel-manager.js'
import { recordHostSuccess } from '../lib/incus/host-health.js'
import { recordReport } from '../lib/node-security/report.js'
import { syncDesiredSecurityPolicy } from '../lib/node-security/policy-sync.js'

export type DiagnosticItemStatus = 'pass' | 'warn' | 'fail' | 'skipped'

export interface DiagnosticCheckItem {
  key: 'agentHeartbeat' | 'reverseTunnel' | 'incusMtls' | 'nodeSecurity' | 'storageBaseline'
  label: string
  status: DiagnosticItemStatus
  ok: boolean
  latencyMs?: number | null
  detail: string
  meta?: Record<string, unknown>
}

export interface HostDiagnosticResult {
  hostId: number
  hostName: string
  checkedAt: string
  ok: boolean
  overallStatus: 'healthy' | 'warning' | 'error'
  summary: string
  needsBaselineSync: boolean
  checks: {
    agentHeartbeat: DiagnosticCheckItem
    reverseTunnel: DiagnosticCheckItem
    incusMtls: DiagnosticCheckItem
    nodeSecurity: DiagnosticCheckItem
    storageBaseline: DiagnosticCheckItem
  }
}

const isRecord = (v: unknown): v is Record<string, any> => !!v && typeof v === 'object' && !Array.isArray(v)

async function withTimeout<T>(promise: Promise<T>, ms: number, message: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(message)), ms)
      })
    ])
  } finally {
    if (timer) clearTimeout(timer)
  }
}

export async function runHostDiagnostics(hostId: number): Promise<HostDiagnosticResult> {
  const host = await prisma.host.findUnique({
    where: { id: hostId },
    include: {
      agent: true,
      nodeSecurity: true,
      storagePools: { orderBy: { createdAt: 'asc' } }
    }
  })
  if (!host) throw new Error('宿主机不存在')

  const now = Date.now()

  // 1. Agent 心跳检查
  let agentHeartbeat: DiagnosticCheckItem
  if (!host.agent) {
    agentHeartbeat = {
      key: 'agentHeartbeat',
      label: 'Agent 心跳上报',
      status: 'fail',
      ok: false,
      detail: '尚未绑定或安装节点 Agent'
    }
  } else {
    const metrics = isRecord(host.agent.lastReport) && isRecord((host.agent.lastReport as any).metrics)
      ? ((host.agent.lastReport as any).metrics as Record<string, unknown>)
      : {}
    const intervalSec = typeof metrics.heartbeatIntervalSeconds === 'number' && metrics.heartbeatIntervalSeconds > 0
      ? metrics.heartbeatIntervalSeconds
      : 5
    const staleThresholdMs = Math.max(intervalSec * 3 * 1000, 35_000)
    const lastSeenMs = host.agent.lastSeenAt ? host.agent.lastSeenAt.getTime() : 0
    const ageMs = lastSeenMs > 0 ? Math.max(0, now - lastSeenMs) : null
    const fresh = ageMs !== null && ageMs <= staleThresholdMs && host.agent.status === 'online'
    agentHeartbeat = {
      key: 'agentHeartbeat',
      label: 'Agent 心跳上报',
      status: fresh ? 'pass' : 'fail',
      ok: fresh,
      detail: ageMs === null
        ? '尚未收到过节点心跳'
        : fresh
          ? `在线（${Math.round(ageMs / 1000)} 秒前心跳，版本 ${host.agent.version || '未知'}）`
          : `心跳超时（已失联 ${Math.round(ageMs / 1000)} 秒，阈值 ${Math.round(staleThresholdMs / 1000)} 秒）`,
      meta: {
        version: host.agent.version || null,
        lastSeenAt: host.agent.lastSeenAt?.toISOString() || null,
        lastHeartbeatIp: host.agent.lastHeartbeatIp || null,
        ageSeconds: ageMs !== null ? Math.round(ageMs / 1000) : null
      }
    }
  }

  // 2. 反向隧道与 WebSocket RTT
  let reverseTunnel: DiagnosticCheckItem
  if (!host.tunnelEnabled) {
    reverseTunnel = {
      key: 'reverseTunnel',
      label: '反向 WebSocket 隧道',
      status: 'skipped',
      ok: true,
      detail: '当前使用直连 mTLS 模式'
    }
  } else {
    const ping = await hostTunnelManager.measureTunnelPing(hostId, 3500)
    reverseTunnel = {
      key: 'reverseTunnel',
      label: '反向 WebSocket 隧道',
      status: ping.ok ? 'pass' : 'fail',
      ok: ping.ok,
      latencyMs: ping.latencyMs,
      detail: ping.ok
        ? ping.latencyMs !== null
          ? `隧道在线 · RTT ${ping.latencyMs} ms`
          : '隧道在线'
        : ping.error || '反向隧道未连接'
    }
  }

  // 3. Incus mTLS + Live 状态核验
  let incusMtls: DiagnosticCheckItem
  let nodeSecurity: DiagnosticCheckItem
  let storageBaseline: DiagnosticCheckItem
  let needsBaselineSync = false

  const dbSecurityReport = isRecord(host.nodeSecurity?.report)
    ? (host.nodeSecurity!.report as Record<string, any>)
    : null
  const dbPools = (host.storagePools || []).filter(p => !(isRecord(p.config) && '_payincusDeletion' in p.config))

  if (host.tunnelEnabled && !reverseTunnel.ok) {
    incusMtls = {
      key: 'incusMtls',
      label: 'Incus mTLS 控制通道',
      status: 'fail',
      ok: false,
      detail: '因反向隧道未在线，无法建立 mTLS 通道'
    }
    nodeSecurity = {
      key: 'nodeSecurity',
      label: '安全模式与节点状态',
      status: dbSecurityReport?.locked === true ? 'warn' : 'fail',
      ok: false,
      detail: dbSecurityReport?.locked === true
        ? '节点断开前处于安全锁定状态，无法实时核验'
        : '无法连接节点核验实时安全状态',
      meta: {
        cachedLocked: dbSecurityReport?.locked === true,
        cachedEnabled: dbSecurityReport?.enabled === true
      }
    }
    storageBaseline = {
      key: 'storageBaseline',
      label: '存储池基线一致性',
      status: dbPools.some(p => p.status === 'missing') ? 'warn' : 'fail',
      ok: false,
      detail: '无法连接节点比对实时存储池，当前仅展示缓存状态',
      meta: {
        cachedPoolCount: dbPools.length,
        missingInCache: dbPools.filter(p => p.status === 'missing').map(p => p.name)
      }
    }
  } else {
    const client = new IncusClient({
      url: host.url,
      certPath: host.certPath,
      keyPath: host.keyPath,
      hostId,
      tunnelEnabled: host.tunnelEnabled,
      targetHost: host.targetHost,
      targetPort: host.targetPort,
      ...(host.nodeSecurity?.serverFingerprint ? { serverFingerprint: host.nodeSecurity.serverFingerprint } : {})
    })

    const mTlsStart = Date.now()
    let baselinePayload: Record<string, any> | null = null
    let liveSecurityState: Record<string, any> | null = null
    let liveStoragePools: Array<Record<string, any>> | null = null

    try {
      await withTimeout(client.connect(), 5500, 'mTLS 握手连接节点超时 (5.5s)')
      const agentInfo = await withTimeout(
        client.request<{ api_extensions?: string[]; version?: string }>('GET', '/1.0/agent'),
        4000,
        '读取 /1.0/agent 超时'
      ).catch(() => null)

      try {
        baselinePayload = await withTimeout(
          client.request<Record<string, any>>('GET', '/1.0/agent/baseline'),
          6000,
          '读取节点基线超时 (6s)'
        )
      } catch (err) {
        if (err instanceof IncusApiError && (err.statusCode === 404 || err.statusCode === 405)) {
          const [secRes, poolsRes] = await Promise.all([
            withTimeout(client.request<Record<string, any>>('GET', '/1.0/agent/security'), 4000, '读取安全状态超时').catch(() => null),
            withTimeout(client.request<Array<Record<string, any>>>('GET', '/1.0/storage-pools?recursion=1'), 4000, '读取存储池列表超时').catch(() => null)
          ])
          liveSecurityState = secRes
          liveStoragePools = Array.isArray(poolsRes) ? poolsRes : null
        } else {
          throw err
        }
      }

      const latencyMs = Math.max(0, Date.now() - mTlsStart)
      recordHostSuccess(hostId, latencyMs)
      incusMtls = {
        key: 'incusMtls',
        label: 'Incus mTLS 控制通道',
        status: 'pass',
        ok: true,
        latencyMs,
        detail: `mTLS 验证通过 · ${latencyMs} ms${host.nodeSecurity?.serverFingerprint ? '（已校验证书指纹）' : '（尚未核对绑定证书指纹）'}`,
        meta: {
          fingerprintPinned: !!host.nodeSecurity?.serverFingerprint,
          apiExtensions: Array.isArray(agentInfo?.api_extensions) ? agentInfo.api_extensions : []
        }
      }

      if (baselinePayload) {
        if (isRecord(baselinePayload.nodeSecurity?.state)) {
          liveSecurityState = baselinePayload.nodeSecurity.state
        }
        if (isRecord(baselinePayload.storage) && Array.isArray(baselinePayload.storage.pools)) {
          liveStoragePools = baselinePayload.storage.pools as Array<Record<string, any>>
        }
      }

      // 4. 实时安全状态同步 & 一致性检查
      if (liveSecurityState && typeof liveSecurityState.enabled === 'boolean' && typeof liveSecurityState.locked === 'boolean' && Number.isSafeInteger(liveSecurityState.revision)) {
        await recordReport(hostId, { state: liveSecurityState, events: [] }, { authoritative: true }).catch(() => {})
        const locked = liveSecurityState.locked === true
        const enabled = liveSecurityState.enabled === true
        const desiredEnabled = typeof dbSecurityReport?._desiredEnabled === 'boolean' ? dbSecurityReport._desiredEnabled : undefined
        let policySynced = false
        if (!locked && desiredEnabled !== undefined && desiredEnabled !== enabled && host.nodeSecurity?.serverFingerprint) {
          policySynced = await syncDesiredSecurityPolicy(hostId, true).catch(() => false)
        }
        const effectiveEnabled = policySynced && desiredEnabled !== undefined ? desiredEnabled : enabled
        nodeSecurity = locked
          ? {
              key: 'nodeSecurity',
              label: '安全模式与节点状态',
              status: 'warn',
              ok: false,
              detail: `节点当前处于安全锁定状态（已停止实例），需在「运维 -> 节点安全模式」批准恢复${liveSecurityState.reason ? ` · 原因: ${liveSecurityState.reason}` : ''}`,
              meta: { locked: true, enabled: effectiveEnabled, revision: liveSecurityState.revision, reason: liveSecurityState.reason || '' }
            }
          : {
              key: 'nodeSecurity',
              label: '安全模式与节点状态',
              status: 'pass',
              ok: true,
              detail: effectiveEnabled
                ? `节点正常运行 · 安全模式已开启（revision ${liveSecurityState.revision}）`
                : `节点正常运行 · 安全模式未开启（revision ${liveSecurityState.revision}）`,
              meta: { locked: false, enabled: effectiveEnabled, revision: liveSecurityState.revision, policySynced }
            }
      } else {
        nodeSecurity = {
          key: 'nodeSecurity',
          label: '安全模式与节点状态',
          status: dbSecurityReport?.locked === true ? 'warn' : 'pass',
          ok: dbSecurityReport?.locked !== true,
          detail: host.nodeSecurity?.serverFingerprint
            ? '未能获取实时安全状态（可能为旧版节点）'
            : '尚未核对节点证书指纹，建议在「运维 -> 核对证书指纹」完成绑定',
          meta: { fingerprintPinned: !!host.nodeSecurity?.serverFingerprint }
        }
      }

      // 5. 存储池基线对比（读只比对，提示是否需要点击基线同步）
      if (liveStoragePools) {
        const nodeByName = new Map<string, Record<string, any>>()
        for (const p of liveStoragePools) {
          if (typeof p?.name === 'string') nodeByName.set(p.name, p)
        }
        const dbNames = new Set(dbPools.map(p => p.name))
        const missingOnNode: string[] = []
        const mismatched: string[] = []
        for (const dbPool of dbPools) {
          const cfg = isRecord(dbPool.config) ? (dbPool.config as Record<string, any>) : {}
          if (cfg._payincusImport) continue
          const livePool = nodeByName.get(dbPool.name)
          if (!livePool) {
            missingOnNode.push(dbPool.name)
            continue
          }
          const dbSource = cfg['zfs.pool_name'] || cfg.source
          const liveCfg = isRecord(livePool.config) ? livePool.config : {}
          const liveSource = liveCfg['zfs.pool_name'] || liveCfg.source
          const sourceMatches = !dbSource || String(dbSource).startsWith('/') || dbSource === liveSource
          if (livePool.driver !== dbPool.driver || !sourceMatches) {
            missingOnNode.push(dbPool.name)
            continue
          }
          if (livePool.status === 'ready' && dbPool.status !== 'ready') {
            mismatched.push(dbPool.name)
          } else if (livePool.status === 'locked' && dbPool.status !== 'locked') {
            mismatched.push(dbPool.name)
          }
        }
        const untrackedOnNode = [...nodeByName.keys()].filter(name => !dbNames.has(name))
        const inSync = missingOnNode.length === 0 && untrackedOnNode.length === 0 && mismatched.length === 0
        needsBaselineSync = !inSync

        const issues: string[] = []
        if (missingOnNode.length > 0) issues.push(`控制端有 ${missingOnNode.length} 个缓存池在节点缺失（${missingOnNode.join(', ')}）`)
        if (untrackedOnNode.length > 0) issues.push(`节点有 ${untrackedOnNode.length} 个未同步存储池（${untrackedOnNode.join(', ')}）`)
        if (mismatched.length > 0) issues.push(`${mismatched.length} 个存储池状态与节点缓存不一致（${mismatched.join(', ')}）`)

        storageBaseline = {
          key: 'storageBaseline',
          label: '存储池基线一致性',
          status: inSync ? 'pass' : 'warn',
          ok: inSync,
          detail: inSync
            ? `基线一致（已同步 ${dbPools.length} 个存储池）`
            : `${issues.join('；')}，建议执行基线同步`,
          meta: {
            inSync,
            dbPoolCount: dbPools.length,
            nodePoolCount: nodeByName.size,
            missingOnNode,
            untrackedOnNode,
            mismatched
          }
        }
      } else {
        storageBaseline = {
          key: 'storageBaseline',
          label: '存储池基线一致性',
          status: 'warn',
          ok: false,
          detail: '未能读取节点实时存储池列表（节点锁定或接口不支持）'
        }
      }
    } catch (err) {
      const detail = nodeErrorDetail(err, 'mTLS 连接或状态查询失败')
      const fingerprintMismatch = /指纹|fingerprint/i.test(detail)
      incusMtls = {
        key: 'incusMtls',
        label: 'Incus mTLS 控制通道',
        status: 'fail',
        ok: false,
        latencyMs: Math.max(0, Date.now() - mTlsStart),
        detail: fingerprintMismatch
          ? `${detail}（若节点已重装，请在「运维 -> 核对证书指纹」更新指纹）`
          : detail,
        meta: { fingerprintMismatch }
      }
      nodeSecurity = {
        key: 'nodeSecurity',
        label: '安全模式与节点状态',
        status: 'fail',
        ok: false,
        detail: '因 mTLS 通道未通过，无法核验节点实时安全状态'
      }
      storageBaseline = {
        key: 'storageBaseline',
        label: '存储池基线一致性',
        status: 'fail',
        ok: false,
        detail: '因 mTLS 通道未通过，无法比对存储池基线'
      }
    } finally {
      await client.close().catch(() => {})
    }
  }

  const allItems = [agentHeartbeat, reverseTunnel, incusMtls, nodeSecurity, storageBaseline]
  const hasFail = allItems.some(item => item.status === 'fail')
  const hasWarn = allItems.some(item => item.status === 'warn')
  const ok = !hasFail && !hasWarn
  const overallStatus = hasFail ? 'error' : hasWarn ? 'warning' : 'healthy'
  const summary = ok
    ? '所有连接与基线检查均已通过'
    : hasFail
      ? '节点链路存在故障项，请查看下方逐项诊断'
      : '链路正常，但存在需要注意的状态或基线差异'

  return {
    hostId,
    hostName: host.name,
    checkedAt: new Date().toISOString(),
    ok,
    overallStatus,
    summary,
    needsBaselineSync,
    checks: {
      agentHeartbeat,
      reverseTunnel,
      incusMtls,
      nodeSecurity,
      storageBaseline
    }
  }
}
