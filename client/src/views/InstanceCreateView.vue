<script setup lang="ts">
/**
 * 实例创建页面
 * 已拆分为以下子组件：
 * - RegionSelector: 地区选择
 * - PackageSelector: 套餐选择
 * - ResourceSliders: 资源配置滑块
 * - HostSelector: 宿主机选择
 * - ImageSelector: 镜像选择
 * - SSHKeySelector: SSH密钥选择
 */

// 为 KeepAlive exclude 匹配定义组件名称
defineOptions({
  name: 'InstanceCreateView'
})

import { ref, onMounted, computed, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { useAuthStore } from '@/stores/auth'
import { useToast } from '@/stores/toast'
import { useThemeStore } from '@/stores/theme'
import { useConfigStore } from '@/stores/config'
import RegionSelector from '@/components/instance/RegionSelector.vue'
import type { Region } from '@/components/instance/RegionSelector.vue'
import PackageSelector from '@/components/instance/PackageSelector.vue'
import PlanSelector from '@/components/instance/PlanSelector.vue'
import GpuSpecification from '@/components/instance/GpuSpecification.vue'
import type { GpuConfiguration } from '@/types/api'
import ResourceSliders from '@/components/instance/ResourceSliders.vue'
import HostSelector from '@/components/instance/HostSelector.vue'
import ImageSelector from '@/components/instance/ImageSelector.vue'
import SSHKeySelector from '@/components/instance/SSHKeySelector.vue'
import InitCommandSelector from '@/components/extensions/InitCommandSelector.vue'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import { instancesPath, profilePath } from '@/utils/app-paths'
import type { Package, UserQuota, SshKey, AvailableHost, CreateInstanceRequest } from '@/types/api'
import { normalizePackageSourceQuery, toPackageSourceRequest, type PackageSource } from '@/utils/publicCatalog'
import { validateName as validateInstanceName } from '@/utils/validation'
import { translateError } from '@/utils/errorHandler'
import { freeSiteCopy, getFreeSiteBillingCycleLabel } from '@/utils/freeSiteFun'
import { Button, Card, Input, Select, Badge, PageHeader } from '@/components/ui'
import {
  Loader2,
  RotateCw,
  KeyRound,
  Eye,
  EyeOff,
  ReceiptText,
  ShieldCheck,
  Shield,
  ShoppingBag,
  Check,
  CheckCircle2,
  XCircle,
  AlertCircle,
  AlertTriangle,
  Info,
  Tag
} from 'lucide-vue-next'

// 保留 Select 符号引用以满足架构组件库完整性
void Select

interface ImageOption {
  value: string
  label: string
  icon: string | null
}

interface PackagePlan {
  gpuShared?: boolean | null
  gpuDeviceIds?: number[]
  gpuConfigurations?: GpuConfiguration[]
  id: number
  name: string
  description: string | null
  cpu: number
  memory: number
  disk: number
  portLimit: number
  snapshotLimit: number
  backupLimit: number
  siteLimit: number
  swapSize: number
  trafficLimit: string
  trafficLimitSpeed: string
  price: number
  billingCycle: number
  setupFee: number
  monthlyPrice: number
  isActive: boolean
  isSoldOut: boolean
  sortOrder: number
  slaGuarantee: number | null
}

interface SourceTab {
  key: PackageSource
  type: 'official'
  label: string
}

const router = useRouter()
const route = useRoute()
const { t } = useI18n()
const authStore = useAuthStore()
const toast = useToast()
const themeStore = useThemeStore()
const configStore = useConfigStore()

// 右栏滚动容器 ref（选套餐后滚回顶部）
const rightPanelScrollRef = ref<HTMLElement | null>(null)

// 当前选中的套餐来源
const packageSource = ref<PackageSource>('official')
const sourceLoading = ref<boolean>(false)

// 数据加载状态
// 地区选择
const regions = ref<Region[]>([])
const selectedRegion = ref<string | null>(null)
const regionsLoading = ref<boolean>(false)

const packages = ref<Package[]>([])
const userQuota = ref<UserQuota | null>(null)
const sshKeys = ref<SshKey[]>([])
const availableHosts = ref<AvailableHost[]>([])
const hostLookupMessage = ref('')
let hostLookupRevision = 0
const availableImages = ref<ImageOption[]>([])
const packagePlans = ref<PackagePlan[]>([])
const imagesLoading = ref<boolean>(false)
const hostsLoading = ref<boolean>(false)
const plansLoading = ref<boolean>(false)
const loading = ref<boolean>(true)
const submitting = ref<boolean>(false)
const error = ref<string>('')
const instanceNameEditedByUser = ref<boolean>(false)
const turnstileToken = ref('')
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileSectionRef = ref<HTMLElement | null>(null)
const isCreateTurnstileRequired = computed(() => configStore.turnstileEnabled && Boolean(configStore.turnstileSiteKey))
const createTurnstileSiteKey = computed(() => configStore.turnstileSiteKey || '')
const createIntentIdempotencyKey = ref<string | null>(null)

// 表单数据
interface InstanceForm {
  name: string
  packageId: number | null
  planId: number | null  // 付费方案ID
  hostId: number | null
  image: string
  cpu: number
  memory: number
  disk: number
  sshKeyId: number | null
  customInitCommandIds: number[]  // 用户自定义初始化命令
  promoCode: string  // 优惠码
  autoRemotePort: boolean  // 自动下发远程端口（Linux=22 / Windows=3389），不计入端口配额
}

const form = ref<InstanceForm>({
  name: '',
  packageId: null,
  planId: null,
  hostId: null,
  image: '',
  cpu: 15,
  memory: 128,
  disk: 512,
  sshKeyId: null,
  customInitCommandIds: [],
  promoCode: '',
  autoRemotePort: true
})

// 认证方式相关状态与处理
const authType = ref<'ssh' | 'password'>('password')
const passwordMode = ref<'auto' | 'custom'>('auto')
const customPassword = ref('')
const showPassword = ref(false)

const isCustomPasswordValid = computed(() => {
  const pwd = customPassword.value
  if (!pwd || pwd.length < 8 || pwd.length > 128) return false
  if (!/[A-Z]/.test(pwd)) return false
  if (!/[a-z]/.test(pwd)) return false
  if (!/[0-9]/.test(pwd)) return false
  const lowerPw = pwd.toLowerCase()
  if (['password', 'admin123', 'root123', '12345678', 'qwertyuiop'].some(weak => lowerPw.includes(weak))) {
    return false
  }
  return true
})

const isAuthValid = computed(() => {
  if (authType.value === 'ssh') {
    return sshKeys.value.length > 0 && form.value.sshKeyId !== null
  }
  if (passwordMode.value === 'auto') {
    return true
  }
  return isCustomPasswordValid.value
})

function generateRandomCustomPassword(): void {
  const upper = 'ABCDEFGHJKLMNPQRSTUVWXYZ'
  const lower = 'abcdefghijkmnopqrstuvwxyz'
  const digits = '23456789'
  const special = '!@#$%^&*'
  const all = upper + lower + digits + special
  const chars = [
    upper[Math.floor(Math.random() * upper.length)],
    lower[Math.floor(Math.random() * lower.length)],
    digits[Math.floor(Math.random() * digits.length)],
    special[Math.floor(Math.random() * special.length)]
  ]
  for (let i = 4; i < 16; i++) {
    chars.push(all[Math.floor(Math.random() * all.length)])
  }
  for (let i = chars.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1))
    const temp = chars[i]
    chars[i] = chars[j]
    chars[j] = temp
  }
  customPassword.value = chars.join('')
}

watch(authType, (val) => {
  if (val === 'ssh' && sshKeys.value.length > 0 && form.value.sshKeyId === null) {
    form.value.sshKeyId = sshKeys.value[0].id
  }
})

watch([form, authType, passwordMode, customPassword], () => {
  if (!submitting.value) {
    createIntentIdempotencyKey.value = null
  }
}, { deep: true })

// 优惠码验证状态
const promoCodeVerifying = ref(false)
const promoCodeValid = ref<boolean | null>(null)
const promoCodeDiscount = ref<number>(0)
const promoCodeCommissionRate = ref<number>(0)  // 返利率
const promoCodeError = ref('')

// 是否为付费套餐（有方案）
const isPaidPackage = computed<boolean>(() => packagePlans.value.length > 0)

// 是否是用户自己的付费套餐（不允许自己开通）
const isOwnPaidPackage = computed<boolean>(() => {
  if (!selectedPackage.value || !isPaidPackage.value) return false
  return selectedPackage.value.ownerId === authStore.user?.id
})

const showDestroyTrafficNotice = computed<boolean>(() => {
  return isPaidPackage.value
    && form.value.planId !== null
    && !isOwnPaidPackage.value
})

