import { useConfigStore } from '@/stores/config'

export function useBrand() {
  const configStore = useConfigStore()

  return {
    get brandName() {
      return configStore.brandName?.trim() || 'Incudal'
    },
    get brandSubtitle() {
      return configStore.brandSubtitle?.trim() || '基于 Incus 的低价 NAT VPS'
    },
    get brandLogoUrl() {
      return configStore.brandLogoUrl?.trim() || '/incudal_logo.webp'
    },
    get brandCopyright() {
      return configStore.brandCopyright?.trim() || '版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。'
    }
  }
}
