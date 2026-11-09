<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { ExternalLink, Info, Radio, Send } from 'lucide-vue-next'
import api from '@/api/admin'
import { useConfigStore } from '@/stores/config'
import { useToast } from '@/stores/toast'
import type { Package } from '@/types/api'
import { translateError } from '@/utils/errorHandler'
import {
  systemSettingsNavigationItems,
  systemSettingsSections,
  type SystemSettingsSectionKey
} from '@/constants/adminSettings'
import {
  Badge,
  Button,
  Card,
  Input,
  PageHeader,
  Select
} from '@/components/ui'

const { t } = useI18n()
const toast = useToast()
const route = useRoute()
const configStore = useConfigStore()

const loading = ref(true)
// 每个卡片独立的 saving 状态
const savingRegistration = ref(false)
const savingInvitePricing = ref(false)
const savingAffRates = ref(false)
const savingVipBenefits = ref(false)
const savingPopupAnnouncement = ref(false)
const savingPopupPromo = ref(false)
const savingQuota = ref(false)
const savingTurnstile = ref(false)
const savingAvatar = ref(false)
const savingSmtp = ref(false)
const savingEmailDomain = ref(false)
const savingTransfer = ref(false)
const savingFooterLinks = ref(false)
const savingBrand = ref(false)
const savingTicket = ref(false)
const savingFreeSite = ref(false)
const savingTicketImages = ref(false)
const savingPlatformPermissions = ref(false)
const testingSmtp = ref(false)
const sendingTestEmail = ref(false)
const testEmailTo = ref('')
const popupPromoPackages = ref<Package[]>([])
const popupPromoPackagesLoading = ref(false)
const popupPromoPackagesLoaded = ref(false)
const MAX_TRANSFER_FEE = 100
const MAX_AFF_COMMISSION_RATE = 0.5
const MAX_AFF_DISCOUNT_RATE = 0.95

interface ConfigItem {
  id: number
  key: string
  value: string
  type: string
  label: string | null
  description: string | null
}

interface VipBenefitLevelConfig {
  orderDiscountPercent: number
  extraTrafficPercent: number
  resourcePoolBonusPercent: number
}

interface VipBenefitsConfig {
  levels: Record<string, VipBenefitLevelConfig>
}

function defaultVipBenefitsConfig(): VipBenefitsConfig {
  return {
    levels: Object.fromEntries(Array.from({ length: 5 }, (_, index) => {
      const level = index + 1
      return [String(level), {
        orderDiscountPercent: level,
        extraTrafficPercent: level * 2,
        resourcePoolBonusPercent: level * 2
      }]
    }))
  }
}

const configs = ref<ConfigItem[]>([])

// 表单数据
const form = ref({
  default_quota_host: 0,
  default_quota_friend: 0,
  default_quota_package: 0,
  registration_enabled: true,
  require_invite_code: true,
  invite_generation_costs: [
    { resource: 'balance', amount: 0, enabled: false },
    { resource: 'points', amount: 0, enabled: false }
  ],
  invite_default_expire_days: 0,
  aff_commission_rate: 0.05,
  aff_discount_rate: 0.05,
  vip_benefits_config: defaultVipBenefitsConfig(),
  popup_announcement: '',
  popup_promo_image_url: '',
  popup_promo_package_id: '',
  ticket_enabled: true,
  ticket_auto_close_enabled: true,
  ticket_auto_close_hours: 24,
  free_site_mode: false,
  free_site_register_gift_enabled: false,
  free_site_register_gift_balance: 0,
  free_site_register_gift_points: 0,
  // Turnstile 配置
  turnstile_enabled: false,
  turnstile_site_key: '',
  turnstile_secret_key: '',
  // 头像 API 配置
  avatar_api_base: 'https://api.dicebear.com/9.x',
  // SMTP 邮件配置
  smtp_enabled: false,
  smtp_host: '',
  smtp_port: 587,
  smtp_secure: false,
  smtp_username: '',
  smtp_password: '',
  smtp_from_email: '',
  smtp_from_name: 'Incudal',
  brand_name: 'Incudal',
  brand_subtitle: '基于 Incus 的低价 NAT VPS',
  brand_logo_url: '/logo.svg',
  brand_copyright: '',
  // 邮箱域名白名单配置
  email_domain_whitelist_enabled: false,
  email_allowed_domains: '',
  // 转移手续费配置
  transfer_fee: 0,
  // 侧边栏底部联系方式
  footer_contact_email: 'ryan@io0.cc',
  // 工单图片 Lsky 配置
  ticket_image_lsky_base_url: '',
  ticket_image_lsky_token: '',
  ticket_image_lsky_api_version: 'v1',
  ticket_image_lsky_target_id: '',
  // 平台运营配置
  system_update_allowed_admin_ids: '',
  payincus_gift_card_admin_ids: ''
})

// Config metadata - computed to support i18n
// 注意：好友配额已禁用，不再显示 default_quota_friend
const configMeta = computed(() => {
  try {
    return {
      default_quota_host: {
        label: t('admin.system.quotaHost') || '默认宿主机配额',
        unit: t('admin.system.unitCount') || '个',
        description: t('admin.system.quotaHostDesc') || '新用户默认可创建宿主机数量'
      },
      default_quota_package: {
        label: t('admin.system.quotaPackage') || '默认套餐配额',
        unit: t('admin.system.unitCount') || '个',
        description: t('admin.system.quotaPackageDesc') || '新用户默认可创建套餐数量（0 = 未授权）'
      }
    }
  } catch (err) {
    console.error('Config metadata computation failed:', err)
    return {
      default_quota_host: { label: '默认宿主机配额', unit: '个', description: '新用户默认可创建宿主机数量（0 = 未授权）' },
      default_quota_package: { label: '默认套餐配额', unit: '个', description: '新用户默认可创建套餐数量（0 = 未授权）' }
    }
  }
})

// 数字类型的配置键
const numericConfigKeys = ['default_quota_host', 'default_quota_friend', 'default_quota_package', 'smtp_port', 'free_site_register_gift_points', 'invite_default_expire_days', 'ticket_auto_close_hours']

// 浮点数类型的配置键
const floatConfigKeys = ['transfer_fee', 'free_site_register_gift_balance', 'aff_commission_rate', 'aff_discount_rate']

// 布尔类型的配置键
const booleanConfigKeys = ['registration_enabled', 'require_invite_code', 'ticket_enabled', 'ticket_auto_close_enabled', 'free_site_mode', 'free_site_register_gift_enabled', 'turnstile_enabled', 'smtp_enabled', 'smtp_secure', 'email_domain_whitelist_enabled']

// 字符串类型的配置键
const stringConfigKeys = ['turnstile_site_key', 'turnstile_secret_key', 'avatar_api_base', 'smtp_host', 'smtp_username', 'smtp_password', 'smtp_from_email', 'smtp_from_name', 'email_allowed_domains', 'footer_contact_email', 'brand_name', 'brand_subtitle', 'brand_logo_url', 'brand_copyright']
stringConfigKeys.push('popup_announcement', 'popup_promo_image_url', 'popup_promo_package_id', 'ticket_image_lsky_base_url', 'ticket_image_lsky_token', 'ticket_image_lsky_api_version', 'ticket_image_lsky_target_id')
stringConfigKeys.push('system_update_allowed_admin_ids', 'payincus_gift_card_admin_ids')

const jsonConfigKeys = ['invite_generation_costs', 'vip_benefits_config']

const currentSectionKey = computed<SystemSettingsSectionKey>(() => {
  return systemSettingsSections.find(section => section.path === route.path)?.key || 'access'
})

const currentSection = computed(() =>
  systemSettingsSections.find(section => section.key === currentSectionKey.value) || systemSettingsSections[0]
)