// 根据选中地区过滤的套餐列表
const filteredPackages = computed<Package[]>(() => {
  let result: Package[]
  
  if (selectedRegion.value === null) {
    // "全部"选项：返回所有套餐
    result = packages.value
  } else {
    // 根据地区过滤：查找选中地区包含的套餐 ID
    const region = regions.value.find(r => r.code === selectedRegion.value)
    if (!region) {
      result = packages.value
    } else {
      result = packages.value.filter(pkg => region.packageIds.includes(pkg.id))
    }
  }
  
  // 售罄的套餐排在最后
  return [...result].sort((a, b) => {
    if (a.soldOut === b.soldOut) return 0
    return a.soldOut ? 1 : -1
  })
})

// 当前选中的套餐
const selectedPackage = computed<Package | undefined>(() => packages.value.find(p => p.id === form.value.packageId))

// 当前选中的方案
const selectedPlan = computed<PackagePlan | undefined>(() => packagePlans.value.find(p => p.id === form.value.planId))

const selectedHost = computed<AvailableHost | undefined>(() => availableHosts.value.find(host => host.id === form.value.hostId))

const prerequisitePackageName = computed<string | null>(() => {
  if (!selectedPackage.value?.required_package_id) return null
  return selectedPackage.value.required_package_name || `#${selectedPackage.value.required_package_id}`
})

const prerequisiteMissing = computed<boolean>(() => {
  return !!selectedPackage.value?.required_package_id
    && (selectedPackage.value as Package & { has_required_package_instance?: boolean }).has_required_package_instance !== true
})

const prerequisiteMessage = computed<string>(() => {
  return t('instance.createPage.packagePrerequisiteRequired', {
    package: prerequisitePackageName.value || ''
  })
})

// 流量格式化
function formatTraffic(bytes: string): string {
  const b = BigInt(bytes || '0')
  if (b === BigInt(0)) return t('common.unlimited')
  if (b >= BigInt(1024 * 1024 * 1024 * 1024)) {
    return (Number(b) / (1024 * 1024 * 1024 * 1024)).toFixed(1) + ' TB'
  }
  if (b >= BigInt(1024 * 1024 * 1024)) {
    return (Number(b) / (1024 * 1024 * 1024)).toFixed(0) + ' GB'
  }
  return (Number(b) / (1024 * 1024)).toFixed(0) + ' MB'
}

// 计费周期标签
function getBillingCycleLabel(months: number): string {
  if (configStore.freeSiteMode) return getFreeSiteBillingCycleLabel(months)
  switch (months) {
    case 1: return t('billing.cycle.monthly')
    case 3: return t('billing.cycle.quarterly')
    case 6: return t('billing.cycle.semiAnnual')
    case 12: return t('billing.cycle.annual')
    default: return `${months} ${t('billing.cycle.months')}`
  }
}

// 方案价格计算（注意：数据库存储为分，需要转换为元）
const planPriceInfo = computed(() => {
  if (!selectedPlan.value) return null
  const plan = selectedPlan.value
  // 价格单位转换：分 -> 元
  const planPriceYuan = plan.price / 100
  const originalPrice = planPriceYuan
  const discount = promoCodeValid.value ? promoCodeDiscount.value : 0
  const discountAmount = Number((planPriceYuan * discount).toFixed(2))
  const finalPrice = Number((originalPrice - discountAmount).toFixed(2))
  return {
    originalPrice,
    planPrice: planPriceYuan,
    discountAmount,
    finalPrice,
    discountRate: discount
  }
})

// 内存格式化（MB -> GB/MB）
function formatMemory(mb: number): string {
  if (mb >= 1024) {
    return (mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1) + ' GB'
  }
  return mb + ' MB'
}

// CPU 格式化（以 C 为单位，1C = 1 核；100% → 1C、150% → 1.5C）
function formatCpu(value: number): string {
  const cores = value / 100
  return cores % 1 === 0 ? `${cores}C` : `${cores.toFixed(1)}C`
}

// 硬盘格式化（MB -> GB，使用1024进制）
function formatDisk(mb: number): string {
  if (mb >= 1024) {
    return (mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1) + ' GB'
  }
  return mb + ' MB'
}

// 从镜像名称提取发行版标识（用于 DistroIcon 组件）
function getDistroFromName(name: string): string {
  const lowerName = (name || '').toLowerCase()
  const distros = ['almalinux', 'alma', 'alpine', 'arch', 'centos', 'debian', 'fedora', 'gentoo', 'kali', 'mint', 'opensuse', 'suse', 'oracle', 'redhat', 'rhel', 'rockylinux', 'rocky', 'ubuntu', 'void']
  for (const distro of distros) {
    if (lowerName.includes(distro)) return distro
  }
  return 'linux'
}

// 获取当前选中镜像的发行版标识
const selectedImageDistro = computed(() => {
  const img = availableImages.value.find(i => i.value === form.value.image)
  return img?.icon || 'linux'
})

// 配额检查
interface QuotaCheckResult {
  valid: boolean
  errors: string[]
}

const quotaCheck = computed<QuotaCheckResult>(() => {
  if (!userQuota.value) {
    return { valid: true, errors: [] }
  }
  
  // 注意：不再限制实例配额，用户可以创建无限数量的实例
  // CPU/内存/磁盘限制由套餐包控制
  const errors: string[] = []
  
  return { valid: errors.length === 0, errors }
})

// 资源限制检查（基于套餐配额信息）
const resourceLimitCheck = computed<QuotaCheckResult>(() => {
  const errors: string[] = []
  const quotaInfo = selectedPackage.value?.quotaInfo
  
  if (!quotaInfo) {
    // 如果没有配额信息，允许提交（可能是自己的套餐，无限制）
    return { valid: true, errors: [] }
  }
  
  // 检查实例个数限制：实例个数 = 0 时禁止创建
  if (quotaInfo.remainingInstances !== null && quotaInfo.remainingInstances <= 0) {
    errors.push(t('instance.createPage.resourceLimit.noInstances'))
  }
  
  // 检查内存限制：内存 < 128MB 时禁止创建
  if (quotaInfo.remainingMemory !== null && quotaInfo.remainingMemory < 128) {
    errors.push(t('instance.createPage.resourceLimit.insufficientMemory'))
  }
  
  // 检查CPU限制：CPU < 15% 时禁止创建
  if (quotaInfo.remainingCpu !== null && quotaInfo.remainingCpu < 15) {
    errors.push(t('instance.createPage.resourceLimit.insufficientCpu'))
  }
  
  return { valid: errors.length === 0, errors }
})

const canSubmit = computed<boolean>(() => {
  // 售罄套餐不允许创建
  if (selectedPackage.value?.soldOut) {
    return false
  }

  if (prerequisiteMissing.value) {
    return false
  }
  
  const baseChecks = form.value.packageId !== null &&
         form.value.hostId !== null &&
         !!form.value.image &&
         availableImages.value.length > 0 &&
         isAuthValid.value &&
         quotaCheck.value.valid &&
         resourceLimitCheck.value.valid &&
         !submitting.value &&
         !isOwnPaidPackage.value  // 不允许使用自己的付费套餐
  
  // 付费套餐需要选择方案
  if (isPaidPackage.value) {
    if (!baseChecks || form.value.planId === null || !selectedPlan.value || selectedPlan.value.isSoldOut) {
      return false
    }
    // 如果用户输入了优惠码，必须处于验证通过状态，且不在验证中
    if (form.value.promoCode.trim()) {
      if (promoCodeVerifying.value || promoCodeValid.value !== true) {
        return false
      }
    }
    return true
  }
  
  return baseChecks
})

const submitDisabledReason = computed<string>(() => {
  if (selectedPackage.value?.soldOut) {
    return t('instance.createPage.packageSoldOut')
  }
  if (isOwnPaidPackage.value) {
    return t('instance.createPage.cannotBuyOwn')
  }
  if (isPaidPackage.value) {
    if (form.value.planId === null || !selectedPlan.value) {
      return t('instance.createPage.selectPlanRequired')
    }
    if (selectedPlan.value.isSoldOut) {
      return t('instance.createPage.planSoldOut')
    }
    if (form.value.promoCode.trim()) {
      if (promoCodeVerifying.value) {
        return t('aff.verifying')
      }
      if (promoCodeValid.value === false) {
        return promoCodeError.value || t('aff.promoCodeInvalid')
      }
      if (promoCodeValid.value === null) {
        return promoCodeError.value || t('aff.promoCodeInvalid')
      }
    }
  }
  if (form.value.hostId === null) {
    return t('instance.createPage.selectHostRequired')
  }
  if (!form.value.image) {
    return t('instance.createPage.selectImageRequired')
  }
  if (!quotaCheck.value.valid) {
    return quotaCheck.value.errors[0] || ''
  }
  if (!resourceLimitCheck.value.valid) {
    return resourceLimitCheck.value.errors[0] || ''
  }
  return ''
})

// 检测资源不足的情况
const hostsInsufficient = computed<boolean>(() => {
  // 付费套餐未选方案时，不显示资源不足警告（因为还没加载宿主机）
  if (isPaidPackage.value && form.value.planId === null) {
    return false
  }
  // 套餐绑定了宿主机但没有可用的，且不在加载中
  return !hostsLoading.value &&
         selectedPackage.value !== undefined &&
         !!selectedPackage.value.host_ids &&
         selectedPackage.value.host_ids.length > 0 &&
         availableHosts.value.length === 0
})

