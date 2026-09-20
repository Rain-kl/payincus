import { useConfigStore } from '@/stores/config'

export function useBrand() {
  const configStore = useConfigStore()

  return {
    get brandName() {
      const name = configStore.brandName?.trim()
      return (!name || name === 'Incudal') ? 'Cloud' : name
    },
    get brandSubtitle() {
      return configStore.brandSubtitle?.trim() || '基于 Incus 的低价 NAT VPS'
    },
    get brandLogoUrl() {
      const url = configStore.brandLogoUrl?.trim()
      return (!url || url === '/incudal_logo.webp') ? '/logo.svg' : url
    },
    get topBarLogoUrl() {
      const url = configStore.brandLogoUrl?.trim()
      return (!url || url === '/incudal_logo.webp' || url === '/logo.svg') ? '/logo_white.svg' : url
    },
    get brandCopyright() {
      return configStore.brandCopyright?.trim() || '版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。'
    }
  }
}