const isAccessSection = computed(() => currentSectionKey.value === 'access')
const isBrandSection = computed(() => currentSectionKey.value === 'brand')
const isSecuritySection = computed(() => currentSectionKey.value === 'security')
const isMailSection = computed(() => currentSectionKey.value === 'mail')
const isTicketSection = computed(() => currentSectionKey.value === 'tickets')
const isPopupAnnouncementTab = computed(() => currentSectionKey.value === 'popup')
const isOperationsSection = computed(() => currentSectionKey.value === 'operations')

function hasAtMostTwoDecimalPlaces(value: number): boolean {
  const cents = value * 100
  return Math.abs(cents - Math.round(cents)) < 1e-8
}

function getConfigSaveErrorMessage(err: any): string {
  const details = typeof err?.details === 'string' ? err.details : ''
  if (details && !Object.prototype.hasOwnProperty.call(form.value, details)) {
    return details
  }
  return translateError({ ...err, details: null })
}

onMounted(async () => {
  try {
    const tasks: Promise<void>[] = [loadConfigs()]
    if (isPopupAnnouncementTab.value) {
      tasks.push(loadPopupPromoPackages())
    }
    await Promise.all(tasks)
  } catch (err: unknown) {
    console.error('[SystemConfigView] Failed to load configs:', err)
    const errorMessage = (err && typeof err === 'object' && 'message' in err)
      ? String(err.message)
      : String(err)
    try {
      toast.error((t('admin.system.loadFailed') || '加载配置失败') + ': ' + errorMessage)
    } catch (toastErr) {
      console.error('[SystemConfigView] Failed to show toast:', toastErr)
    }
    loading.value = false
  }
})

watch(isPopupAnnouncementTab, (active) => {
  if (active) {
    void loadPopupPromoPackages()
  }
})

async function loadConfigs() {
  loading.value = true
  try {
    const response = await api.systemConfig.list()
    if (!response || typeof response !== 'object') {
      console.warn('[SystemConfigView] Invalid response format:', response)
      configs.value = []
      return
    }

    configs.value = Array.isArray(response.configs) ? response.configs : []

    // 填充表单
    if (Array.isArray(response.configs)) {
      for (const config of response.configs) {
        if (config && typeof config === 'object' && config.key && config.key in form.value) {
          try {
            if (numericConfigKeys.includes(config.key)) {
              (form.value as any)[config.key] = parseInt(config.value, 10) || 0
            } else if (floatConfigKeys.includes(config.key)) {
              (form.value as any)[config.key] = parseFloat(config.value) || 0
            } else if (booleanConfigKeys.includes(config.key)) {
              (form.value as any)[config.key] = config.value === 'true'
            } else if (stringConfigKeys.includes(config.key)) {
              (form.value as any)[config.key] = config.value || ''
            } else if (jsonConfigKeys.includes(config.key)) {
              try {
                (form.value as any)[config.key] = JSON.parse(config.value || '[]')
              } catch {
                (form.value as any)[config.key] = []
              }
            }
          } catch (configErr) {
            console.error(`[SystemConfigView] Failed to process config key "${config.key}":`, configErr)
          }
        }
      }
    }
  } catch (err: any) {
    console.error('[SystemConfigView] Error loading configs:', err)
    try {
      toast.error((t('admin.system.loadFailed') || '加载配置失败') + ': ' + (err?.message || String(err)))
    } catch (toastErr) {
      console.error('[SystemConfigView] Failed to show toast:', toastErr)
    }
    configs.value = []
  } finally {
    loading.value = false
  }
}

async function loadPopupPromoPackages() {
  if (popupPromoPackagesLoading.value || popupPromoPackagesLoaded.value) {
    return
  }
  popupPromoPackagesLoading.value = true
  try {
    const officialResponse = await api.packages.list({ source: 'official' })
    popupPromoPackages.value = [
      ...(officialResponse.packages || [])
    ].filter(pkg => pkg.active === 1)
    popupPromoPackagesLoaded.value = true
  } catch (err) {
    console.error('[SystemConfigView] Failed to load promo packages:', err)
    popupPromoPackages.value = []
  } finally {
    popupPromoPackagesLoading.value = false
  }
}

const promoPackageOptions = computed(() => {
  const options: Array<{ label: string; value: string; disabled?: boolean }> = [
    {
      label: popupPromoPackagesLoading.value
        ? (t('admin.system.popupAnnouncement.promoPackageLoading') || '加载套餐中...')
        : (t('admin.system.popupAnnouncement.promoPackagePlaceholder') || '请选择促销套餐'),
      value: ''
    }
  ]
  if (!popupPromoPackagesLoading.value && popupPromoPackages.value.length === 0) {
    options.push({
      label: t('admin.system.popupAnnouncement.promoPackageEmpty') || '无可用套餐',
      value: '',
      disabled: true
    })
  }
  for (const pkg of popupPromoPackages.value) {
    options.push({
      label: `#${pkg.id} · ${pkg.name}`,
      value: String(pkg.id)
    })
  }
  return options
})

// 通用保存函数
async function saveConfigGroup(keys: string[], savingRef: { value: boolean }) {
  savingRef.value = true
  try {
    const configsToUpdate = keys.map(key => ({
      key,
      value: jsonConfigKeys.includes(key)
        ? JSON.stringify((form.value as any)[key] || [])
        : typeof (form.value as any)[key] === 'boolean'
          ? ((form.value as any)[key] ? 'true' : 'false')
          : String((form.value as any)[key])
    }))

    await api.systemConfig.update(configsToUpdate)
    toast.success(t('admin.system.saveSuccess'))
    await loadConfigs()
  } catch (err: any) {
    toast.error(t('admin.system.saveFailed') + ': ' + getConfigSaveErrorMessage(err))
  } finally {
    savingRef.value = false
  }
}

// 注册设置保存
const registrationKeys = ['registration_enabled', 'require_invite_code']
async function saveRegistration() {
  await saveConfigGroup(registrationKeys, savingRegistration)
}
const hasRegistrationChanges = computed(() => {
  return registrationKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

function normalizeInviteCostOptions() {
  const options = Array.isArray(form.value.invite_generation_costs)
    ? form.value.invite_generation_costs
    : []
  const optionMap = new Map(options.map((option: any) => [option.resource, option]))
  for (const resource of ['balance', 'points']) {
    if (!optionMap.has(resource)) {
      optionMap.set(resource, { resource, amount: 0, enabled: false })
    }
  }
  form.value.invite_generation_costs = Array.from(optionMap.values()).map((option: any) => ({
    resource: option.resource,
    amount: option.resource === 'points'
      ? Math.max(0, Math.floor(Number(option.amount) || 0))
      : Math.max(0, Math.round((Number(option.amount) || 0) * 100) / 100),
    enabled: option.enabled === true
  }))
}

const invitePricingKeys = ['invite_generation_costs', 'invite_default_expire_days']
async function saveInvitePricing() {
  normalizeInviteCostOptions()
  await saveConfigGroup(invitePricingKeys, savingInvitePricing)
}
const hasInvitePricingChanges = computed(() => {
  return invitePricingKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    const currentValue = jsonConfigKeys.includes(key)
      ? JSON.stringify((form.value as any)[key] || [])
      : String((form.value as any)[key])
    return currentValue !== config.value
  })
})

const affRateKeys = ['aff_commission_rate', 'aff_discount_rate']
async function saveAffRates() {
  const commissionRate = Number(form.value.aff_commission_rate)
  const discountRate = Number(form.value.aff_discount_rate)
  if (
    !Number.isFinite(commissionRate) || commissionRate < 0 || commissionRate > MAX_AFF_COMMISSION_RATE ||
    !Number.isFinite(discountRate) || discountRate < 0 || discountRate > MAX_AFF_DISCOUNT_RATE
  ) {
    toast.error(t('admin.system.affRates.rangeError'))
    return
  }
  await saveConfigGroup(affRateKeys, savingAffRates)
}
const hasAffRateChanges = computed(() => affRateKeys.some(key => {
  const config = configs.value.find(c => c.key === key)
  if (!config) return false
  return String((form.value as any)[key]) !== config.value
}))