const createPageTextPrefix = computed(() => configStore.freeSiteMode ? 'instance.createPage.fun' : 'instance.selector')
const packageCountLabelKey = computed(() => `${createPageTextPrefix.value}.packageCount`)

function getCreatePageText(key: string): string {
  return t(`${createPageTextPrefix.value}.${key}`)
}

function formatInstanceNameTimestamp(date = new Date()): string {
  const pad = (value: number) => String(value).padStart(2, '0')
  return [
    date.getFullYear(),
    pad(date.getMonth() + 1),
    pad(date.getDate()),
    pad(date.getHours()),
    pad(date.getMinutes()),
    pad(date.getSeconds())
  ].join('')
}

function getRandomNameSuffix(length = 4): string {
  const alphabet = 'abcdefghijklmnopqrstuvwxyz0123456789'
  const bytes = new Uint8Array(length)
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < length; i += 1) {
      bytes[i] = Math.floor(Math.random() * 256)
    }
  }
  return Array.from(bytes, byte => alphabet[byte % alphabet.length]).join('')
}

function getInstanceNameRegionPrefix(): string {
  const code = selectedHost.value?.countryCode || selectedRegion.value || 'incus'
  return String(code).toLowerCase().replace(/[^a-z0-9]/g, '').slice(0, 6) || 'incus'
}

function generateInstanceName(): string {
  const uidTail = String(authStore.user?.id || 0).padStart(3, '0').slice(-3)
  return `${getInstanceNameRegionPrefix()}${uidTail}-${formatInstanceNameTimestamp()}-${getRandomNameSuffix()}`
}

function refreshGeneratedInstanceName(force = false): void {
  if (!force && instanceNameEditedByUser.value) return
  form.value.name = generateInstanceName()
}

function handleInstanceNameInput(): void {
  instanceNameEditedByUser.value = true
}

function focusCreateTurnstile(): void {
  const section = turnstileSectionRef.value
  if (!section) return

  section.scrollIntoView({ behavior: 'smooth', block: 'center' })
  window.setTimeout(() => {
    section.focus({ preventScroll: true })
  }, 250)
}

async function getCreateTurnstileToken(): Promise<string | undefined | null> {
  if (!isCreateTurnstileRequired.value) return undefined

  const widgetToken = turnstileRef.value?.getToken?.()
  if (widgetToken) {
    turnstileToken.value = widgetToken
    return widgetToken
  }

  if (turnstileToken.value) return turnstileToken.value

  const domToken = document
    .querySelector<HTMLInputElement>('input[name="cf-turnstile-response"]')
    ?.value
    ?.trim()
  if (domToken) {
    turnstileToken.value = domToken
    return domToken
  }

  focusCreateTurnstile()
  error.value = t('instance.createPage.turnstileRequired')
  toast.warning(t('instance.createPage.turnstileRequired'))
  return null
}

function resetCreateTurnstile(): void {
  turnstileToken.value = ''
  turnstileRef.value?.reset?.()
}

function onTurnstileExpire(): void {
  turnstileToken.value = ''
}

function onTurnstileVerify(token: string): void {
  turnstileToken.value = token
}

function onTurnstileError(): void {
  turnstileToken.value = ''
  toast.error(t('instance.createPage.turnstileFailed'))
}

function getPackageSourceLabel(source: 'official'): string {
  if (configStore.freeSiteMode) {
    return t(`instance.createPage.source.${source}`)
  }

  return t(`publicSite.market.${source}`)
}

const sourceTabs = computed<SourceTab[]>(() => {
  return [
    {
      key: 'official',
      type: 'official',
      label: getPackageSourceLabel('official')
    }
  ]
})

function isPackageSourceSelectable(source: PackageSource): boolean {
  return source === 'official' || sourceTabs.value.some(tab => tab.key === source)
}

function getSelectablePackageSource(source: PackageSource): PackageSource {
  return isPackageSourceSelectable(source) ? source : 'official'
}

const turnstileAction = computed(() => 'create_instance')

onMounted(async (): Promise<void> => {
  await configStore.loadPublicConfig(true)

  // 检查 URL 参数（分享链接）
  const packageParam = route.query.package as string | undefined
  const planParam = route.query.plan as string | undefined
  const targetPackageId = packageParam ? Number(packageParam) : null
  const targetPlanId = planParam ? Number(planParam) : null
  
  // 确定初始加载的套餐来源
  const initialSource = normalizePackageSourceQuery(route.query.source)
  
  try {
    const [userRes, keysRes] = await Promise.all([
      api.users.get(authStore.user!.id),
      api.sshKeys.list()
    ])

    const effectiveInitialSource = getSelectablePackageSource(initialSource)
    const initialSourceRequest = toPackageSourceRequest(effectiveInitialSource)

    const [packagesRes, regionsRes] = await Promise.all([
      api.packages.list(initialSourceRequest),
      api.packages.getRegions(initialSourceRequest)
    ])
    
    packages.value = ((packagesRes as { packages?: Package[] }).packages || []).filter(p => p.active === 1)
    const userData = userRes as { user?: { quota?: UserQuota } }
    userQuota.value = userData.user?.quota || null
    sshKeys.value = keysRes.keys || []
    regions.value = regionsRes.regions || []
    
    // 如果有 URL 参数指定的来源，更新状态
    if (effectiveInitialSource !== 'official') {
      packageSource.value = effectiveInitialSource as PackageSource
    }
    
    // 处理分享链接参数：自动选择地区和套餐
    if (targetPackageId) {
      // 查找包含该套餐的地区
      const targetRegion = regions.value.find(r => r.packageIds.includes(targetPackageId))
      if (targetRegion) {
        selectedRegion.value = targetRegion.code
      }
      
      // 选择套餐
      const targetPkg = packages.value.find(p => p.id === targetPackageId)
      if (targetPkg) {
        await selectPackage(targetPkg, targetPlanId)
      } else {
        // 套餐不存在，提示用户并选择第一个
        toast.warning(t('instance.createPage.sharedPackageNotFound'))
        if (filteredPackages.value.length > 0) {
          selectPackage(filteredPackages.value[0])
        }
      }
    } else {
      // 没有分享链接参数，默认选中第一个套餐
      if (filteredPackages.value.length > 0) {
        selectPackage(filteredPackages.value[0])
      }
    }
    
    if (sshKeys.value.length > 0) {
      authType.value = 'ssh'
      form.value.sshKeyId = sshKeys.value[0].id
    } else {
      authType.value = 'password'
      passwordMode.value = 'auto'
      form.value.sshKeyId = null
    }
  } catch (err: any) {
    error.value = t('instance.createPage.loadFailed') + ': ' + (err?.message || String(err))
  } finally {
    loading.value = false
  }
})

/**
 * 切换套餐来源
 */
async function switchPackageSource(source: PackageSource): Promise<void> {
  if (!isPackageSourceSelectable(source)) return
  if (source === packageSource.value || sourceLoading.value) return
  
  sourceLoading.value = true
  packageSource.value = source
  
  // 重置选择状态
  form.value.packageId = null
  form.value.planId = null
  form.value.hostId = null
  form.value.image = ''
  packagePlans.value = []
  availableHosts.value = []
  availableImages.value = []
  selectedRegion.value = null
  
  try {
    const sourceRequest = toPackageSourceRequest(source)
    // 并行加载套餐和地区数据
    const [packagesRes, regionsRes] = await Promise.all([
      api.packages.list(sourceRequest),
      api.packages.getRegions(sourceRequest)
    ])
    
    packages.value = ((packagesRes as { packages?: Package[] }).packages || []).filter(p => p.active === 1)
    regions.value = regionsRes.regions || []
    
    // 如果有套餐，默认选中第一个
    if (filteredPackages.value.length > 0) {
      selectPackage(filteredPackages.value[0])
    }
  } catch (err: any) {
    toast.error(t('common.loadFailed'))
    console.error('Failed to load packages:', err)
  } finally {
    sourceLoading.value = false
  }
}

// 是否正在切换套餐（用于防止 watch 意外触发 loadAvailableHosts）
const isSwitchingPackage = ref(false)

/**
 * 选择地区
 */
function selectRegion(regionCode: string | null): void {
  selectedRegion.value = regionCode
  // 重置套餐选择
  form.value.packageId = null
  form.value.planId = null
  form.value.hostId = null
  form.value.image = ''
  packagePlans.value = []
  availableHosts.value = []
  availableImages.value = []
  
  // 如果过滤后有套餐，自动选中第一个
  if (filteredPackages.value.length > 0) {
    selectPackage(filteredPackages.value[0])
  }
}

