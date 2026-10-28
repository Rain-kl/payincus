// Hardware inventory comes from authenticated node heartbeats.
export function extractHostPublicIpv6FromResources(resources: Record<string, unknown>): string | null {
  try {
    const network = resources.network as { cards?: Array<{ addresses?: Array<{ address?: string; family?: string; scope?: string }>; ports?: Array<{ addresses?: Array<{ address?: string; family?: string; scope?: string }> }> }> } | undefined
    if (!network?.cards) return null

    for (const card of network.cards) {
      // 直接检查网卡地址
      const addresses = card.addresses || []
      for (const addr of addresses) {
        if (addr.family === 'inet6' && addr.scope === 'global' && addr.address) {
          const ip = addr.address
          // 排除 link-local、unique-local 地址
          if (!ip.startsWith('fe80:') && !ip.startsWith('fd') && !ip.startsWith('fc')) {
            return ip
          }
        }
      }
      // 检查端口上的地址
      if (card.ports) {
        for (const port of card.ports) {
          const portAddresses = port.addresses || []
          for (const addr of portAddresses) {
            if (addr.family === 'inet6' && addr.scope === 'global' && addr.address) {
              const ip = addr.address
              if (!ip.startsWith('fe80:') && !ip.startsWith('fd') && !ip.startsWith('fc')) {
                return ip
              }
            }
          }
        }
      }
    }
  } catch {
    // 忽略解析失败
  }
  return null
}