const vipBenefitsKeys = ['vip_benefits_config']
function normalizeVipBenefitsForm(): boolean {
  const levels = form.value.vip_benefits_config?.levels
  if (!levels || typeof levels !== 'object') return false
  for (let level = 1; level <= 5; level++) {
    const item = levels[String(level)]
    if (!item) return false
    for (const key of ['orderDiscountPercent', 'extraTrafficPercent', 'resourcePoolBonusPercent'] as const) {
      const value = Number(item[key])
      if (!Number.isFinite(value) || value < 0 || value > 100) return false
      item[key] = Math.round(value * 100) / 100
    }
  }
  return true
}
async function saveVipBenefits() {
  if (!normalizeVipBenefitsForm()) {
    toast.error(t('admin.system.vipBenefits.rangeError'))
    return
  }
  await saveConfigGroup(vipBenefitsKeys, savingVipBenefits)
}
const hasVipBenefitsChanges = computed(() => {
  const config = configs.value.find(item => item.key === 'vip_benefits_config')
  return !!config && JSON.stringify(form.value.vip_benefits_config) !== config.value
})

const popupAnnouncementKeys = ['popup_announcement']
async function savePopupAnnouncement() {
  await saveConfigGroup(popupAnnouncementKeys, savingPopupAnnouncement)
  await configStore.loadPublicConfig(true)
}
const hasPopupAnnouncementChanges = computed(() => {
  return popupAnnouncementKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const popupPromoKeys = ['popup_promo_image_url', 'popup_promo_package_id']
async function savePopupPromo() {
  await saveConfigGroup(popupPromoKeys, savingPopupPromo)
  await configStore.loadPublicConfig(true)
}
const hasPopupPromoChanges = computed(() => {
  return popupPromoKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})
const selectedPopupPromoPackage = computed(() => {
  const packageId = Number(form.value.popup_promo_package_id)
  if (!Number.isInteger(packageId) || packageId <= 0) return null
  return popupPromoPackages.value.find(pkg => pkg.id === packageId) || null
})

// 转移设置保存
const transferKeys = ['transfer_fee']
async function saveTransfer() {
  const fee = Number(form.value.transfer_fee)
  if (!Number.isFinite(fee) || fee < 0 || fee > MAX_TRANSFER_FEE || !hasAtMostTwoDecimalPlaces(fee)) {
    toast.error(t('admin.system.transfer.feeRangeError', { max: MAX_TRANSFER_FEE.toFixed(2) }))
    return
  }
  form.value.transfer_fee = Math.round(fee * 100) / 100
  await saveConfigGroup(transferKeys, savingTransfer)
  await configStore.loadPublicConfig(true)
}
const hasTransferChanges = computed(() => {
  return transferKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const footerLinkKeys = ['footer_contact_email']
async function saveFooterLinks() {
  await saveConfigGroup(footerLinkKeys, savingFooterLinks)
}
const hasFooterLinkChanges = computed(() => {
  return footerLinkKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const brandKeys = ['brand_name', 'brand_subtitle', 'brand_logo_url', 'brand_copyright']
async function saveBrand() {
  await saveConfigGroup(brandKeys, savingBrand)
  await configStore.loadPublicConfig(true)
}
const hasBrandChanges = computed(() => {
  return brandKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const ticketKeys = ['ticket_enabled', 'ticket_auto_close_enabled', 'ticket_auto_close_hours']
async function saveTicket() {
  await saveConfigGroup(ticketKeys, savingTicket)
  await configStore.loadPublicConfig(true)
}
const hasTicketChanges = computed(() => {
  return ticketKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const freeSiteKeys = ['free_site_mode', 'free_site_register_gift_enabled', 'free_site_register_gift_balance', 'free_site_register_gift_points']
async function saveFreeSite() {
  await saveConfigGroup(freeSiteKeys, savingFreeSite)
  await configStore.loadPublicConfig(true)
}
function toggleFreeSiteMode() {
  form.value.free_site_mode = !form.value.free_site_mode
  if (!form.value.free_site_mode) {
    form.value.free_site_register_gift_enabled = false
    form.value.free_site_register_gift_balance = 0
    form.value.free_site_register_gift_points = 0
  }
}
function toggleFreeSiteRegisterGift() {
  if (!form.value.free_site_mode) return
  form.value.free_site_register_gift_enabled = !form.value.free_site_register_gift_enabled
  if (!form.value.free_site_register_gift_enabled) {
    form.value.free_site_register_gift_balance = 0
    form.value.free_site_register_gift_points = 0
  }
}
const hasFreeSiteChanges = computed(() => {
  return freeSiteKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const ticketImageKeys = ['ticket_image_lsky_base_url', 'ticket_image_lsky_token', 'ticket_image_lsky_api_version', 'ticket_image_lsky_target_id']
async function saveTicketImages() {
  await saveConfigGroup(ticketImageKeys, savingTicketImages)
}
const hasTicketImageChanges = computed(() => {
  return ticketImageKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

const platformPermissionKeys = [
  'system_update_allowed_admin_ids',
  'payincus_gift_card_admin_ids'
]
async function savePlatformPermissions() {
  await saveConfigGroup(platformPermissionKeys, savingPlatformPermissions)
}
const hasPlatformPermissionChanges = computed(() => {
  return platformPermissionKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

// 用户默认配额保存
// 注意：不再限制实例配额，已删除 default_quota_instance
// 注意：好友配额已禁用，保留 default_quota_friend 以确保旧数据兼容
const quotaKeys = ['default_quota_host', 'default_quota_friend', 'default_quota_package']
async function saveQuota() {
  await saveConfigGroup(quotaKeys, savingQuota)
}
const hasQuotaChanges = computed(() => {
  return quotaKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

// Turnstile 保存
const turnstileKeys = ['turnstile_enabled', 'turnstile_site_key', 'turnstile_secret_key']
async function saveTurnstile() {
  await saveConfigGroup(turnstileKeys, savingTurnstile)
}
const hasTurnstileChanges = computed(() => {
  return turnstileKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

// 头像 API 保存
const avatarKeys = ['avatar_api_base']
async function saveAvatar() {
  await saveConfigGroup(avatarKeys, savingAvatar)
}
const hasAvatarChanges = computed(() => {
  return avatarKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

// SMTP 保存
const smtpKeys = ['smtp_enabled', 'smtp_host', 'smtp_port', 'smtp_secure', 'smtp_username', 'smtp_password', 'smtp_from_email', 'smtp_from_name']
async function saveSmtp() {
  await saveConfigGroup(smtpKeys, savingSmtp)
}
const hasSmtpChanges = computed(() => {
  return smtpKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

// 邮箱域名白名单保存
const emailDomainKeys = ['email_domain_whitelist_enabled', 'email_allowed_domains']
async function saveEmailDomain() {
  await saveConfigGroup(emailDomainKeys, savingEmailDomain)
}
const hasEmailDomainChanges = computed(() => {
  return emailDomainKeys.some(key => {
    const config = configs.value.find(c => c.key === key)
    if (!config) return false
    return String((form.value as any)[key]) !== config.value
  })
})

async function testSmtpConnection() {
  testingSmtp.value = true
  try {
    const result = await api.systemConfig.testSmtp()
    if (result.success) {
      toast.success(t('admin.system.smtp.testSuccess'))
    } else {
      toast.error(t('admin.system.smtp.testFailed') + ': ' + (result.error || 'Unknown error'))
    }
  } catch (err: any) {
    toast.error(t('admin.system.smtp.testFailed') + ': ' + (err?.message || String(err)))
  } finally {
    testingSmtp.value = false
  }
}

async function sendTestEmail() {
  if (!testEmailTo.value || !testEmailTo.value.includes('@')) {
    toast.error(t('admin.system.smtp.invalidEmail'))
    return
  }

  sendingTestEmail.value = true
  try {
    const result = await api.systemConfig.sendTestEmail(testEmailTo.value)
    if (result.success) {
      toast.success(t('admin.system.smtp.testEmailSent', { email: testEmailTo.value }))
      testEmailTo.value = ''
    } else {
      toast.error(t('admin.system.smtp.testEmailFailed') + ': ' + (result.error || 'Unknown error'))
    }
  } catch (err: any) {
    toast.error(t('admin.system.smtp.testEmailFailed') + ': ' + (err?.message || String(err)))
  } finally {
    sendingTestEmail.value = false
  }
}
</script>

<template>
  <div class="max-w-6xl mx-auto space-y-6 pb-12">
    <!-- Apple HIG Page Header -->
    <PageHeader
      :title="t('admin.system.title') || '系统设置'"
      :description="t(currentSection.descriptionKey) || t('admin.system.description')"
    />

    <!-- Segmented Navigation Bar -->
    <nav class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto no-scrollbar shadow-sm">
      <router-link
        v-for="item in systemSettingsNavigationItems"
        :key="item.path"
        :to="item.path"
        class="shrink-0 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 active:scale-[0.98] select-none"
        :class="route.path === item.path
          ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold border border-[var(--border-color)]/60'
          : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/50 border border-transparent'"
      >
        {{ t(item.labelKey) }}
      </router-link>
    </nav>

    <!-- Skeleton Loading State -->
    <div v-if="loading" class="space-y-6 animate-pulse">
      <Card v-for="i in 3" :key="i" padding="md">
        <div class="flex items-center justify-between border-b border-[var(--border-color)] pb-4 mb-6">
          <div class="space-y-2">
            <div class="h-5 w-40 bg-[var(--bg-surface-soft)] rounded-md"></div>
            <div class="h-3.5 w-64 bg-[var(--bg-surface-soft)] rounded-md"></div>
          </div>
          <div class="h-8 w-20 bg-[var(--bg-surface-soft)] rounded-full"></div>
        </div>
        <div class="space-y-4">
          <div v-for="j in 2" :key="j" class="h-16 bg-[var(--bg-surface-soft)] rounded-xl"></div>
        </div>
      </Card>
    </div>

    <!-- Main Config Container -->
    <div v-else class="space-y-6">
      <!-- 弹窗公告与促销 Tab -->
      <template v-if="isPopupAnnouncementTab">
        <!-- 弹窗公告内容 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.popupAnnouncement.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.popupAnnouncement.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasPopupAnnouncementChanges || savingPopupAnnouncement || form.popup_announcement.length > 5000"
                  :loading="savingPopupAnnouncement"
                  @click="savePopupAnnouncement"
                >
                  {{ savingPopupAnnouncement ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-2">
            <label class="block text-xs font-semibold text-[var(--text-secondary)]">
              {{ t('admin.system.popupAnnouncement.content') }}
            </label>
            <textarea
              v-model="form.popup_announcement"
              rows="8"
              maxlength="5000"
              class="w-full min-h-[180px] rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 resize-y transition-all"
              :placeholder="t('admin.system.popupAnnouncement.placeholder')"
            />
            <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-1 text-xs text-[var(--text-secondary)]">
              <p>{{ t('admin.system.popupAnnouncement.hint') }}</p>
              <p class="tabular-nums font-medium" :class="form.popup_announcement.length > 5000 ? 'text-[var(--error)]' : 'text-[var(--text-tertiary)]'">
                {{ form.popup_announcement.length }}/5000
              </p>
            </div>
          </div>
        </Card>

        <!-- 弹窗促销套餐推荐 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.popupAnnouncement.promoTitle') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.popupAnnouncement.promoDescription') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasPopupPromoChanges || savingPopupPromo || form.popup_promo_image_url.length > 1000"
                  :loading="savingPopupPromo"
                  @click="savePopupPromo"
                >
                  {{ savingPopupPromo ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid gap-6 lg:grid-cols-[minmax(0,1fr)_340px]">
            <div class="space-y-5">
              <div class="space-y-1.5">
                <Input
                  v-model="form.popup_promo_image_url"
                  type="url"
                  :maxlength="1000"
                  class="font-mono text-xs"
                  :label="t('admin.system.popupAnnouncement.promoImageUrl')"
                  :placeholder="t('admin.system.popupAnnouncement.promoImagePlaceholder')"
                />
                <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-1 text-xs text-[var(--text-secondary)]">
                  <p>{{ t('admin.system.popupAnnouncement.promoImageHint') }}</p>
                  <p class="tabular-nums font-medium" :class="form.popup_promo_image_url.length > 1000 ? 'text-[var(--error)]' : 'text-[var(--text-tertiary)]'">
                    {{ form.popup_promo_image_url.length }}/1000
                  </p>
                </div>
              </div>

              <div class="space-y-1.5">
                <Select
                  v-model="form.popup_promo_package_id"
                  :options="promoPackageOptions"
                  :disabled="popupPromoPackagesLoading"
                  :label="t('admin.system.popupAnnouncement.promoPackage')"
                  :hint="t('admin.system.popupAnnouncement.promoPackageHint')"
                />
              </div>
            </div>

            <!-- Apple Style Live Preview Card -->
            <div class="overflow-hidden rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-sm flex flex-col">
              <div class="h-48 flex items-center justify-center overflow-hidden bg-[var(--bg-surface-soft)] p-4">
                <img
                  v-if="form.popup_promo_image_url"
                  :src="form.popup_promo_image_url"
                  :alt="selectedPopupPromoPackage?.name || t('admin.system.popupAnnouncement.promoPreview')"
                  class="max-h-full max-w-full object-contain rounded-lg"
                />
                <div v-else class="px-6 text-center text-xs text-[var(--text-tertiary)]">
                  {{ t('admin.system.popupAnnouncement.promoPreviewEmpty') }}
                </div>
              </div>
              <div class="border-t border-[var(--border-color)] p-4 space-y-3 mt-auto bg-[var(--bg-surface)]">
                <div class="flex items-center justify-between">
                  <Badge variant="info" size="sm">
                    {{ t('admin.system.popupAnnouncement.promoPreview') }}
                  </Badge>
                </div>
                <p class="text-sm font-semibold text-[var(--text-primary)] truncate">
                  {{ selectedPopupPromoPackage?.name || t('admin.system.popupAnnouncement.promoNoPackage') }}
                </p>
                <Button variant="primary" size="sm" block disabled class="opacity-80">
                  {{ t('popupAnnouncement.buyNow', { name: selectedPopupPromoPackage?.name || t('admin.system.popupAnnouncement.promoPackageFallback') }) }}
                </Button>
              </div>
            </div>
          </div>
        </Card>
      </template>

      <!-- 运营配置 Tab -->
      <template v-else-if="isOperationsSection">
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  平台权限白名单
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  配置高风险后台能力允许操作的管理员 UID。多个 UID 用英文逗号分隔，环境变量会作为兜底白名单合并生效。
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasPlatformPermissionChanges || savingPlatformPermissions"
                  :loading="savingPlatformPermissions"
                  @click="savePlatformPermissions"
                >
                  {{ savingPlatformPermissions ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <Input
              v-model="form.system_update_allowed_admin_ids"
              type="text"
              class="font-mono"
              label="OTA 更新管理员 UID"
              hint="控制版本检查后的在线更新、回滚等高风险操作。留空时仅 admin 用户名可操作。"
              placeholder="1,2,3"
            />
            <Input
              v-model="form.payincus_gift_card_admin_ids"
              type="text"
              class="font-mono"
              label="礼品卡管理员 UID"
              hint="留空时所有管理员可操作；配置后仅白名单内的管理员可生成、禁用或删除礼品卡。"
              placeholder="1,2,3"
            />
          </div>
        </Card>
      </template>

      <!-- 访问与注册 Tab -->
      <template v-else-if="isAccessSection">
        <!-- 注册设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.registration') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.registrationDesc') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasRegistrationChanges || savingRegistration"
                  :loading="savingRegistration"
                  @click="saveRegistration"
                >
                  {{ savingRegistration ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-4">
            <!-- 开放注册开关 -->
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">
                  {{ t('admin.system.registrationEnabled') }}
                </label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.registrationEnabledDesc') }}
                </p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.registration_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.registrationClosed') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.registration_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.registration_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.registration_enabled = !form.registration_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.registration_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.registration_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.registrationOpen') }}
                </span>
              </div>
            </div>

            <!-- 强制邀请码开关 -->
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">
                  {{ t('admin.system.requireInviteCode') }}
                </label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.requireInviteCodeDesc') }}
                </p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.require_invite_code ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.openRegistration') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.require_invite_code"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.require_invite_code ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.require_invite_code = !form.require_invite_code"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.require_invite_code ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.require_invite_code ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.inviteOnly') }}
                </span>
              </div>
            </div>
          </div>
        </Card>

        <!-- 推广返利比例 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.affRates.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.affRates.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasAffRateChanges || savingAffRates"
                  :loading="savingAffRates"
                  @click="saveAffRates"
                >
                  {{ savingAffRates ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div class="space-y-1.5">
              <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('admin.system.affRates.commissionRate') }}
              </label>
              <input
                v-model.number="form.aff_commission_rate"
                type="number"
                min="0"
                :max="MAX_AFF_COMMISSION_RATE"
                step="0.01"
                class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
              />
              <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.affRates.commissionRateHint') }}</p>
            </div>

            <div class="space-y-1.5">
              <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('admin.system.affRates.discountRate') }}
              </label>
              <input
                v-model.number="form.aff_discount_rate"
                type="number"
                min="0"
                :max="MAX_AFF_DISCOUNT_RATE"
                step="0.01"
                class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
              />
              <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.affRates.discountRateHint') }}</p>
            </div>
          </div>
        </Card>

        <!-- VIP 会员权益配置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.vipBenefits.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.vipBenefits.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasVipBenefitsChanges || savingVipBenefits"
                  :loading="savingVipBenefits"
                  @click="saveVipBenefits"
                >
                  {{ savingVipBenefits ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-3">
            <div
              v-for="level in 5"
              :key="level"
              class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 grid grid-cols-1 md:grid-cols-4 gap-4 items-center"
            >
              <div class="flex items-center gap-2">
                <Badge variant="info" size="md">VIP {{ level }}</Badge>
              </div>
              <div class="space-y-1">
                <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                  {{ t('admin.system.vipBenefits.orderDiscount') }}
                </label>
                <div class="relative flex items-center">
                  <input
                    v-model.number="form.vip_benefits_config.levels[String(level)].orderDiscountPercent"
                    type="number"
                    min="0"
                    max="100"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 pr-8 tabular-nums transition-all"
                  />
                  <span class="absolute right-3 text-xs text-[var(--text-tertiary)] select-none pointer-events-none">%</span>
                </div>
              </div>
              <div class="space-y-1">
                <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                  {{ t('admin.system.vipBenefits.extraTraffic') }}
                </label>
                <div class="relative flex items-center">
                  <input
                    v-model.number="form.vip_benefits_config.levels[String(level)].extraTrafficPercent"
                    type="number"
                    min="0"
                    max="100"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 pr-8 tabular-nums transition-all"
                  />
                  <span class="absolute right-3 text-xs text-[var(--text-tertiary)] select-none pointer-events-none">%</span>
                </div>
              </div>
              <div class="space-y-1">
                <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                  {{ t('admin.system.vipBenefits.resourcePoolBonus') }}
                </label>
                <div class="relative flex items-center">
                  <input
                    v-model.number="form.vip_benefits_config.levels[String(level)].resourcePoolBonusPercent"
                    type="number"
                    min="0"
                    max="100"
                    step="0.01"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 pr-8 tabular-nums transition-all"
                  />
                  <span class="absolute right-3 text-xs text-[var(--text-tertiary)] select-none pointer-events-none">%</span>
                </div>
              </div>
            </div>
          </div>
          <p class="mt-4 text-xs text-[var(--text-tertiary)]">{{ t('admin.system.vipBenefits.arbitrationHint') }}</p>
        </Card>

        <!-- 邀请码生成定价 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  邀请码生成定价
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  配置普通用户生成一个邀请码需要消耗的资源。当前支持余额和积分，价格保存为可扩展的成本项列表。
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasInvitePricingChanges || savingInvitePricing"
                  :loading="savingInvitePricing"
                  @click="saveInvitePricing"
                >
                  {{ savingInvitePricing ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-5">
            <div class="grid gap-4 lg:grid-cols-2">
              <div
                v-for="option in form.invite_generation_costs"
                :key="option.resource"
                class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 space-y-4"
              >
                <div class="flex items-center justify-between gap-4">
                  <div>
                    <label class="text-sm font-semibold text-[var(--text-primary)]">
                      {{ option.resource === 'balance' ? '余额生成' : option.resource === 'points' ? '积分生成' : option.resource }}
                    </label>
                    <p class="mt-0.5 text-xs text-[var(--text-secondary)]">
                      {{ option.resource === 'balance' ? '用户每生成 1 个邀请码扣除账户余额' : '用户每生成 1 个邀请码扣除积分' }}
                    </p>
                  </div>
                  <button
                    type="button"
                    role="switch"
                    :aria-checked="option.enabled"
                    class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                    :class="option.enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                    @click="option.enabled = !option.enabled"
                  >
                    <span
                      class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                      :class="option.enabled ? 'translate-x-5' : 'translate-x-0.5'"
                    />
                  </button>
                </div>

                <div>
                  <label class="block text-xs font-semibold text-[var(--text-secondary)] mb-1.5">
                    {{ option.resource === 'balance' ? '单个价格（元）' : '单个价格（积分）' }}
                  </label>
                  <div class="relative max-w-xs flex items-center">
                    <span v-if="option.resource === 'balance'" class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
                    <input
                      v-model.number="option.amount"
                      type="number"
                      min="0"
                      :step="option.resource === 'balance' ? '0.01' : '1'"
                      class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                      :class="option.resource === 'balance' ? 'pl-8 pr-3.5' : 'px-3.5'"
                    />
                  </div>
                </div>
              </div>
            </div>

            <div class="space-y-1.5 max-w-sm pt-2">
              <label class="block text-xs font-semibold text-[var(--text-secondary)]">默认有效期</label>
              <div class="relative flex items-center">
                <input
                  v-model.number="form.invite_default_expire_days"
                  type="number"
                  min="0"
                  max="3650"
                  step="1"
                  class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 pr-12 tabular-nums transition-all"
                />
                <span class="absolute right-3.5 text-xs text-[var(--text-tertiary)] select-none pointer-events-none">天</span>
              </div>
              <p class="text-xs text-[var(--text-tertiary)]">0 表示用户生成的邀请码永不过期。</p>
            </div>
          </div>
        </Card>

        <!-- 转移设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.transfer.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.transfer.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasTransferChanges || savingTransfer"
                  :loading="savingTransfer"
                  @click="saveTransfer"
                >
                  {{ savingTransfer ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-1.5 max-w-sm">
            <label class="block text-xs font-semibold text-[var(--text-secondary)]">
              {{ t('admin.system.transfer.feeLabel') }}
            </label>
            <div class="relative flex items-center">
              <span class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
              <input
                v-model.number="form.transfer_fee"
                type="number"
                min="0"
                :max="MAX_TRANSFER_FEE"
                step="0.01"
                class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] pl-8 pr-12 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                placeholder="0.00"
              />
              <span class="absolute right-3.5 text-xs text-[var(--text-tertiary)] pointer-events-none select-none">
                {{ t('admin.system.transfer.feeUnit') }}
              </span>
            </div>
            <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.transfer.feeDesc') }}</p>
          </div>
        </Card>

        <!-- 免单模式与注册赠金 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.freeSite.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.freeSite.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasFreeSiteChanges || savingFreeSite"
                  :loading="savingFreeSite"
                  @click="saveFreeSite"
                >
                  {{ savingFreeSite ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-4">
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.freeSite.enable') }}</label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.freeSite.enableDesc') }}</p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.free_site_mode ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.freeSite.disabled') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.free_site_mode"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.free_site_mode ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="toggleFreeSiteMode"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.free_site_mode ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.free_site_mode ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.freeSite.enabled') }}
                </span>
              </div>
            </div>

            <div
              class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 space-y-4 transition-all duration-300"
              :class="form.free_site_mode ? 'opacity-100' : 'opacity-50 pointer-events-none'"
            >
              <div class="flex items-center justify-between">
                <div class="flex-1 pr-4">
                  <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.freeSite.registerGift') }}</label>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.freeSite.registerGiftDesc') }}</p>
                </div>
                <div class="flex items-center gap-3 shrink-0">
                  <span class="text-xs font-medium" :class="form.free_site_register_gift_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                    {{ t('admin.system.freeSite.giftDisabled') }}
                  </span>
                  <button
                    type="button"
                    role="switch"
                    :aria-checked="form.free_site_register_gift_enabled && form.free_site_mode"
                    :disabled="!form.free_site_mode"
                    class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                    :class="form.free_site_mode && form.free_site_register_gift_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                    @click="toggleFreeSiteRegisterGift"
                  >
                    <span
                      class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                      :class="form.free_site_mode && form.free_site_register_gift_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                    />
                  </button>
                  <span class="text-xs font-medium" :class="form.free_site_mode && form.free_site_register_gift_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                    {{ t('admin.system.freeSite.giftEnabled') }}
                  </span>
                </div>
              </div>

              <div class="grid grid-cols-1 md:grid-cols-2 gap-4 pt-2">
                <div class="space-y-1.5">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                    {{ t('admin.system.freeSite.giftBalance') }}
                  </label>
                  <div class="relative flex items-center">
                    <span class="absolute left-3.5 text-sm font-medium text-[var(--text-tertiary)] pointer-events-none select-none">¥</span>
                    <input
                      v-model.number="form.free_site_register_gift_balance"
                      type="number"
                      min="0"
                      step="0.01"
                      class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] pl-8 pr-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                      placeholder="0.00"
                      :disabled="!form.free_site_mode || !form.free_site_register_gift_enabled"
                    />
                  </div>
                  <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.freeSite.giftBalanceDesc') }}</p>
                </div>

                <div class="space-y-1.5">
                  <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                    {{ t('admin.system.freeSite.giftPoints') }}
                  </label>
                  <div class="relative flex items-center">
                    <input
                      v-model.number="form.free_site_register_gift_points"
                      type="number"
                      min="0"
                      step="1"
                      class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                      placeholder="0"
                      :disabled="!form.free_site_mode || !form.free_site_register_gift_enabled"
                    />
                  </div>
                  <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.freeSite.giftPointsDesc') }}</p>
                </div>
              </div>

              <p v-if="!form.free_site_mode" class="text-xs text-[var(--text-tertiary)]">
                {{ t('admin.system.freeSite.giftRequiresFreeSite') }}
              </p>
            </div>
          </div>
        </Card>

        <!-- 用户默认配额 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.defaultQuota') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.defaultQuotaDesc') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasQuotaChanges || savingQuota"
                  :loading="savingQuota"
                  @click="saveQuota"
                >
                  {{ savingQuota ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
            <div v-for="(meta, key) in configMeta" :key="key" class="space-y-1.5">
              <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                {{ meta.label }}
              </label>
              <div class="relative flex items-center">
                <input
                  v-model.number="(form as any)[key]"
                  type="number"
                  min="0"
                  class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 pr-12 tabular-nums transition-all"
                  :placeholder="meta.label"
                />
                <span class="absolute right-3.5 text-xs text-[var(--text-tertiary)] select-none pointer-events-none">
                  {{ meta.unit }}
                </span>
              </div>
              <p class="text-xs text-[var(--text-tertiary)]">{{ meta.description }}</p>
            </div>
          </div>
        </Card>

        <!-- 补充说明卡片 -->
        <Card padding="md">
          <div class="flex items-center gap-2 mb-4">
            <Info class="w-4 h-4 text-[var(--accent)]" />
            <h3 class="text-base font-semibold text-[var(--text-primary)]">
              {{ t('admin.system.notes') }}
            </h3>
          </div>
          <div class="space-y-3 text-sm text-[var(--text-secondary)]">
            <div class="flex items-start gap-2.5">
              <span class="mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full bg-[var(--accent)]" />
              <span>{{ t('admin.system.note1') }}</span>
            </div>
            <div class="flex items-start gap-2.5">
              <span class="mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full bg-[var(--accent)]" />
              <span>{{ t('admin.system.note2') }}</span>
            </div>
            <div class="flex items-start gap-2.5">
              <span class="mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full bg-[var(--accent)]" />
              <span>{{ t('admin.system.note3') }}</span>
            </div>
            <div class="flex items-start gap-2.5">
              <span class="mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full bg-[var(--accent)]" />
              <span>{{ t('admin.system.note4') }}</span>
            </div>
          </div>
        </Card>
      </template>

      <!-- 品牌与外观 Tab -->
      <template v-else-if="isBrandSection">
        <!-- 底部联系方式 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.footerLinks.title') || '底部联系方式' }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.footerLinks.description') || '配置侧边栏底部的邮箱按钮' }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasFooterLinkChanges || savingFooterLinks"
                  :loading="savingFooterLinks"
                  @click="saveFooterLinks"
                >
                  {{ savingFooterLinks ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <Input
              v-model="form.footer_contact_email"
              type="text"
              class="font-mono"
              :label="t('admin.system.footerLinks.email') || '联系邮箱'"
              :hint="t('admin.system.footerLinks.emailDesc') || '留空则隐藏邮箱按钮；支持填写邮箱地址或完整 mailto: 链接。'"
              :placeholder="t('admin.system.footerLinks.emailPlaceholder') || 'support@example.com 或 mailto:support@example.com'"
            />
          </div>
        </Card>

        <!-- 品牌设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.brand.title') || '品牌设置' }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.brand.description') || '配置站点顶部、登录页、SEO 等位置使用的系统名称与 Logo。留空则使用默认值。' }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasBrandChanges || savingBrand"
                  :loading="savingBrand"
                  @click="saveBrand"
                >
                  {{ savingBrand ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <Input
              v-model="form.brand_name"
              type="text"
              :label="t('admin.system.brand.name') || '系统名称'"
              placeholder="Incudal"
            />
            <Input
              v-model="form.brand_subtitle"
              type="text"
              :label="t('admin.system.brand.subtitle') || '网站副标题'"
              :hint="t('admin.system.brand.subtitleDesc') || '显示在公开站头部/底部、浏览器默认标题和 SEO 默认描述中。'"
              :placeholder="t('admin.system.brand.subtitlePlaceholder') || '基于 Incus 的低价 NAT VPS'"
            />
            <Input
              v-model="form.brand_logo_url"
              type="text"
              class="font-mono"
              :label="t('admin.system.brand.logo') || 'Logo 地址'"
              :hint="t('admin.system.brand.logoDesc') || '支持 http(s) 图片地址或站点内绝对路径。'"
              placeholder="/logo.svg"
            />
            <Input
              v-model="form.brand_copyright"
              type="text"
              class="md:col-span-2"
              :label="t('admin.system.brand.copyright') || '版权所有'"
              :hint="t('admin.system.brand.copyrightDesc') || '显示在页面底部的版权所有文案，留空则使用默认值。'"
              placeholder="版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。"
            />
          </div>
        </Card>

        <!-- 头像 API 设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.avatar.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.avatar.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasAvatarChanges || savingAvatar"
                  :loading="savingAvatar"
                  @click="saveAvatar"
                >
                  {{ savingAvatar ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="max-w-xl space-y-4">
            <Input
              v-model="form.avatar_api_base"
              type="text"
              class="font-mono"
              :label="t('admin.system.avatar.apiBase')"
              :hint="t('admin.system.avatar.apiBaseDesc')"
              placeholder="https://api.dicebear.com/9.x"
            />

            <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 flex items-center justify-between text-xs text-[var(--text-secondary)]">
              <span>{{ t('admin.system.avatar.helpText') }}</span>
              <a
                href="https://www.dicebear.com/how-to-use/http-api"
                target="_blank"
                rel="noopener noreferrer"
                class="inline-flex items-center gap-1 font-medium text-[var(--accent)] hover:underline ml-2"
              >
                DiceBear HTTP API
                <ExternalLink class="w-3.5 h-3.5" />
              </a>
            </div>
          </div>
        </Card>
      </template>

      <!-- 安全验证 Tab -->
      <template v-else-if="isSecuritySection">
        <!-- Turnstile 设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.turnstile.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.turnstile.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasTurnstileChanges || savingTurnstile"
                  :loading="savingTurnstile"
                  @click="saveTurnstile"
                >
                  {{ savingTurnstile ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-5">
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.turnstile.enable') }}</label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.turnstile.enableDesc') }}</p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.turnstile_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.turnstile.disabled') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.turnstile_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.turnstile_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.turnstile_enabled = !form.turnstile_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.turnstile_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.turnstile_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.turnstile.enabled') }}
                </span>
              </div>
            </div>

            <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
              <Input
                v-model="form.turnstile_site_key"
                type="text"
                class="font-mono"
                :label="t('admin.system.turnstile.siteKey')"
                :hint="t('admin.system.turnstile.siteKeyDesc')"
                :placeholder="t('admin.system.turnstile.siteKeyPlaceholder')"
              />
              <Input
                v-model="form.turnstile_secret_key"
                type="password"
                class="font-mono"
                :label="t('admin.system.turnstile.secretKey')"
                :hint="t('admin.system.turnstile.secretKeyDesc')"
                :placeholder="t('admin.system.turnstile.secretKeyPlaceholder')"
              />
            </div>

            <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 flex items-center justify-between text-xs text-[var(--text-secondary)]">
              <span>{{ t('admin.system.turnstile.helpText') }}</span>
              <a
                href="https://dash.cloudflare.com/?to=/:account/turnstile"
                target="_blank"
                rel="noopener noreferrer"
                class="inline-flex items-center gap-1 font-medium text-[var(--accent)] hover:underline ml-2"
              >
                Cloudflare Turnstile Dashboard
                <ExternalLink class="w-3.5 h-3.5" />
              </a>
            </div>
          </div>
        </Card>

        <!-- 邮箱域名白名单 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.emailDomain.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.emailDomain.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasEmailDomainChanges || savingEmailDomain"
                  :loading="savingEmailDomain"
                  @click="saveEmailDomain"
                >
                  {{ savingEmailDomain ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-5">
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.emailDomain.enable') }}</label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.emailDomain.enableDesc') }}</p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.email_domain_whitelist_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.emailDomain.disabled') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.email_domain_whitelist_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.email_domain_whitelist_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.email_domain_whitelist_enabled = !form.email_domain_whitelist_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.email_domain_whitelist_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.email_domain_whitelist_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.emailDomain.enabled') }}
                </span>
              </div>
            </div>

            <div class="space-y-1.5">
              <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('admin.system.emailDomain.allowedDomains') }}
              </label>
              <textarea
                v-model="form.email_allowed_domains"
                class="w-full min-h-[120px] rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-sm font-mono text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 disabled:opacity-50 disabled:cursor-not-allowed transition-all"
                :placeholder="t('admin.system.emailDomain.allowedDomainsPlaceholder')"
                :disabled="!form.email_domain_whitelist_enabled"
              />
              <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.emailDomain.allowedDomainsDesc') }}</p>
            </div>

            <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 text-xs text-[var(--text-secondary)]">
              {{ t('admin.system.emailDomain.helpText') }}
            </div>
          </div>
        </Card>
      </template>

      <!-- 邮件服务 Tab -->
      <template v-else-if="isMailSection">
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.smtp.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.smtp.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasSmtpChanges || savingSmtp"
                  :loading="savingSmtp"
                  @click="saveSmtp"
                >
                  {{ savingSmtp ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-5">
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.smtp.enable') }}</label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.smtp.enableDesc') }}</p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.smtp_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.smtp.disabled') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.smtp_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.smtp_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.smtp_enabled = !form.smtp_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.smtp_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.smtp_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.smtp.enabled') }}
                </span>
              </div>
            </div>

            <!-- Server Settings -->
            <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
              <Input
                v-model="form.smtp_host"
                type="text"
                class="font-mono"
                :label="t('admin.system.smtp.host')"
                :placeholder="t('admin.system.smtp.hostPlaceholder')"
              />
              <div class="space-y-1.5">
                <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                  {{ t('admin.system.smtp.port') }}
                </label>
                <input
                  v-model.number="form.smtp_port"
                  type="number"
                  class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] px-3.5 py-2.5 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 tabular-nums transition-all"
                  placeholder="587"
                />
              </div>
            </div>

            <!-- SSL/TLS Toggle row -->
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div>
                <label class="text-sm font-semibold text-[var(--text-primary)]">
                  {{ t('admin.system.smtp.secure') }}
                </label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.smtp.secureHint') }}
                </p>
              </div>
              <button
                type="button"
                role="switch"
                :aria-checked="form.smtp_secure"
                class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                :class="form.smtp_secure ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                @click="form.smtp_secure = !form.smtp_secure"
              >
                <span
                  class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                  :class="form.smtp_secure ? 'translate-x-5' : 'translate-x-0.5'"
                />
              </button>
            </div>

            <!-- Auth settings -->
            <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
              <Input
                v-model="form.smtp_username"
                type="text"
                :label="t('admin.system.smtp.username')"
                :placeholder="t('admin.system.smtp.usernamePlaceholder')"
              />
              <Input
                v-model="form.smtp_password"
                type="password"
                :label="t('admin.system.smtp.password')"
                :placeholder="t('admin.system.smtp.passwordPlaceholder')"
              />
              <Input
                v-model="form.smtp_from_email"
                type="email"
                :label="t('admin.system.smtp.fromEmail')"
                :placeholder="t('admin.system.smtp.fromEmailPlaceholder')"
              />
              <Input
                v-model="form.smtp_from_name"
                type="text"
                :label="t('admin.system.smtp.fromName')"
                placeholder="Incudal"
              />
            </div>

            <!-- Test Connection & Send Test Email -->
            <div class="space-y-4 pt-2">
              <div class="flex justify-end">
                <Button
                  variant="secondary"
                  size="sm"
                  :disabled="testingSmtp || !form.smtp_host || !form.smtp_username"
                  :loading="testingSmtp"
                  @click="testSmtpConnection"
                >
                  <template #icon>
                    <Radio class="w-4 h-4" />
                  </template>
                  {{ testingSmtp ? t('admin.system.smtp.testing') : t('admin.system.smtp.testConnection') }}
                </Button>
              </div>

              <div class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 space-y-3">
                <div>
                  <label class="block text-sm font-semibold text-[var(--text-primary)]">
                    {{ t('admin.system.smtp.sendTestEmail') }}
                  </label>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('admin.system.smtp.sendTestEmailDesc') }}
                  </p>
                </div>
                <div class="flex flex-col sm:flex-row gap-2.5">
                  <Input
                    v-model="testEmailTo"
                    type="email"
                    class="flex-1"
                    :placeholder="t('admin.system.smtp.testEmailPlaceholder')"
                    :disabled="sendingTestEmail"
                  />
                  <Button
                    variant="secondary"
                    size="md"
                    class="shrink-0"
                    :disabled="sendingTestEmail || !testEmailTo || !form.smtp_host || !form.smtp_username"
                    :loading="sendingTestEmail"
                    @click="sendTestEmail"
                  >
                    <template #icon>
                      <Send class="w-4 h-4" />
                    </template>
                    {{ sendingTestEmail ? t('admin.system.smtp.sending') : t('admin.system.smtp.send') }}
                  </Button>
                </div>
              </div>
            </div>

            <div class="p-3.5 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 text-xs text-[var(--text-secondary)]">
              {{ t('admin.system.smtp.helpText') }}
            </div>
          </div>
        </Card>
      </template>

      <!-- 工单与附件 Tab -->
      <template v-else-if="isTicketSection">
        <!-- 工单系统设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.ticket.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.ticket.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasTicketChanges || savingTicket"
                  :loading="savingTicket"
                  @click="saveTicket"
                >
                  {{ savingTicket ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="space-y-4">
            <!-- 开启工单系统开关 -->
            <div class="flex items-center justify-between p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60">
              <div class="flex-1 pr-4">
                <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.ticket.enable') }}</label>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.ticket.enableDesc') }}</p>
              </div>
              <div class="flex items-center gap-3 shrink-0">
                <span class="text-xs font-medium" :class="form.ticket_enabled ? 'text-[var(--text-secondary)]' : 'text-[var(--text-primary)]'">
                  {{ t('admin.system.ticket.disabled') }}
                </span>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.ticket_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.ticket_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.ticket_enabled = !form.ticket_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.ticket_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
                <span class="text-xs font-medium" :class="form.ticket_enabled ? 'text-[var(--text-primary)]' : 'text-[var(--text-secondary)]'">
                  {{ t('admin.system.ticket.enabled') }}
                </span>
              </div>
            </div>

            <!-- 工单自动关闭子设置 -->
            <div
              class="p-4 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]/60 space-y-4 transition-all duration-300"
              :class="form.ticket_enabled ? 'opacity-100' : 'opacity-50 pointer-events-none'"
            >
              <div class="flex items-center justify-between">
                <div class="flex-1 pr-4">
                  <label class="text-sm font-semibold text-[var(--text-primary)]">{{ t('admin.system.ticket.autoClose') }}</label>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">{{ t('admin.system.ticket.autoCloseDesc') }}</p>
                </div>
                <button
                  type="button"
                  role="switch"
                  :aria-checked="form.ticket_auto_close_enabled"
                  class="relative inline-flex h-7 w-12 shrink-0 cursor-pointer rounded-full border border-transparent transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)] active:scale-[0.96]"
                  :class="form.ticket_auto_close_enabled ? 'bg-[var(--accent)]' : 'bg-[var(--bg-tertiary)] border-[var(--border-color)]'"
                  @click="form.ticket_auto_close_enabled = !form.ticket_auto_close_enabled"
                >
                  <span
                    class="pointer-events-none inline-block h-6 w-6 transform rounded-full bg-white shadow-sm ring-0 transition-transform duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)]"
                    :class="form.ticket_auto_close_enabled ? 'translate-x-5' : 'translate-x-0.5'"
                  />
                </button>
              </div>

              <div class="space-y-1.5 max-w-sm">
                <label class="block text-xs font-semibold text-[var(--text-secondary)]">
                  {{ t('admin.system.ticket.autoCloseHours') }}
                </label>
                <div class="relative flex items-center">
                  <input
                    v-model.number="form.ticket_auto_close_hours"
                    type="number"
                    min="1"
                    step="1"
                    class="w-full rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] px-3.5 py-2 text-sm text-[var(--text-primary)] placeholder-[var(--text-tertiary)] focus:border-[var(--accent)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]/30 pr-12 tabular-nums transition-all disabled:opacity-50 disabled:cursor-not-allowed"
                    :disabled="!form.ticket_auto_close_enabled"
                  />
                  <span class="absolute right-3.5 text-xs text-[var(--text-tertiary)] select-none pointer-events-none">小时</span>
                </div>
                <p class="text-xs text-[var(--text-tertiary)]">{{ t('admin.system.ticket.autoCloseHoursDesc') }}</p>
              </div>
            </div>
          </div>
        </Card>

        <!-- 工单图片图床设置 -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 w-full">
              <div class="min-w-0 flex-1">
                <h3 class="font-semibold tracking-tight text-base sm:text-lg text-[var(--text-primary)]">
                  {{ t('admin.system.ticketImages.title') }}
                </h3>
                <p class="text-xs sm:text-sm text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.system.ticketImages.description') }}
                </p>
              </div>
              <div class="flex items-center gap-2 shrink-0 self-end sm:self-auto">
                <Button
                  variant="primary"
                  size="sm"
                  :disabled="!hasTicketImageChanges || savingTicketImages"
                  :loading="savingTicketImages"
                  @click="saveTicketImages"
                >
                  {{ savingTicketImages ? t('admin.system.saving') : t('admin.system.save') }}
                </Button>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <Input
              v-model="form.ticket_image_lsky_base_url"
              type="text"
              class="font-mono"
              :label="t('admin.system.ticketImages.baseUrl')"
              :hint="t('admin.system.ticketImages.baseUrlDesc')"
              :placeholder="t('admin.system.ticketImages.baseUrlPlaceholder')"
            />
            <Input
              v-model="form.ticket_image_lsky_token"
              type="password"
              class="font-mono"
              :label="t('admin.system.ticketImages.token')"
              :hint="t('admin.system.ticketImages.tokenDesc')"
              :placeholder="t('admin.system.ticketImages.tokenPlaceholder')"
            />
            <Select
              v-model="form.ticket_image_lsky_api_version"
              :options="[
                { label: 'v1', value: 'v1' },
                { label: 'v2', value: 'v2' }
              ]"
              :label="t('admin.system.ticketImages.apiVersion')"
              :hint="t('admin.system.ticketImages.apiVersionDesc')"
            />
            <Input
              v-model="form.ticket_image_lsky_target_id"
              type="text"
              class="font-mono"
              :label="t('admin.system.ticketImages.targetId')"
              :hint="t('admin.system.ticketImages.targetIdDesc')"
              :placeholder="t('admin.system.ticketImages.targetIdPlaceholder')"
            />
          </div>
        </Card>
      </template>
    </div>
  </div>
</template>

<style scoped>
.no-scrollbar::-webkit-scrollbar {
  display: none;
}
.no-scrollbar {
  -ms-overflow-style: none;
  scrollbar-width: none;
}
</style>