async function selectPackage(pkg: Package, preferredPlanId?: number | null): Promise<void> {
  isSwitchingPackage.value = true  // 开始切换套餐
  hostLookupRevision++
  hostsLoading.value = false
  hostLookupMessage.value = ''
  
  form.value.packageId = pkg.id
  form.value.hostId = null
  form.value.image = ''
  form.value.planId = null  // 重置方案选择
  packagePlans.value = []   // 清空方案列表
  availableHosts.value = [] // 清空宿主机列表，避免显示旧数据
  availableImages.value = []
  resetPromoCode()          // 重置优惠码状态
  
  // 先加载套餐方案（这样才能正确判断是否是付费套餐）
  await loadPackagePlans(pkg.id)

  if (packagePlans.value.length > 0 && preferredPlanId) {
    const matchedPlan = packagePlans.value.find(plan => plan.id === preferredPlanId)
    if (matchedPlan && !matchedPlan.isSoldOut) {
      selectPlan(matchedPlan)
    }
  }
  
  // 如果是免费套餐（没有方案），设置默认资源值
  if (packagePlans.value.length === 0) {
    // CPU：最小15%，步长5%，确保不超过套餐最大值
    const cpuMax = Math.max(15, pkg.cpu_max) // 确保套餐最大值至少为15%
    const cpuHalf = Math.ceil(cpuMax / 2)
    form.value.cpu = Math.max(15, Math.min(Math.floor(cpuHalf / 5) * 5, cpuMax))
    
    // 内存：最小128MB，步长64MB，确保不超过套餐最大值
    const memoryMax = Math.max(128, pkg.memory_max) // 确保套餐最大值至少为128MB
    const memoryHalf = Math.ceil(memoryMax / 2)
    form.value.memory = Math.max(128, Math.min(Math.floor(memoryHalf / 64) * 64, memoryMax))
    
    // 硬盘：最小512MB，步长0.1GB (102MB)，确保不超过套餐最大值
    const diskMax = Math.max(512, pkg.disk_max) // 确保套餐最大值至少为512MB
    const diskHalf = Math.ceil(diskMax / 2)
    form.value.disk = Math.max(512, Math.min(Math.floor(diskHalf / 102) * 102, diskMax))
  }
  
  isSwitchingPackage.value = false  // 结束切换套餐

  // 选套餐后右栏滚回顶部
  if (rightPanelScrollRef.value) {
    rightPanelScrollRef.value.scrollTop = 0
  }
  
  // 加载套餐详情以获取quotaInfo（仅在套餐信息中还没有quotaInfo时加载）
  const currentPkg = packages.value.find(p => p.id === pkg.id)
  if (!currentPkg?.quotaInfo) {
    try {
      const detailRes = await api.packages.get(pkg.id) as any
      const detailPkg = detailRes.package || detailRes
      // 更新packages数组中的套餐信息，包含quotaInfo
      const index = packages.value.findIndex(p => p.id === pkg.id)
      if (index !== -1) {
        packages.value[index] = { 
          ...packages.value[index], 
          quotaInfo: detailPkg.quotaInfo || null,
          required_package_id: detailPkg.required_package_id ?? packages.value[index].required_package_id ?? null,
          required_package_name: detailPkg.required_package_name ?? packages.value[index].required_package_name ?? null,
          has_required_package_instance: detailPkg.has_required_package_instance ?? (packages.value[index] as any).has_required_package_instance ?? true
        }
      }
    } catch (err) {
      // 静默失败，不影响其他功能
      console.warn('Failed to load package quota info:', err)
    }
  }
  
  // 检查套餐是否绑定了宿主机
  if (!pkg.host_ids || pkg.host_ids.length === 0) {
    availableHosts.value = []
    availableImages.value = []
    form.value.image = ''
    toast.warning(t('instance.createPage.packageNoHosts'))
  } else {
    // 免费套餐需要加载宿主机，付费套餐由方案选择时加载
    if (!isPaidPackage.value) {
      loadAvailableHosts()
    }
  }
  
  previousMemoryIsLow = form.value.memory <= 128
}

/**
 * 加载套餐方案列表
 */
async function loadPackagePlans(packageId: number): Promise<void> {
  plansLoading.value = true
  try {
    const res = await api.packages.getPlans(packageId, { activeOnly: true })
    packagePlans.value = res.plans || []
  } catch (err) {
    console.warn('Failed to load package plans:', err)
    packagePlans.value = []
  } finally {
    plansLoading.value = false
  }
}

/**
 * 选择付费方案
 */
function selectPlan(plan: PackagePlan): void {
  if (plan.isSoldOut || !plan.isActive) {
    return
  }

  form.value.planId = plan.id
  // 使用方案的固定资源配置
  form.value.cpu = plan.cpu
  form.value.memory = plan.memory
  form.value.disk = plan.disk
  // 重置优惠码状态
  resetPromoCode()
  // 加载可用宿主机
  loadAvailableHosts()
}

function clearPromoStatus(): void {
  promoCodeValid.value = null
  promoCodeDiscount.value = 0
  promoCodeCommissionRate.value = 0
  promoCodeError.value = ''
}

/**
 * 重置优惠码状态
 */
function resetPromoCode(): void {
  form.value.promoCode = ''
  clearPromoStatus()
}

// 监听优惠码输入变动，及时重置旧验证状态，防止篡改后沿用旧验证
watch(() => form.value.promoCode, () => {
  clearPromoStatus()
})

/**
 * 验证优惠码
 */
async function verifyPromoCode(): Promise<void> {
  if (!form.value.promoCode.trim() || !form.value.planId) {
    clearPromoStatus()
    return
  }
  
  promoCodeVerifying.value = true
  promoCodeError.value = ''
  
  try {
    const res = await api.coupon.validate(form.value.promoCode.trim(), form.value.planId, form.value.packageId)
    if (res.valid) {
      promoCodeValid.value = true
      promoCodeDiscount.value = parseFloat(String(res.discountRate ?? 0)) || 0
      // 从验证响应中获取返利率（如果有的话）
      const commissionRate = parseFloat(String(res.commissionRate ?? 0)) || 0
      promoCodeCommissionRate.value = commissionRate
    } else {
      promoCodeValid.value = false
      promoCodeError.value = res.error || t('aff.promoCodeInvalid')
    }
  } catch (err: any) {
    promoCodeValid.value = false
    promoCodeError.value = err?.response?.data?.error || err.message || t('aff.promoCodeInvalid')
  } finally {
    promoCodeVerifying.value = false
  }
}

async function loadAvailableHosts(): Promise<void> {
  const revision = ++hostLookupRevision
  hostLookupMessage.value = ''
  if (!form.value.packageId) {
    availableHosts.value = []
    form.value.hostId = null
    availableImages.value = []
    form.value.image = ''
    return
  }
  
  // 如果套餐没有绑定宿主机，不发起 API 请求
  if (!selectedPackage.value?.host_ids || selectedPackage.value.host_ids.length === 0) {
    availableHosts.value = []
    form.value.hostId = null
    availableImages.value = []
    form.value.image = ''
    return
  }
  
  hostsLoading.value = true
  try {
    const res = await api.instances.getAvailableHosts({
      packageId: form.value.packageId,
      planId: form.value.planId || undefined,
      cpu: form.value.cpu,
      memory: form.value.memory,
      disk: form.value.disk
    })
    if (revision !== hostLookupRevision) return
    availableHosts.value = res.hosts || []
    const reason = res.unavailableHosts?.find(h => h.reason.startsWith('storage_'))?.reason || res.unavailableHosts?.[0]?.reason
    if (reason) hostLookupMessage.value = t(`storageCapacity.${reason}`)

    if (availableHosts.value.length === 0) {
      form.value.hostId = null
      availableImages.value = []
      form.value.image = ''
      return
    }

    const currentExists = availableHosts.value.some(host => host.id === form.value.hostId)
    if (!currentExists) {
      form.value.hostId = availableHosts.value[0].id
    }
  } catch (err: any) {
    if (revision !== hostLookupRevision) return
    hostLookupMessage.value = t('storageCapacity.lookupFailed')
    availableHosts.value = []
    form.value.hostId = null
    availableImages.value = []
    form.value.image = ''
    
    // 如果是套餐没有绑定宿主机的错误，显示友好提示
    if (err?.response?.data?.code === 'PACKAGE_NO_HOSTS' || err?.message?.includes('bind at least one host')) {
      toast.error(t('instance.createPage.packageNoHosts'))
    }
  } finally {
    if (revision === hostLookupRevision) hostsLoading.value = false
  }
}

watch(() => form.value.packageId, () => {
  if (selectedPackage.value) {
    // 确保CPU值符合最小值和步长要求（15%起步，步长5%）
    const cpuMax = Math.max(15, selectedPackage.value.cpu_max)
    const normalizedCpu = Math.max(15, Math.min(Math.floor(form.value.cpu / 5) * 5, cpuMax))
    form.value.cpu = normalizedCpu
    // 确保内存和硬盘值符合最小值和步长要求
    const memoryMax = Math.max(128, selectedPackage.value.memory_max)
    const normalizedMemory = Math.max(128, Math.min(Math.floor(form.value.memory / 64) * 64, memoryMax))
    const diskMax = Math.max(512, selectedPackage.value.disk_max)
    const normalizedDisk = Math.max(512, Math.min(Math.floor(form.value.disk / 102) * 102, diskMax))
    form.value.memory = normalizedMemory
    form.value.disk = normalizedDisk
  }
})

watch([() => form.value.cpu, () => form.value.memory, () => form.value.disk], () => {
  // 付费套餐不需要监听资源变化（配置固定）
  // 切换套餐时也不触发（避免用旧资源值查询）
  if (form.value.packageId && !isPaidPackage.value && !isSwitchingPackage.value) {
    loadAvailableHosts()
  }
}, { immediate: false })

watch(() => form.value.hostId, (newHostId) => {
  if (!selectedPackage.value || isSwitchingPackage.value) return

  if (!newHostId) {
    availableImages.value = []
    form.value.image = ''
    return
  }

  refreshGeneratedInstanceName()
  loadAvailableImages(
    selectedPackage.value.instance_type as 'container' | 'vm' | undefined,
    form.value.memory,
    newHostId
  )
}, { immediate: false })

// 内存变化时重新加载镜像（128MB 限制只能选 Alpine/Debian）
let previousMemoryIsLow = false
watch(() => form.value.memory, (newMemory) => {
  if (!selectedPackage.value || isSwitchingPackage.value) return
  
  const currentIsLow = newMemory <= 128
  // 只有在跨越 128MB 阈值时才重新加载镜像
  if (currentIsLow !== previousMemoryIsLow) {
    previousMemoryIsLow = currentIsLow
    loadAvailableImages(
      selectedPackage.value.instance_type as 'container' | 'vm' | undefined,
      newMemory,
      form.value.hostId || undefined
    )
  }
}, { immediate: false })

async function loadAvailableImages(instanceType?: 'container' | 'vm', memory?: number, hostId?: number): Promise<void> {
  if (!hostId) {
    availableImages.value = []
    form.value.image = ''
    return
  }

  imagesLoading.value = true
  try {
    const res = await api.images.getSystemImages(instanceType, memory, hostId)
    availableImages.value = (res.images || []).map(img => ({
      value: img.remoteAlias,
      label: img.name,
      icon: img.icon || getDistroFromName(img.name)
    }))
    
    if (availableImages.value.length > 0) {
      const currentExists = availableImages.value.some((img) => img.value === form.value.image)
      if (!currentExists) {
        form.value.image = availableImages.value[0].value
      }
    } else {
      form.value.image = ''
    }
  } catch (_err) {
    availableImages.value = []
    form.value.image = ''
  } finally {
    imagesLoading.value = false
  }
}

async function handleSubmit(): Promise<void> {
  if (!canSubmit.value) return
  
  error.value = ''
  submitting.value = true
  
  try {
    if (form.value.packageId === null) throw new Error(t('instance.createPage.selectPackage'))
    if (form.value.hostId === null) throw new Error(getCreatePageText('selectHost'))
    if (authType.value === 'ssh') {
      if (form.value.sshKeyId === null) throw new Error(getCreatePageText('selectSshKey'))
    } else {
      if (passwordMode.value === 'custom') {
        if (!customPassword.value) throw new Error(t('instance.createPage.passwordRequired'))
        if (!isCustomPasswordValid.value) throw new Error(t('instance.createPage.passwordTooWeak'))
      }
    }
    
    if (!form.value.name.trim()) {
      refreshGeneratedInstanceName(true)
    }

    // 验证实例名称
    const nameValidation = validateInstanceName(form.value.name)
    if (!nameValidation.valid) {
      throw new Error(nameValidation.message)
    }

    // 如果是付费套餐且填写了优惠码，确保先完成验证；若无效则强行拦截禁止创建
    if (isPaidPackage.value && form.value.promoCode.trim()) {
      if (promoCodeValid.value === null || promoCodeVerifying.value) {
        await verifyPromoCode()
      }
      if (promoCodeValid.value !== true) {
        const errMsg = promoCodeError.value || t('aff.promoCodeInvalid')
        throw new Error(errMsg)
      }
    }

    const verificationToken = await getCreateTurnstileToken()
    if (verificationToken === null) return
    if (isPaidPackage.value && !createIntentIdempotencyKey.value) {
      createIntentIdempotencyKey.value = crypto.randomUUID?.() || `instance-create-${Date.now()}-${Math.random().toString(36).slice(2)}`
    }
    const requestPayload: CreateInstanceRequest = {
      name: form.value.name.trim(),
      packageId: form.value.packageId,
      planId: form.value.planId || undefined,
      hostId: form.value.hostId,
      image: form.value.image,
      cpu: form.value.cpu,
      memory: form.value.memory,
      disk: form.value.disk,
      sshKeyId: authType.value === 'ssh' ? (form.value.sshKeyId ?? undefined) : undefined,
      password: (authType.value === 'password' && passwordMode.value === 'custom') ? customPassword.value : undefined,
      customInitCommandIds: form.value.customInitCommandIds.length > 0 ? form.value.customInitCommandIds : undefined,
      promoCode: (isPaidPackage.value && form.value.promoCode.trim()) ? form.value.promoCode.trim() : undefined,
      idempotencyKey: isPaidPackage.value ? createIntentIdempotencyKey.value || undefined : undefined,
      turnstileToken: verificationToken,
      autoRemotePort: form.value.autoRemotePort
    }

    const response = await api.instances.create(requestPayload)
    createIntentIdempotencyKey.value = null
    
    toast.success(t('instance.createPage.createSuccess'))
    resetCreateTurnstile()
    submitting.value = false
    await router.push({
      path: instancesPath(),
      query: response.instance?.id ? { created: String(response.instance.id) } : undefined
    })
  } catch (err: any) {
    const translated = translateError(err)
    error.value = translated
    toast.error(translated)

    // 如果报错与优惠码相关，同步更新优惠码状态并提示
    const errString = String(err?.response?.data?.error || err?.details || err?.message || '')
    const isPromoError = errString.includes('优惠码') ||
                         errString.includes('PROMO_') ||
                         errString.includes('AFF_') ||
                         errString.includes('CANNOT_USE_OWN_AFF_CODE')
    if (isPromoError && form.value.promoCode.trim()) {
      promoCodeValid.value = false
      promoCodeError.value = translated
    }
  } finally {
    if (submitting.value) {
      resetCreateTurnstile()
    }
    submitting.value = false
  }
}
</script>

<template>
  <div class="kawaii-page animate-fade-in lg:h-full lg:flex lg:flex-col">
    <!-- Header with Apple PageHeader -->
    <PageHeader
      :title="$t('instance.createPage.title')"
      :description="$t('instance.createPage.description')"
      :back="true"
      :back-label="$t('common.back')"
      class="shrink-0 !mb-4"
      @back="router.push(instancesPath())"
    >
      <template #actions>
        <!-- 套餐来源切换器（多来源时显示） -->
        <div v-if="sourceTabs.length > 1" class="inline-flex items-center gap-1 overflow-x-auto rounded-full border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-1 shadow-sm scrollbar-hide">
          <button
            v-for="source in sourceTabs"
            :key="source.key"
            type="button"
            :disabled="sourceLoading"
            class="relative shrink-0 rounded-full px-4 py-1.5 text-xs font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98]"
            :class="[
              packageSource === source.key
                ? 'bg-[var(--accent)] text-white shadow-sm'
                : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]',
              sourceLoading ? 'cursor-not-allowed opacity-50' : ''
            ]"
            @click="switchPackageSource(source.key)"
          >
            <span class="relative z-10 flex items-center gap-1.5">
              <Loader2 v-if="sourceLoading && packageSource === source.key" class="w-3.5 h-3.5 animate-spin" />
              <template v-else>
                <Shield v-if="source.type === 'official'" class="w-3.5 h-3.5" />
                <ShoppingBag v-else class="w-3.5 h-3.5" />
              </template>
              {{ source.label }}
            </span>
          </button>
        </div>
      </template>
    </PageHeader>

    <!-- 加载状态 -->
    <Card v-if="loading" padding="lg" class="rounded-2xl text-center py-16 my-auto">
      <div class="flex flex-col items-center justify-center space-y-3">
        <div class="w-12 h-12 rounded-full bg-[var(--bg-surface-soft)] flex items-center justify-center text-[var(--accent)]">
          <Loader2 class="w-6 h-6 animate-spin" />
        </div>
        <div class="text-sm font-medium text-[var(--text-secondary)]">
          {{ $t('common.loading') }}
        </div>
      </div>
    </Card>

    <!-- 主表单 -->
    <form v-else class="lg:flex-1 lg:min-h-0 lg:flex lg:flex-col" @submit.prevent="handleSubmit">
      <div class="lg:flex lg:flex-1 lg:min-h-0 lg:gap-6">
        <!-- LEFT: 选择区（独立滚动）-->
        <div class="apple-pane space-y-4 mb-4 lg:mb-0 lg:flex-[3] lg:overflow-y-auto lg:pr-1 lg:pb-4 scrollbar-hide">
          <RegionSelector
            v-if="regions.length > 0"
            :regions="regions"
            :selected-region="selectedRegion"
            :loading="regionsLoading"
            :title="getCreatePageText('selectRegion')"
            :package-count-label-key="packageCountLabelKey"
            @select="selectRegion"
          />
          <PackageSelector
            :packages="filteredPackages"
            :selected-package-id="form.packageId"
            :step-number="regions.length > 0 ? 2 : 1"
            :title="getCreatePageText('selectPackage')"
            :empty-message="getCreatePageText('noPackages')"
            :sold-out-label="getCreatePageText('planSoldOut')"
            @select="selectPackage"
          />
          <PlanSelector
            v-if="isPaidPackage"
            :plans="packagePlans"
            :package-gpu-shared="selectedPackage?.gpu_shared"
            :host-id="form.hostId"
            :selected-plan-id="form.planId"
            :instance-type="(selectedPackage?.instance_type === 'vm' ? 'vm' : 'container')"
            :loading="plansLoading"
            :step-number="regions.length > 0 ? 3 : 2"
            :prerequisite-message="prerequisiteMissing ? prerequisiteMessage : null"
            :free-site-mode="configStore.freeSiteMode"
            :title="getCreatePageText('selectPlan')"
            :description="getCreatePageText('planDesc')"
            :empty-message="getCreatePageText('noPlans')"
            :sold-out-label="getCreatePageText('planSoldOut')"
            :custom-plan-hint="getCreatePageText('customPlanHint')"
            @select="selectPlan"
          />
        </div>

        <!-- RIGHT: 配置区（独立滚动）-->
        <div ref="rightPanelScrollRef" class="apple-pane lg:flex-[2] lg:overflow-y-auto lg:pb-4 scrollbar-hide space-y-4">
          <!-- 前置套餐缺失提示 -->
          <div
            v-if="!isPaidPackage && prerequisiteMissing"
            class="rounded-2xl border border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-4 text-sm text-[var(--warning)] flex items-start gap-3"
          >
            <AlertCircle class="w-5 h-5 shrink-0 mt-0.5" />
            <div class="leading-relaxed font-medium">{{ prerequisiteMessage }}</div>
          </div>

          <!-- 免费套餐资源滑块 -->
          <ResourceSliders
            v-if="!isPaidPackage"
            :selected-package="selectedPackage || null"
            :user-quota="userQuota"
            :cpu="form.cpu"
            :memory="form.memory"
            :disk="form.disk"
            :step-number="regions.length > 0 ? 3 : 2"
            @update:cpu="form.cpu = $event"
            @update:memory="form.memory = $event"
            @update:disk="form.disk = $event"
          />

          <!-- 宿主机选择 -->
          <HostSelector
            :available-hosts="availableHosts"
            :selected-host-id="form.hostId"
            :hosts-loading="hostsLoading"
            :hosts-insufficient="hostsInsufficient"
            :unavailable-message="hostLookupMessage"
            :cpu="form.cpu"
            :memory="form.memory"
            :selected-package="selectedPackage || null"
            :show-deduction="!isPaidPackage || form.planId !== null"
            :title="getCreatePageText('selectHost')"
            :auto-selected-label="getCreatePageText('hostAutoSelected')"
            @update:selected-host-id="form.hostId = $event"
          />

          <!-- GPU镜像要求提示 -->
          <p
            v-if="selectedPlan?.gpuShared ?? selectedPackage?.gpu_shared"
            class="px-1 text-sm text-[var(--text-secondary)] flex items-center gap-2"
          >
            <Info class="w-4 h-4 shrink-0 text-[var(--accent)]" />
            <span>{{ t('gpu.imageRequirement') }}</span>
          </p>

          <!-- 系统镜像选择 -->
          <ImageSelector
            :available-images="availableImages"
            :selected-image="form.image"
            :images-loading="imagesLoading"
            :step-number="regions.length > 0 ? 4 : 3"
            :title="getCreatePageText('selectSystem')"
            :empty-message="getCreatePageText('noImages')"
            @update:selected-image="form.image = $event"
          />

          <!-- 自定义初始化脚本 -->
          <Card v-if="form.image" padding="md" class="rounded-2xl">
            <InitCommandSelector v-model="form.customInitCommandIds" :distro="selectedImageDistro" />
          </Card>

          <!-- 步骤：认证方式 -->
          <Card padding="md" class="rounded-2xl">
            <div class="flex items-center justify-between gap-3 mb-5">
              <div class="flex items-center gap-2.5">
                <Badge variant="default" shape="pill" size="md">
                  {{ regions.length > 0 ? 5 : 4 }}
                </Badge>
                <h2 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ $t('instance.createPage.authMethod') }}
                </h2>
              </div>

              <!-- 认证方式切换 (Apple Segmented Pills) -->
              <div class="inline-flex items-center p-1 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] text-xs">
                <button
                  type="button"
                  class="min-h-[36px] px-4 py-1.5 rounded-full font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98]"
                  :class="authType === 'ssh' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="authType = 'ssh'"
                >
                  {{ $t('instance.createPage.authMethodSsh') }}
                </button>
                <button
                  type="button"
                  class="min-h-[36px] px-4 py-1.5 rounded-full font-medium transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98]"
                  :class="authType === 'password' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="authType = 'password'"
                >
                  {{ $t('instance.createPage.authMethodPassword') }}
                </button>
              </div>
            </div>

            <!-- SSH 密钥选择 -->
            <div v-if="authType === 'ssh'">
              <div v-if="sshKeys.length === 0" class="text-center py-8 px-4 rounded-xl border border-dashed border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <div class="w-12 h-12 mx-auto mb-3 rounded-full bg-[var(--bg-tertiary)] flex items-center justify-center text-[var(--text-secondary)]">
                  <KeyRound class="w-6 h-6 stroke-[1.8]" />
                </div>
                <p class="text-sm font-medium text-[var(--text-primary)] mb-1">{{ $t('instance.createPage.noSshKeyConfigured') || '未检测到 SSH 密钥' }}</p>
                <p class="text-xs text-[var(--text-secondary)] max-w-md mx-auto mb-4 leading-relaxed">{{ $t('instance.createPage.noSshKeyHint') }}</p>
                <div class="flex items-center justify-center gap-3">
                  <Button
                    type="button"
                    variant="primary"
                    size="sm"
                    @click="authType = 'password'"
                  >
                    {{ $t('instance.createPage.switchToPassword') }}
                  </Button>
                  <router-link :to="profilePath()">
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                    >
                      {{ $t('instance.createPage.profileSettings') }}
                    </Button>
                  </router-link>
                </div>
              </div>
              <div v-else>
                <SSHKeySelector
                  :ssh-keys="sshKeys"
                  :selected-key-id="form.sshKeyId"
                  :show-header="false"
                  @update:selected-key-id="form.sshKeyId = $event"
                />
              </div>
            </div>

            <!-- 密码认证 -->
            <div v-else class="space-y-4">
              <!-- 模式切换：随机生成 vs 自定义 -->
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <div
                  class="p-4 rounded-xl border cursor-pointer transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98] flex items-center gap-3"
                  :class="passwordMode === 'auto'
                    ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)]'"
                  @click="passwordMode = 'auto'"
                >
                  <div class="w-4 h-4 rounded-full border flex items-center justify-center shrink-0" :class="passwordMode === 'auto' ? 'border-[var(--accent)]' : 'border-[var(--border-color)]'">
                    <div v-if="passwordMode === 'auto'" class="w-2 h-2 rounded-full bg-[var(--accent)]" />
                  </div>
                  <div class="min-w-0">
                    <div class="text-sm font-semibold text-[var(--text-primary)]">{{ $t('instance.createPage.passwordModeAuto') }}</div>
                    <div class="text-xs text-[var(--text-secondary)] truncate mt-0.5">{{ $t('instance.createPage.passwordModeAutoDesc') }}</div>
                  </div>
                </div>

                <div
                  class="p-4 rounded-xl border cursor-pointer transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.98] flex items-center gap-3"
                  :class="passwordMode === 'custom'
                    ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)]'"
                  @click="passwordMode = 'custom'"
                >
                  <div class="w-4 h-4 rounded-full border flex items-center justify-center shrink-0" :class="passwordMode === 'custom' ? 'border-[var(--accent)]' : 'border-[var(--border-color)]'">
                    <div v-if="passwordMode === 'custom'" class="w-2 h-2 rounded-full bg-[var(--accent)]" />
                  </div>
                  <div class="min-w-0">
                    <div class="text-sm font-semibold text-[var(--text-primary)]">{{ $t('instance.createPage.passwordModeCustom') }}</div>
                    <div class="text-xs text-[var(--text-secondary)] truncate mt-0.5">{{ $t('instance.createPage.passwordModeCustomDesc') }}</div>
                  </div>
                </div>
              </div>

              <!-- 自动生成提示 -->
              <div v-if="passwordMode === 'auto'" class="p-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-xs text-[var(--text-secondary)] flex items-start gap-3">
                <Info class="w-4 h-4 text-[var(--accent)] mt-0.5 shrink-0" />
                <span class="leading-relaxed">{{ $t('instance.createPage.autoPasswordHint') }}</span>
              </div>

              <!-- 自定义密码输入 -->
              <div v-else class="space-y-2">
                <div class="flex items-end gap-2.5">
                  <Input
                    v-model="customPassword"
                    :type="showPassword ? 'text' : 'password'"
                    :label="$t('instance.createPage.passwordLabel') || 'Root 密码'"
                    :placeholder="$t('instance.createPage.passwordPlaceholder')"
                    autocomplete="new-password"
                    class="flex-1 font-mono text-sm"
                  >
                    <template #suffix>
                      <button
                        type="button"
                        class="text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors focus:outline-none"
                        :aria-label="showPassword ? 'Hide password' : 'Show password'"
                        @click="showPassword = !showPassword"
                      >
                        <EyeOff v-if="showPassword" class="w-4 h-4" />
                        <Eye v-else class="w-4 h-4" />
                      </button>
                    </template>
                  </Input>
                  <Button
                    type="button"
                    variant="secondary"
                    size="md"
                    class="shrink-0 mb-0.5"
                    @click="generateRandomCustomPassword"
                  >
                    <template #icon>
                      <RotateCw class="w-4 h-4" />
                    </template>
                    <span>{{ $t('instance.createPage.generatePassword') }}</span>
                  </Button>
                </div>

                <!-- 弱密码提示 -->
                <div v-if="customPassword && !isCustomPasswordValid" class="text-xs text-[var(--error)] flex items-center gap-1.5 mt-1 font-medium">
                  <AlertCircle class="w-3.5 h-3.5 shrink-0" />
                  <span>{{ $t('instance.createPage.passwordTooWeak') }}</span>
                </div>
              </div>
            </div>
          </Card>

          <!-- 订单概览 (付费套餐且已选方案) -->
          <Card v-if="isPaidPackage && form.planId && selectedPlan" padding="md" class="rounded-2xl">
            <div class="flex items-center gap-3.5 mb-5">
              <div class="w-10 h-10 rounded-xl flex items-center justify-center shrink-0 bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)]">
                <ReceiptText class="w-5 h-5 stroke-[1.8]" />
              </div>
              <div class="min-w-0">
                <h3 class="text-base font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ configStore.freeSiteMode ? freeSiteCopy.createOrderSummary : $t('instance.createPage.orderSummary') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] truncate mt-0.5">
                  {{ selectedPackage?.name }} · {{ selectedPlan.name }}
                </p>
              </div>
            </div>

            <div class="space-y-4">
              <!-- 方案配置参数网格 -->
              <div class="p-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <div class="flex items-center justify-between mb-4">
                  <Badge variant="default" size="sm">
                    {{ getBillingCycleLabel(selectedPlan.billingCycle) }}
                  </Badge>
                  <Badge v-if="selectedPlan.slaGuarantee" variant="success" size="sm">
                    <ShieldCheck class="w-3.5 h-3.5 mr-1 inline" />
                    SLA {{ selectedPlan.slaGuarantee }}%
                  </Badge>
                </div>

                <div class="grid grid-cols-2 gap-2.5 text-center">
                  <div class="p-3 rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] flex flex-col justify-center">
                    <div class="text-base font-mono font-semibold tabular-nums whitespace-nowrap text-[var(--text-primary)]">
                      {{ formatCpu(selectedPlan.cpu) }}
                    </div>
                    <div class="text-xs text-[var(--text-secondary)] mt-0.5 font-medium">CPU</div>
                  </div>

                  <div class="p-3 rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] flex flex-col justify-center">
                    <div class="text-base font-mono font-semibold tabular-nums whitespace-nowrap text-[var(--text-primary)]">
                      {{ formatMemory(selectedPlan.memory) }}
                    </div>
                    <div class="text-xs text-[var(--text-secondary)] leading-tight mt-0.5 font-medium">
                      <div>{{ $t('instance.memory') }}</div>
                      <div v-if="selectedPackage?.instance_type !== 'vm' && selectedPlan.swapSize > 0" class="mt-0.5 flex items-center justify-center gap-1 text-[var(--success)] font-mono text-[10px]">
                        <span>SWAP</span>
                        <Check class="h-3 w-3" />
                      </div>
                    </div>
                  </div>

                  <div class="p-3 rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] flex flex-col justify-center">
                    <div class="text-base font-mono font-semibold tabular-nums whitespace-nowrap text-[var(--text-primary)]">
                      {{ formatDisk(selectedPlan.disk) }}
                    </div>
                    <div class="text-xs text-[var(--text-secondary)] mt-0.5 font-medium">{{ $t('instance.disk') }}</div>
                  </div>

                  <div class="p-3 rounded-lg bg-[var(--bg-surface)] border border-[var(--border-color)] flex flex-col justify-center">
                    <div class="text-base font-mono font-semibold tabular-nums whitespace-nowrap text-[var(--text-primary)]">
                      {{ formatTraffic(selectedPlan.trafficLimit) }}
                    </div>
                    <div class="text-xs text-[var(--text-secondary)] mt-0.5 font-medium">
                      {{ $t('billing.traffic') }} <span class="opacity-60">({{ $t('billing.trafficBidirectional') }})</span>
                    </div>
                  </div>

                  <GpuSpecification
                    v-if="selectedPlan.gpuShared ?? selectedPackage?.gpu_shared"
                    variant="card"
                    class="col-span-2"
                    :configurations="selectedPlan.gpuConfigurations"
                    :host-id="form.hostId"
                  />
                </div>

                <div class="flex flex-wrap items-center justify-center gap-x-6 gap-y-1.5 mt-3 pt-3 border-t border-[var(--border-color)] text-xs text-[var(--text-secondary)]">
                  <span><span class="opacity-75">{{ $t('instance.ports') }}:</span> <strong class="font-mono tabular-nums text-[var(--text-primary)] ml-1">{{ selectedPlan.portLimit }}</strong></span>
                  <span><span class="opacity-75">{{ $t('instance.snapshots') }}:</span> <strong class="font-mono tabular-nums text-[var(--text-primary)] ml-1">{{ selectedPlan.snapshotLimit }}</strong></span>
                </div>
              </div>

              <!-- 优惠码输入 -->
              <div class="space-y-1.5">
                <Input
                  v-model="form.promoCode"
                  type="text"
                  :label="configStore.freeSiteMode ? freeSiteCopy.createPromoCode : $t('aff.promoCode')"
                  :placeholder="configStore.freeSiteMode ? freeSiteCopy.createPromoPlaceholder : $t('aff.promoCodePlaceholder')"
                  :disabled="promoCodeVerifying"
                  @blur="verifyPromoCode"
                  @enter="verifyPromoCode"
                >
                  <template #suffix>
                    <Loader2 v-if="promoCodeVerifying" class="w-4 h-4 animate-spin text-[var(--text-tertiary)]" />
                    <CheckCircle2 v-else-if="promoCodeValid === true" class="w-4 h-4 text-[var(--success)]" />
                    <XCircle v-else-if="promoCodeValid === false" class="w-4 h-4 text-[var(--error)]" />
                  </template>
                </Input>
                <p v-if="promoCodeValid === true" class="text-xs text-[var(--success)] mt-1.5 flex items-center gap-1 font-medium">
                  <CheckCircle2 class="w-3.5 h-3.5" />
                  {{ configStore.freeSiteMode ? freeSiteCopy.createPromoValid.replace('{rate}', (promoCodeDiscount * 100).toFixed(0) + '%') : $t('aff.promoCodeValid', { rate: (promoCodeDiscount * 100).toFixed(0) + '%' }) }}
                </p>
                <p v-else-if="promoCodeError" class="text-xs text-[var(--error)] mt-1.5 font-medium flex items-center gap-1">
                  <AlertCircle class="w-3.5 h-3.5 shrink-0" />
                  {{ promoCodeError }}
                </p>
              </div>

              <!-- 价格明细 -->
              <div v-if="planPriceInfo" class="space-y-3 pt-2">
                <div v-if="promoCodeValid && planPriceInfo.discountAmount > 0" class="p-4 rounded-xl space-y-2 bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
                  <div class="flex justify-between text-sm">
                    <span class="text-[var(--text-secondary)]">{{ configStore.freeSiteMode ? freeSiteCopy.createPlanFee : $t('instance.createPage.planFee') }}</span>
                    <span class="font-mono tabular-nums font-medium text-[var(--text-primary)]">¥{{ planPriceInfo.planPrice.toFixed(2) }}</span>
                  </div>
                  <div class="flex justify-between text-sm pt-2 border-t border-[var(--border-color)]">
                    <span class="text-[var(--success)] flex items-center gap-1.5 font-medium">
                      <Tag class="w-3.5 h-3.5" />
                      {{ $t('aff.discountAmount') }} (-{{ (planPriceInfo.discountRate * 100).toFixed(0) }}%)
                    </span>
                    <span class="font-mono tabular-nums font-semibold text-[var(--success)]">-¥{{ planPriceInfo.discountAmount.toFixed(2) }}</span>
                  </div>
                </div>

                <!-- 最终价格 Hero 块 -->
                <div class="rounded-xl overflow-hidden border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                  <div class="p-5 flex items-center justify-between gap-4">
                    <div class="min-w-0">
                      <div class="text-sm font-semibold tracking-tight text-[var(--text-secondary)]">
                        {{ configStore.freeSiteMode ? freeSiteCopy.finalPrice : $t('aff.finalPrice') }}
                      </div>
                      <div v-if="selectedPlan.billingCycle > 1" class="text-xs mt-1 text-[var(--text-tertiary)]">
                        <span class="font-mono tabular-nums">
                          {{ configStore.freeSiteMode ? freeSiteCopy.createMonthlyEquivalent : `≈ ¥${(planPriceInfo.finalPrice / selectedPlan.billingCycle).toFixed(2)}/${$t('instance.createPage.month')}` }}
                        </span>
                      </div>
                    </div>
                    <div class="text-3xl md:text-4xl font-mono font-semibold tabular-nums tracking-tight text-[var(--text-primary)] flex items-baseline gap-1">
                      <span class="text-lg font-medium text-[var(--text-secondary)]">¥</span>
                      <span>{{ planPriceInfo.finalPrice.toFixed(2) }}</span>
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </Card>

          <!-- 提交区 -->
          <Card padding="md" class="mt-4 rounded-2xl">
            <div class="space-y-4">
            <!-- 实例名称 -->
            <div>
              <div
                v-if="showDestroyTrafficNotice"
                class="mb-3 p-3 rounded-xl border border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] text-xs text-[var(--warning)] leading-relaxed flex items-start gap-2"
              >
                <AlertTriangle class="w-4 h-4 mt-0.5 shrink-0" />
                <span>{{ $t('instance.createPage.destroyTrafficNotice') }}</span>
              </div>
              <div class="flex gap-2.5 items-end">
                <Input
                  v-model="form.name"
                  type="text"
                  :label="$t('instance.createPage.instanceName')"
                  :placeholder="$t('instance.createPage.instanceNamePlaceholder')"
                  class="flex-1 min-w-0"
                  @update:model-value="handleInstanceNameInput"
                />
                <Button
                  type="button"
                  variant="secondary"
                  size="md"
                  class="shrink-0 mb-0.5"
                  :disabled="!form.hostId"
                  @click="refreshGeneratedInstanceName(true)"
                >
                  <template #icon>
                    <RotateCw class="w-4 h-4" />
                  </template>
                  {{ $t('instance.createPage.regenerateName') }}
                </Button>
              </div>
              <p class="mt-1.5 text-xs text-[var(--text-tertiary)]">{{ $t('instance.createPage.autoNameHint') }}</p>
            </div>

            <!-- Missing SSH Key Alert -->
            <div
              v-if="authType === 'ssh' && sshKeys.length === 0"
              class="p-4 rounded-xl border border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] text-xs text-[var(--warning)]"
            >
              <p class="text-sm font-semibold mb-1">{{ $t('instance.createPage.missingSshKey') }}</p>
              <p class="leading-relaxed">
                {{ $t('instance.createPage.missingSshKeyDesc') }}
                <router-link :to="profilePath()" class="font-medium underline hover:brightness-110 ml-1">
                  {{ $t('instance.createPage.profileSettings') }}
                </router-link>
                {{ $t('instance.createPage.addSshKey') }}
              </p>
            </div>

            <!-- Quota check error -->
            <div
              v-if="!quotaCheck.valid"
              class="p-4 rounded-xl border border-[color-mix(in_srgb,var(--error)_30%,transparent)] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] text-xs text-[var(--error)]"
            >
              <p class="text-sm font-semibold mb-2 flex items-center gap-1.5">
                <AlertCircle class="w-4 h-4 shrink-0" />
                {{ $t('instance.createPage.quotaInsufficient') }}
              </p>
              <ul class="space-y-1 pl-4 list-disc font-medium">
                <li v-for="(err, idx) in quotaCheck.errors" :key="idx">{{ err }}</li>
              </ul>
            </div>

            <!-- Resource limit check error -->
            <div
              v-if="!resourceLimitCheck.valid"
              class="p-4 rounded-xl border border-[color-mix(in_srgb,var(--error)_30%,transparent)] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] text-xs text-[var(--error)]"
            >
              <p class="text-sm font-semibold mb-2 flex items-center gap-1.5">
                <AlertCircle class="w-4 h-4 shrink-0" />
                {{ $t('instance.createPage.resourceLimit.title') }}
              </p>
              <ul class="space-y-1 pl-4 list-disc font-medium">
                <li v-for="(err, idx) in resourceLimitCheck.errors" :key="idx">{{ err }}</li>
              </ul>
            </div>

            <!-- Own paid package warning -->
            <div
              v-if="isOwnPaidPackage"
              class="p-4 rounded-xl border border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] text-xs text-[var(--warning)] flex items-center gap-2"
            >
              <AlertTriangle class="w-4 h-4 shrink-0" />
              <span class="font-medium">{{ $t('instance.createPage.ownPaidPackageWarning') }}</span>
            </div>

            <!-- Prerequisite missing -->
            <div
              v-if="prerequisiteMissing"
              class="p-4 rounded-xl border border-[color-mix(in_srgb,var(--warning)_30%,transparent)] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] text-xs text-[var(--warning)] flex items-center gap-2"
            >
              <AlertTriangle class="w-4 h-4 shrink-0" />
              <span class="font-medium">{{ prerequisiteMessage }}</span>
            </div>

            <!-- 自动下发远程端口选项 -->
            <label class="flex items-start gap-3 p-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] cursor-pointer hover:border-[var(--border-hover)] transition-all duration-200">
              <input
                v-model="form.autoRemotePort"
                type="checkbox"
                class="mt-1 h-4 w-4 rounded text-[var(--accent)] border-[var(--border-color)] focus:ring-[var(--accent)] cursor-pointer"
              />
              <span class="text-sm select-none">
                <span class="font-medium text-[var(--text-primary)] block">{{ $t('instance.createPage.autoRemotePort') }}</span>
                <span class="mt-0.5 block text-xs text-[var(--text-secondary)] leading-relaxed">{{ $t('instance.createPage.autoRemotePortDesc') }}</span>
              </span>
            </label>

            <!-- Turnstile 验证组件 (严格保留所有 guard 要求) -->
            <div
              v-if="isCreateTurnstileRequired"
              ref="turnstileSectionRef"
              tabindex="-1"
              class="space-y-3 p-4 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-sm text-[var(--text-secondary)] focus:outline-none focus:ring-2 focus:ring-[var(--accent)]"
            >
              <div>
                <p class="font-medium text-[var(--text-primary)]">{{ $t('instance.createPage.turnstileRequiredTitle') }}</p>
                <p class="mt-1 text-xs text-[var(--text-secondary)]">{{ $t('instance.createPage.turnstileRequiredDesc') }}</p>
              </div>
              <TurnstileWidget
                ref="turnstileRef"
                v-model="turnstileToken"
                :site-key="createTurnstileSiteKey"
                :theme="themeStore.isDark ? 'dark' : 'light'"
                :action="turnstileAction"
                @verify="onTurnstileVerify"
                @expire="onTurnstileExpire"
                @error="onTurnstileError"
              />
            </div>

            <!-- 提交错误提示 -->
            <div
              v-if="error"
              class="space-y-2 rounded-xl border border-[color-mix(in_srgb,var(--error)_30%,transparent)] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-4 text-sm text-[var(--error)] flex items-start gap-2.5"
            >
              <AlertCircle class="w-5 h-5 shrink-0 mt-0.5" />
              <div class="font-medium leading-relaxed">{{ error }}</div>
            </div>

            <!-- 提交按钮 -->
            <Button
              type="submit"
              variant="primary"
              size="lg"
              block
              :disabled="!canSubmit"
              :loading="submitting"
              :title="submitDisabledReason"
            >
              {{ submitting ? $t('instance.createPage.creating') : $t('instance.create') }}
            </Button>
            </div>
          </Card>
        </div><!-- end right col -->
      </div><!-- end grid -->
    </form>
  </div>
</template>

<style scoped>
/* Apple style card entrance & physics */
.apple-pane :deep(.card),
.apple-pane :deep([class*="rounded-2xl"]) {
  transition: transform 0.3s cubic-bezier(0.25, 0.1, 0.25, 1), box-shadow 0.3s cubic-bezier(0.25, 0.1, 0.25, 1), border-color 0.3s cubic-bezier(0.25, 0.1, 0.25, 1);
}

@media (prefers-reduced-motion: reduce) {
  .apple-pane :deep(.card),
  .apple-pane :deep([class*="rounded-2xl"]) {
    transition: none !important;
    transform: none !important;
  }
}
</style>
