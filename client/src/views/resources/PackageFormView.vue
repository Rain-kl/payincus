<script setup lang="ts">
import PackageEgressFields from '@/components/PackageEgressFields.vue'
import GpuSharingFields from '@/components/GpuSharingFields.vue'
import { ref, computed, onMounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import api from '@/api'
import FlagIcon from '@/components/FlagIcon.vue'
import { useToast } from '@/stores/toast'
import { useAuthStore } from '@/stores/auth'
import type { Host, Package, CreatePackageRequest, UpdatePackageRequest } from '@/types/api'
import { validateName, validateText } from '@/utils/validation'
import { translateError } from '@/utils/errorHandler'
import { Badge, Button, Card, Input, PageHeader, Select } from '@/components/ui'
import type { BadgeVariant } from '@/components/ui'
import {
  Package as PackageIcon,
  Cpu,
  Layers,
  HardDrive,
  Network,
  Activity,
  Power,
  Link2,
  Eye,
  Sliders,
  Search,
  X,
  AlertCircle,
  Loader2,
  Box,
  Server,
  Check
} from 'lucide-vue-next'

// 为 KeepAlive exclude 匹配定义组件名称
defineOptions({
  name: 'PackageFormView'
})

const props = withDefaults(
  defineProps<{
    show?: boolean
    packageId?: number | null
  }>(),
  {
    show: false,
    packageId: null
  }
)

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void
  (e: 'close'): void
  (e: 'saved', pkg?: any): void
}>()

const { t } = useI18n()
const route = useRoute()
const toast = useToast()
const authStore = useAuthStore()

// Mode: create or edit
const packageId = computed(() => props.packageId ?? (route.params.id ? Number(route.params.id) : null))
const isEditMode = computed(() => !!packageId.value)
type PackageCreationMode = 'free' | 'paid'
const packageCreationMode = ref<PackageCreationMode>('free')
const showPackageLevelInstanceDefaults = computed(() => packageCreationMode.value === 'free')

// Loading states
const loading = ref(false)
const saving = ref(false)
type PackageHostOption = Host & {
  countryCode?: string
  cpuAllowanceMax?: number
  memoryMax?: number
  instanceType?: 'container' | 'vm' | 'both'
}

const hosts = ref<PackageHostOption[]>([])
const allKnownHosts = ref<PackageHostOption[]>([])
const hostsLoading = ref(false)
const hostSearchInput = ref('')
const hostSearchQuery = ref('')
const prerequisitePackages = ref<Package[]>([])
const currentPackageOwnerId = ref<number | null>(authStore.user?.id ?? null)

const prerequisitePackageOptions = computed(() => {
  const ownerId = currentPackageOwnerId.value ?? authStore.user?.id ?? null
  const currentPackageId = packageId.value
  const requiredPackageById = new Map(
    prerequisitePackages.value.map(pkg => [pkg.id, pkg.required_package_id ?? null])
  )
  const wouldCreateCycle = (candidateId: number): boolean => {
    if (!currentPackageId) return false

    let cursor = requiredPackageById.get(candidateId) ?? null
    const visited = new Set<number>()

    while (cursor !== null) {
      if (cursor === currentPackageId) return true
      if (visited.has(cursor)) return false
      visited.add(cursor)
      cursor = requiredPackageById.get(cursor) ?? null
    }

    return false
  }

  return prerequisitePackages.value.filter(pkg => {
    if (pkg.id === currentPackageId) return false
    if (wouldCreateCycle(pkg.id)) return false
    if (ownerId === null) return pkg.isOwn === true
    return pkg.ownerId === ownerId
  })
})

interface HostStoragePoolOption {
  name: string
  driver: string
  description: string
  status: string
  purpose?: 'instance_data' | 'instance_storage' | null
}

const storagePoolsByHost = ref<Record<number, HostStoragePoolOption[]>>({})
const storagePoolsLoadingByHost = ref<Record<number, boolean>>({})

// Form data
interface PackageForm {
  egressMode: string
  egressProfileId: number | null
  gpuShared: boolean
  gpuDeviceIds: number[]
  name: string
  description: string
  cpuMax: number
  memoryMax: number
  diskMax: number
  hostIds: number[]
  hostStoragePools: Record<string, string>
  hostTrafficMultipliers: Record<string, number>
  networkMode: 'nat' | 'nat_ipv6' | 'nat_ipv6_nat' | 'ipv6_only' | 'ipv6_nat' | 'public_ipv4' | 'public_ipv4_ipv6'
  instanceType: 'container' | 'vm'
  privileged: boolean
  nested: boolean
  active: boolean
  monthlyTrafficLimitGB: string
  trafficResetPrice: number
  // 实例配额
  portLimit: number | null
  snapshotLimit: number | null
  siteLimit: number | null
  // Storage I/O
  ioLimitMode: 'throughput' | 'iops'
  limitsRead: string
  limitsReadUnit: string
  limitsWrite: string
  limitsWriteUnit: string
  limitsReadIops: number
  limitsWriteIops: number
  // Network
  limitsIngress: string
  limitsIngressUnit: string
  limitsEgress: string
  limitsEgressUnit: string
  // Process/CPU
  limitsProcesses: number
  limitsCpuPriority: number
  // Boot settings
  bootAutostart: boolean
  bootAutostartPriority: number
  bootAutostartDelay: number
  bootHostShutdownTimeout: number
  // Global sharing
  globalShared: boolean
  globalMaxInstances: number
  requiredPackageId: number | null
}

const form = ref<PackageForm>(getDefaultForm())
const formError = ref('')

// Storage units
const storageUnits = ['B', 'kB', 'MB', 'GB', 'TB', 'PB']
const networkUnits = ['Mbit']
const publicPackageMaxInstanceOptions = [1, 2, 3, 4, 5]
const MAX_TRAFFIC_RESET_PRICE = 999999.99

// 网络模式（旧 IPv6 NAT 模式仅保留历史数据兼容，不再作为新套餐选项）
const networkModes = [
  { value: 'nat', labelKey: 'common.networkMode.nat' },
  { value: 'nat_ipv6', labelKey: 'common.networkMode.nat_ipv6' },
  { value: 'ipv6_only', labelKey: 'common.networkMode.ipv6_only' },
  { value: 'public_ipv4', labelKey: 'common.networkMode.public_ipv4' },
  { value: 'public_ipv4_ipv6', labelKey: 'common.networkMode.public_ipv4_ipv6' }
]
const disallowedKvmNetworkModes = new Set(['nat_ipv6_nat', 'ipv6_nat'])
const availableNetworkModes = computed(() => {
  if (form.value.instanceType === 'vm') {
    return networkModes.filter(mode => !disallowedKvmNetworkModes.has(mode.value))
  }
  return networkModes
})

const selectedHostIdSet = computed(() => new Set(form.value.hostIds))
const selectedKnownHosts = computed(() => {
  return form.value.hostIds
    .map(id => getHostById(id))
    .filter((host): host is PackageHostOption => host !== null)
})
const selectedUnknownHostIds = computed(() => form.value.hostIds.filter(id => !getHostById(id)))
const displayedHosts = computed(() => {
  const query = hostSearchQuery.value.trim().toLowerCase()
  if (!query) return hosts.value

  return hosts.value.filter(host => {
    const fields = [
      host.name,
      host.location,
      host.url,
      getHostCountryCode(host)
    ]
    return fields.some(value => String(value || '').toLowerCase().includes(query))
  })
})
const hasHostSearch = computed(() => hostSearchQuery.value.trim().length > 0)

watch(() => form.value.instanceType, (instanceType) => {
  if (instanceType === 'vm' && disallowedKvmNetworkModes.has(form.value.networkMode)) {
    form.value.networkMode = 'nat'
  }
}, { immediate: true })

function resetPackageLevelInstanceDefaults(): void {
  const defaults = getDefaultForm()
  form.value.cpuMax = defaults.cpuMax
  form.value.memoryMax = defaults.memoryMax
  form.value.diskMax = defaults.diskMax
  form.value.monthlyTrafficLimitGB = defaults.monthlyTrafficLimitGB
  form.value.portLimit = defaults.portLimit
  form.value.snapshotLimit = defaults.snapshotLimit
  form.value.siteLimit = defaults.siteLimit
  form.value.limitsIngress = defaults.limitsIngress
  form.value.limitsIngressUnit = defaults.limitsIngressUnit
  form.value.limitsEgress = defaults.limitsEgress
  form.value.limitsEgressUnit = defaults.limitsEgressUnit
}

watch(packageCreationMode, (mode) => {
  if (!loading.value && mode === 'paid') {
    resetPackageLevelInstanceDefaults()
  }
})

watch(() => form.value.globalShared, (globalShared) => {
  if (globalShared) {
    const value = Number(form.value.globalMaxInstances)
    if (!Number.isInteger(value) || value < 1 || value > 5) {
      form.value.globalMaxInstances = 1
    }
  }
})

function getDefaultForm(): PackageForm {
  return {
    egressMode: 'host',
    egressProfileId: null,
    gpuShared: false,
    gpuDeviceIds: [],
    name: '',
    description: '',
    cpuMax: 100,
    memoryMax: 4096,
    diskMax: 51200,
    hostIds: [],
    hostStoragePools: {},
    hostTrafficMultipliers: {},
    networkMode: 'nat',
    instanceType: 'container',
    privileged: false,
    nested: false,
    active: true,
    monthlyTrafficLimitGB: '',
    trafficResetPrice: 0,
    // 实例配额默认值
    portLimit: 20,
    snapshotLimit: 10,
    siteLimit: 10,
    // Storage I/O defaults
    ioLimitMode: 'throughput',
    limitsRead: '100',
    limitsReadUnit: 'MB',
    limitsWrite: '100',
    limitsWriteUnit: 'MB',
    limitsReadIops: 500,
    limitsWriteIops: 500,
    // Network defaults
    limitsIngress: '300',
    limitsIngressUnit: 'Mbit',
    limitsEgress: '300',
    limitsEgressUnit: 'Mbit',
    // Process/CPU defaults
    limitsProcesses: 500,
    limitsCpuPriority: 10,
    // Boot settings defaults
    bootAutostart: true,
    bootAutostartPriority: 20,
    bootAutostartDelay: 15,
    bootHostShutdownTimeout: 30,
    // Global sharing defaults
    globalShared: false,
    globalMaxInstances: 1,
    requiredPackageId: null
  }
}

// GB <-> Bytes conversion
const GB_TO_BYTES = BigInt(1024 * 1024 * 1024)

function bytesToGB(bytes: string | null | undefined): string {
  if (!bytes) return ''
  try {
    const b = BigInt(bytes)
    const gb = Number(b / GB_TO_BYTES)
    return gb.toString()
  } catch {
    return ''
  }
}

function gbToBytes(gb: string | number): string | null {
  if (gb === '' || gb === null || gb === undefined) return null
  const str = String(gb).trim()
  if (str === '') return null
  const num = parseFloat(str)
  if (isNaN(num) || num <= 0) return null
  return (BigInt(Math.round(num)) * GB_TO_BYTES).toString()
}

function normalizeTrafficMultiplier(value: unknown): number {
  const num = Number(value)
  if (!Number.isFinite(num) || num <= 0) return 1
  return Math.round(Math.min(Math.max(num, 0.001), 100) * 1000) / 1000
}

function normalizeMoneyYuan(value: unknown): number | null {
  const amount = Number(value)
  if (!Number.isFinite(amount) || amount < 0 || amount > MAX_TRAFFIC_RESET_PRICE) {
    return null
  }
  const rounded = Math.round(amount * 100) / 100
  if (Math.abs(amount - rounded) >= 1e-8) {
    return null
  }
  return rounded
}

// Parse storage value with unit (e.g., "100MB" -> { value: "100", unit: "MB" })
function parseStorageValue(value: string | null | undefined): { value: string; unit: string } {
  if (!value) return { value: '', unit: 'MB' }
  const match = value.match(/^(\d+(?:\.\d+)?)\s*(B|kB|MB|GB|TB|PB)$/i)
  if (match) {
    return { value: match[1], unit: match[2] }
  }
  return { value: value, unit: 'MB' }
}

// Parse network value with unit (e.g., "300Mbit" -> { value: "300", unit: "Mbit" })
// Only Mbit is supported; legacy Gbit values are automatically converted (×1000).
function parseNetworkValue(value: string | null | undefined): { value: string; unit: string } {
  if (!value) return { value: '', unit: 'Mbit' }
  const match = value.match(/^(\d+(?:\.\d+)?)\s*(Mbit|Gbit)$/i)
  if (match) {
    if (match[2].toLowerCase() === 'gbit') {
      return { value: String(Math.round(Number(match[1]) * 1000)), unit: 'Mbit' }
    }
    return { value: match[1], unit: 'Mbit' }
  }
  return { value: value, unit: 'Mbit' }
}

// Combine value and unit
function combineStorageValue(value: string, unit: string): string {
  if (!value || value.trim() === '') return ''
  return `${value}${unit}`
}

function getSystemStoragePools(hostId: number): HostStoragePoolOption[] {
  return (storagePoolsByHost.value[hostId] || []).filter(pool => pool.purpose === 'instance_data')
}

function rememberHosts(nextHosts: PackageHostOption[]): void {
  const merged = new Map<number, PackageHostOption>()
  allKnownHosts.value.forEach(host => merged.set(host.id, host))
  nextHosts.forEach(host => merged.set(host.id, host))
  allKnownHosts.value = Array.from(merged.values())
}

function getHostById(hostId: number): PackageHostOption | null {
  return allKnownHosts.value.find(item => item.id === hostId) || hosts.value.find(item => item.id === hostId) || null
}

function getSelectedHostLabel(hostId: number): string {
  const host = getHostById(hostId)
  if (!host) return `Host #${hostId}`
  return host.location ? `${host.name} (${host.location})` : host.name
}

function getHostCountryCode(host: PackageHostOption | null): string {
  return host?.countryCode || host?.country_code || 'us'
}

function getHostStatusLabel(status: string | undefined): string {
  if (status === 'online') return t('resources.hosts.online')
  if (status === 'offline') return t('resources.hosts.offline')
  if (status === 'maintenance') return t('resources.hosts.maintenance')
  return status || '-'
}

function getHostBadgeVariant(status: string | undefined): BadgeVariant {
  if (status === 'online') return 'success'
  if (status === 'maintenance') return 'warning'
  return 'default'
}

function getHostSummary(host: PackageHostOption): string {
  return [
    host.location,
    host.architecture,
    host.instanceType || host.instance_type
  ].filter(Boolean).join(' · ')
}

function toggleHostSelection(hostId: number): void {
  const selected = new Set(form.value.hostIds)
  if (selected.has(hostId)) {
    selected.delete(hostId)
  } else {
    selected.add(hostId)
  }
  form.value.hostIds = Array.from(selected)
}

function applyHostSearch(): void {
  hostSearchQuery.value = hostSearchInput.value
}

function clearHostSearch(): void {
  hostSearchInput.value = ''
  hostSearchQuery.value = ''
}

async function loadHostStoragePools(hostId: number): Promise<void> {
  if (storagePoolsByHost.value[hostId] || storagePoolsLoadingByHost.value[hostId]) {
    return
  }

  storagePoolsLoadingByHost.value = {
    ...storagePoolsLoadingByHost.value,
    [hostId]: true
  }

  try {
    const response = await api.hosts.getStoragePools(hostId)
    storagePoolsByHost.value = {
      ...storagePoolsByHost.value,
      [hostId]: response.pools || []
    }
  } catch (err) {
    console.error(`Failed to load storage pools for host ${hostId}:`, err)
    storagePoolsByHost.value = {
      ...storagePoolsByHost.value,
      [hostId]: []
    }
  } finally {
    storagePoolsLoadingByHost.value = {
      ...storagePoolsLoadingByHost.value,
      [hostId]: false
    }
  }
}

async function syncHostStoragePoolSelections(hostIds: number[]): Promise<void> {
  const requestedHostIds = [...new Set(hostIds)]
  await Promise.all(requestedHostIds.map(hostId => loadHostStoragePools(hostId)))

  const uniqueHostIds = [...new Set(form.value.hostIds)]
  const nextSelections: Record<string, string> = {}
  for (const hostId of uniqueHostIds) {
    const key = String(hostId)
    const currentSelection = form.value.hostStoragePools[key] || ''
    const systemPools = getSystemStoragePools(hostId)
    const hasCurrentSelection = currentSelection
      ? systemPools.some(pool => pool.name === currentSelection)
      : false

    if (hasCurrentSelection) {
      nextSelections[key] = currentSelection
      continue
    }

    if (systemPools.length === 1) {
      nextSelections[key] = systemPools[0].name
      continue
    }

    nextSelections[key] = ''
  }

  form.value.hostStoragePools = nextSelections

  const nextMultipliers: Record<string, number> = {}
  for (const hostId of uniqueHostIds) {
    const key = String(hostId)
    nextMultipliers[key] = normalizeTrafficMultiplier(form.value.hostTrafficMultipliers[key])
  }
  form.value.hostTrafficMultipliers = nextMultipliers
}

async function initForm(): Promise<void> {
  formError.value = ''
  saving.value = false
  await Promise.all([
    loadHosts(),
    loadPrerequisitePackages()
  ])
  if (packageId.value) {
    loading.value = true
    form.value = getDefaultForm()
    await loadPackage(packageId.value)
  } else {
    form.value = getDefaultForm()
    packageCreationMode.value = 'free'
    currentPackageOwnerId.value = authStore.user?.id ?? null
  }
}

onMounted(async () => {
  if (props.show || route.params.id) {
    await initForm()
  }
})

watch(() => props.show, async (shown) => {
  if (shown) {
    await initForm()
  }
})

watch(() => props.packageId, async () => {
  if (props.show) {
    await initForm()
  }
})

watch(() => route.params.id, async (newId, oldId) => {
  if (newId !== oldId) {
    await initForm()
  }
})

watch(() => form.value.hostIds.slice(), (hostIds) => {
  void syncHostStoragePoolSelections(hostIds)
})

async function loadHosts(): Promise<void> {
  hostsLoading.value = true
  try {
    const canBindAnyHost = authStore.isAdmin
    const params = canBindAnyHost
      ? { pageSize: '1000', scope: 'official' }
      : { mine: 'true', pageSize: '1000' }
    const response = await api.hosts.list(params)
    hosts.value = ((response as { hosts?: PackageHostOption[] }).hosts || [])
    rememberHosts(hosts.value)
  } catch (err) {
    console.error('Failed to load hosts:', err)
  } finally {
    hostsLoading.value = false
  }
}

async function loadPrerequisitePackages(): Promise<void> {
  try {
    const response = await api.packages.list({ all: true })
    prerequisitePackages.value = (response as { packages?: Package[] }).packages || []
  } catch (err) {
    console.error('Failed to load prerequisite packages:', err)
    prerequisitePackages.value = []
  }
}

async function loadPackage(id: number): Promise<void> {
  loading.value = true
  try {
    const response = await api.packages.get(id) as any
    const pkg = response.package || response
    currentPackageOwnerId.value = pkg.ownerId ?? pkg.user_id ?? authStore.user?.id ?? null
    
    // Parse storage I/O values
    const readParsed = parseStorageValue(pkg.limits_read)
    const writeParsed = parseStorageValue(pkg.limits_write)
    const ingressParsed = parseNetworkValue(pkg.limits_ingress)
    const egressParsed = parseNetworkValue(pkg.limits_egress)
    
    form.value = {
      egressMode: pkg.egress_mode ?? 'host',
      egressProfileId: pkg.egress_profile_id ?? null,
      gpuShared: pkg.gpu_shared ?? false,
      gpuDeviceIds: pkg.gpu_device_ids ?? [],
      name: pkg.name || '',
      description: pkg.description || '',
      cpuMax: Number(pkg.cpu_max) || 100,
      memoryMax: Number(pkg.memory_max) || 4096,
      diskMax: Number(pkg.disk_max) || 51200,
      hostIds: pkg.host_ids || [],
      hostStoragePools: Object.fromEntries(
        Object.entries((pkg.host_storage_pools || {}) as Record<string, string | null>)
          .map(([hostId, poolName]) => [hostId, poolName || ''])
      ),
      hostTrafficMultipliers: Object.fromEntries(
        Object.entries((pkg.host_traffic_multipliers || {}) as Record<string, number>)
          .map(([hostId, multiplier]) => [hostId, normalizeTrafficMultiplier(multiplier)])
      ),
      networkMode: pkg.network_mode || 'nat',
      instanceType: (pkg.instance_type === 'vm' ? 'vm' : 'container') as 'container' | 'vm',
      privileged: pkg.privileged === 1 || pkg.privileged === true,
      nested: pkg.nested === 1 || pkg.nested === true,
      active: pkg.active === 1 || pkg.active === true,
      monthlyTrafficLimitGB: bytesToGB(pkg.monthly_traffic_limit),
      trafficResetPrice: Number(pkg.traffic_reset_price || 0),
      // 实例配额
      portLimit: pkg.port_limit ?? 20,
      snapshotLimit: pkg.snapshot_limit ?? 10,
      siteLimit: pkg.site_limit ?? 10,
      // Storage I/O
      ioLimitMode: (pkg.io_limit_mode === 'iops' ? 'iops' : 'throughput') as 'throughput' | 'iops',
      limitsRead: readParsed.value || '100',
      limitsReadUnit: readParsed.unit || 'MB',
      limitsWrite: writeParsed.value || '100',
      limitsWriteUnit: writeParsed.unit || 'MB',
      limitsReadIops: Number(pkg.limits_read_iops) || 500,
      limitsWriteIops: Number(pkg.limits_write_iops) || 500,
      // Network
      limitsIngress: ingressParsed.value || '300',
      limitsIngressUnit: ingressParsed.unit || 'Mbit',
      limitsEgress: egressParsed.value || '300',
      limitsEgressUnit: egressParsed.unit || 'Mbit',
      // Process/CPU
      limitsProcesses: Number(pkg.limits_processes) || 500,
      limitsCpuPriority: Number(pkg.limits_cpu_priority) || 10,
      // Boot settings
      bootAutostart: pkg.boot_autostart === 1 || pkg.boot_autostart === true || pkg.boot_autostart == null,
      bootAutostartPriority: Number(pkg.boot_autostart_priority) || 20,
      bootAutostartDelay: Number(pkg.boot_autostart_delay) || 15,
      bootHostShutdownTimeout: Number(pkg.boot_host_shutdown_timeout) || 30,
      // Global sharing
      globalShared: pkg.global_shared === true || pkg.global_shared === 1,
      globalMaxInstances: Number.isInteger(Number(pkg.global_max_instances)) && Number(pkg.global_max_instances) >= 1 && Number(pkg.global_max_instances) <= 5 ? Number(pkg.global_max_instances) : 1,
      requiredPackageId: pkg.required_package_id ?? null
    }

    try {
      const plansRes = await api.packages.getPlans(id)
      if (plansRes?.plans && plansRes.plans.length > 0) {
        packageCreationMode.value = 'paid'
      } else if (Number(pkg.cpu_max) === 100 && Number(pkg.memory_max) === 4096 && Number(pkg.disk_max) === 51200) {
        packageCreationMode.value = 'paid'
      } else {
        packageCreationMode.value = 'free'
      }
    } catch {
      packageCreationMode.value = 'free'
    }
  } catch (_err: any) {
    toast.error(t('admin.packages.loadFailed') || 'Failed to load package')
    closeDrawer()
  } finally {
    loading.value = false
  }
}

async function savePackage(): Promise<void> {
  formError.value = ''
  
  if (!form.value.name) {
    formError.value = t('admin.packages.enterName')
    return
  }

  const nameValidation = validateName(form.value.name, t('admin.packages.name'), 2, 64)
  if (!nameValidation.valid) {
    formError.value = nameValidation.message || t('admin.packages.enterName')
    return
  }

  if (form.value.description) {
    const descValidation = validateText(form.value.description, t('admin.packages.descLabel'), 500)
    if (!descValidation.valid) {
      formError.value = descValidation.message || ''
      return
    }
  }

  if (!form.value.hostIds || form.value.hostIds.length === 0) {
    formError.value = t('admin.packages.mustBindHost')
    return
  }

  const cpuPriority = Number(form.value.limitsCpuPriority) || 10
  const bootPriority = Number(form.value.bootAutostartPriority) || 20
  const startupDelay = Number(form.value.bootAutostartDelay) || 15
  const shutdownTimeout = Number(form.value.bootHostShutdownTimeout) || 30

  if (cpuPriority < 0 || cpuPriority > 10) {
    formError.value = t('packageForm.validation.cpuPriorityRange')
    return
  }

  if (bootPriority < 0 || bootPriority > 100) {
    formError.value = t('packageForm.validation.bootPriorityRange')
    return
  }

  if (startupDelay < 5 || startupDelay > 600) {
    formError.value = t('packageForm.validation.startupDelayRange')
    return
  }

  if (shutdownTimeout < 30 || shutdownTimeout > 600) {
    formError.value = t('packageForm.validation.shutdownTimeoutRange')
    return
  }

  if (form.value.portLimit !== null && form.value.portLimit !== undefined) {
    if (form.value.portLimit < 1) {
      formError.value = t('packageForm.validation.portLimitMin')
      return
    }
  }

  const globalMaxInstances = Number(form.value.globalMaxInstances)
  if (form.value.globalShared && (!Number.isInteger(globalMaxInstances) || globalMaxInstances < 1 || globalMaxInstances > 5)) {
    formError.value = t('packageForm.validation.globalMaxInstancesRange')
    return
  }

  const trafficResetPriceYuan = normalizeMoneyYuan(form.value.trafficResetPrice)
  if (trafficResetPriceYuan === null) {
    formError.value = `流量重置价格必须在 0-${MAX_TRAFFIC_RESET_PRICE.toFixed(2)} 元之间，且最多两位小数`
    return
  }

  form.value.limitsCpuPriority = cpuPriority
  form.value.bootAutostartPriority = bootPriority
  form.value.bootAutostartDelay = startupDelay
  form.value.bootHostShutdownTimeout = shutdownTimeout

  saving.value = true

  try {
    const trafficLimitBytes = gbToBytes(form.value.monthlyTrafficLimitGB)
    const hostStoragePools = Object.fromEntries(
      form.value.hostIds.map(hostId => {
        const selectedPool = form.value.hostStoragePools[String(hostId)] || ''
        return [String(hostId), selectedPool.trim() || null]
      })
    )
    const hostTrafficMultipliers = Object.fromEntries(
      form.value.hostIds.map(hostId => [
        String(hostId),
        normalizeTrafficMultiplier(form.value.hostTrafficMultipliers[String(hostId)])
      ])
    )
    
    const data: CreatePackageRequest | UpdatePackageRequest = {
      egressMode: form.value.egressMode,
      egressProfileId: form.value.egressMode === 'profile' ? form.value.egressProfileId : null,
      gpuShared: form.value.gpuShared,
      gpuDeviceIds: form.value.gpuDeviceIds,
      name: form.value.name,
      description: form.value.description || undefined,
      cpuMax: form.value.cpuMax,
      memoryMax: form.value.memoryMax,
      diskMax: form.value.diskMax,
      hostIds: form.value.hostIds,
      hostStoragePools,
      hostTrafficMultipliers,
      networkMode: form.value.networkMode,
      instanceType: form.value.instanceType,
      privileged: form.value.privileged,
      nested: form.value.nested,
      active: form.value.active,
      monthlyTrafficLimit: trafficLimitBytes,
      trafficResetPrice: trafficResetPriceYuan,
      portLimit: form.value.portLimit ?? undefined,
      snapshotLimit: form.value.snapshotLimit ?? undefined,
      siteLimit: form.value.siteLimit ?? undefined,
      ioLimitMode: form.value.ioLimitMode,
      limitsRead: combineStorageValue(form.value.limitsRead, form.value.limitsReadUnit) || undefined,
      limitsWrite: combineStorageValue(form.value.limitsWrite, form.value.limitsWriteUnit) || undefined,
      limitsReadIops: form.value.limitsReadIops,
      limitsWriteIops: form.value.limitsWriteIops,
      limitsIngress: combineStorageValue(form.value.limitsIngress, form.value.limitsIngressUnit) || undefined,
      limitsEgress: combineStorageValue(form.value.limitsEgress, form.value.limitsEgressUnit) || undefined,
      limitsProcesses: form.value.limitsProcesses,
      limitsCpuPriority: form.value.limitsCpuPriority,
      bootAutostart: form.value.bootAutostart,
      bootAutostartPriority: form.value.bootAutostartPriority,
      bootAutostartDelay: form.value.bootAutostartDelay,
      bootHostShutdownTimeout: form.value.bootHostShutdownTimeout,
      globalShared: form.value.globalShared,
      globalQuotaMultiplier: null,
      globalMaxInstances: form.value.globalShared ? globalMaxInstances : null,
      requiredPackageId: form.value.requiredPackageId
    }

    if (isEditMode.value && packageId.value) {
      await api.packages.update(packageId.value, data as UpdatePackageRequest)
      toast.success(t('admin.packages.packageUpdated'))
    } else {
      await api.packages.create(data as CreatePackageRequest)
      toast.success(t('admin.packages.packageCreated'))
    }

    emit('saved')
    emit('update:show', false)
    emit('close')
  } catch (err: any) {
    formError.value = translateError(err) || t('admin.packages.saveFailed')
  } finally {
    saving.value = false
  }
}

function closeDrawer(): void {
  emit('update:show', false)
  emit('close')
}

const goBack = closeDrawer
</script>

<template>
  <DrawerModal
    :show="props.show"
    max-width="max-w-3xl lg:!max-w-4xl"
    raw
    @close="closeDrawer"
  >
    <!-- Header with Apple PageHeader -->
    <div class="modal-header border-b border-[var(--border-color)] bg-[var(--bg-surface)] px-6 py-4 shrink-0">
      <PageHeader
        :title="isEditMode ? t('packageForm.editTitle') : t('packageForm.createTitle')"
        :description="isEditMode ? (form.name ? `${form.name} (ID: ${packageId})` : '') : t('packageForm.hints.freePackageCreationMode')"
        :back="true"
        :back-label="t('common.close')"
        class="!mb-0 w-full"
        @back="goBack"
      >
        <template #actions>
          <Badge :variant="form.active ? 'success' : 'default'" size="md">
            {{ form.active ? t('admin.packages.active') : t('admin.packages.inactive') }}
          </Badge>
          <button
            type="button"
            class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-tertiary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] transition-colors active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
            :aria-label="t('common.close')"
            @click="goBack"
          >
            <X class="w-5 h-5" />
          </button>
        </template>
      </PageHeader>
    </div>

    <!-- Body -->
    <div class="modal-body p-6 space-y-6 overflow-y-auto flex-1 bg-[var(--bg-primary)]">
      <!-- Loading State -->
      <Card v-if="loading" padding="lg">
        <div class="py-12 flex flex-col items-center justify-center text-center">
          <Loader2 class="w-8 h-8 animate-spin text-[var(--accent)]" />
          <p class="text-sm text-[var(--text-secondary)] mt-4 font-medium">{{ t('common.loading') }}</p>
        </div>
      </Card>

      <!-- Form Content -->
      <form v-else id="package-form" class="space-y-6" novalidate @submit.prevent="savePackage">
        <!-- Error Alert -->
        <div v-if="formError" class="flex items-start gap-3 rounded-2xl border border-[var(--error)]/30 bg-[var(--error)]/10 p-4 text-sm text-[var(--error)]">
          <AlertCircle class="w-5 h-5 shrink-0 mt-0.5 text-[var(--error)]" />
          <div class="flex-1 font-medium leading-relaxed">{{ formError }}</div>
        </div>

        <!-- 1. 基本信息 Basic Info Section -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <PackageIcon class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.basicInfo') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.hints.freePackageCreationMode') }}
                </p>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <!-- Name -->
            <div class="md:col-span-2">
              <Input
                v-model="form.name"
                type="text"
                :label="`${t('admin.packages.name')} *`"
                :placeholder="t('admin.packages.enterName')"
                required
              />
            </div>

            <!-- Description -->
            <div class="md:col-span-2">
              <Input
                v-model="form.description"
                type="text"
                :label="t('admin.packages.descLabel')"
                :placeholder="t('admin.packages.descLabel')"
                :hint="t('common.noIncudalHint')"
              />
            </div>

            <!-- Package Creation Mode -->
            <div class="md:col-span-2 space-y-2.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                {{ t('packageForm.fields.packageCreationMode') }}
              </label>
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <div
                  class="group relative rounded-2xl border p-4 transition-all duration-300 cursor-pointer active:scale-[0.98]"
                  :class="packageCreationMode === 'free'
                    ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                  @click="packageCreationMode = 'free'"
                >
                  <div class="flex items-center justify-between gap-2 mb-2">
                    <div class="flex items-center gap-2.5">
                      <div
                        class="flex h-8 w-8 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] transition-colors"
                        :class="packageCreationMode === 'free' ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
                      >
                        <Box class="w-4 h-4" />
                      </div>
                      <span
                        class="text-sm font-semibold tracking-tight"
                        :class="packageCreationMode === 'free' ? 'text-[var(--accent)]' : 'text-[var(--text-primary)]'"
                      >
                        {{ t('packageForm.creationModes.free.title') }}
                      </span>
                    </div>
                    <Badge v-if="packageCreationMode === 'free'" variant="info" size="sm">
                      <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                    </Badge>
                  </div>
                  <p class="text-xs leading-relaxed text-[var(--text-secondary)]">
                    {{ t('packageForm.creationModes.free.description') }}
                  </p>
                </div>

                <div
                  class="group relative rounded-2xl border p-4 transition-all duration-300 cursor-pointer active:scale-[0.98]"
                  :class="packageCreationMode === 'paid'
                    ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                  @click="packageCreationMode = 'paid'"
                >
                  <div class="flex items-center justify-between gap-2 mb-2">
                    <div class="flex items-center gap-2.5">
                      <div
                        class="flex h-8 w-8 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] transition-colors"
                        :class="packageCreationMode === 'paid' ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
                      >
                        <Layers class="w-4 h-4" />
                      </div>
                      <span
                        class="text-sm font-semibold tracking-tight"
                        :class="packageCreationMode === 'paid' ? 'text-[var(--accent)]' : 'text-[var(--text-primary)]'"
                      >
                        {{ t('packageForm.creationModes.paid.title') }}
                      </span>
                    </div>
                    <Badge v-if="packageCreationMode === 'paid'" variant="info" size="sm">
                      <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                    </Badge>
                  </div>
                  <p class="text-xs leading-relaxed text-[var(--text-secondary)]">
                    {{ t('packageForm.creationModes.paid.description') }}
                  </p>
                </div>
              </div>

              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs text-[var(--text-secondary)] leading-relaxed">
                {{ packageCreationMode === 'free' ? t('packageForm.hints.freePackageCreationMode') : t('packageForm.hints.paidPackageCreationMode') }}
              </div>
            </div>

            <!-- Egress Fields -->
            <PackageEgressFields
              v-model:mode="form.egressMode"
              v-model:profile-id="form.egressProfileId"
              class="md:col-span-2"
              :host-ids="form.hostIds"
              :network-mode="form.networkMode"
              :instance-type="form.instanceType"
              :privileged="form.privileged"
            />

            <!-- Network Mode -->
            <div class="md:col-span-2">
              <Select
                v-model="form.networkMode"
                :label="`${t('packageForm.fields.networkMode')} *`"
              >
                <option v-for="mode in availableNetworkModes" :key="mode.value" :value="mode.value">
                  {{ t(mode.labelKey) }}
                </option>
              </Select>
            </div>

            <!-- Instance Type -->
            <div class="md:col-span-2 space-y-2.5">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                {{ t('packageForm.fields.instanceType') }} *
              </label>
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <div
                  class="group relative rounded-2xl border p-4 transition-all duration-300 cursor-pointer active:scale-[0.98]"
                  :class="form.instanceType === 'container'
                    ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                  @click="form.instanceType = 'container'"
                >
                  <div class="flex items-center justify-between gap-2 mb-2">
                    <div class="flex items-center gap-2.5">
                      <div
                        class="flex h-8 w-8 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] transition-colors"
                        :class="form.instanceType === 'container' ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
                      >
                        <Box class="w-4 h-4" />
                      </div>
                      <span
                        class="text-sm font-semibold tracking-tight"
                        :class="form.instanceType === 'container' ? 'text-[var(--accent)]' : 'text-[var(--text-primary)]'"
                      >
                        {{ t('common.instanceType.container') }}
                      </span>
                    </div>
                    <Badge v-if="form.instanceType === 'container'" variant="info" size="sm">
                      <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                    </Badge>
                  </div>
                  <ul class="text-xs space-y-1 text-[var(--text-secondary)]">
                    <li>• {{ t('packageForm.typeHelp.containerFast') }}</li>
                    <li>• {{ t('packageForm.typeHelp.containerLight') }}</li>
                    <li>• {{ t('packageForm.typeHelp.containerDocker') }}</li>
                  </ul>
                </div>

                <div
                  class="group relative rounded-2xl border p-4 transition-all duration-300 cursor-pointer active:scale-[0.98]"
                  :class="form.instanceType === 'vm'
                    ? 'border-[var(--accent)] bg-[var(--bg-surface-soft)] ring-1 ring-[var(--accent)] shadow-sm'
                    : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface-soft)]/50'"
                  @click="form.instanceType = 'vm'"
                >
                  <div class="flex items-center justify-between gap-2 mb-2">
                    <div class="flex items-center gap-2.5">
                      <div
                        class="flex h-8 w-8 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] transition-colors"
                        :class="form.instanceType === 'vm' ? 'text-[var(--accent)]' : 'text-[var(--text-tertiary)]'"
                      >
                        <Server class="w-4 h-4" />
                      </div>
                      <span
                        class="text-sm font-semibold tracking-tight"
                        :class="form.instanceType === 'vm' ? 'text-[var(--accent)]' : 'text-[var(--text-primary)]'"
                      >
                        {{ t('common.instanceType.vm') }}
                      </span>
                    </div>
                    <Badge v-if="form.instanceType === 'vm'" variant="info" size="sm">
                      <Check class="w-3.5 h-3.5 stroke-[2.5]" />
                    </Badge>
                  </div>
                  <ul class="text-xs space-y-1 text-[var(--text-secondary)]">
                    <li>• {{ t('packageForm.typeHelp.vmIsolation') }}</li>
                    <li>• {{ t('packageForm.typeHelp.vmKernel') }}</li>
                    <li>• {{ t('packageForm.typeHelp.vmWindows') }}</li>
                  </ul>
                </div>
              </div>
            </div>

            <!-- Bound Hosts -->
            <div class="md:col-span-2 space-y-3">
              <div class="flex items-center justify-between">
                <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                  {{ t('admin.packages.boundHosts') }} *
                </label>
                <Badge variant="info" size="sm">
                  {{ t('packageForm.hostSelector.selectedCount', { count: form.hostIds.length }) }}
                </Badge>
              </div>

              <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-3">
                <div class="flex flex-col sm:flex-row gap-2">
                  <Input
                    v-model="hostSearchInput"
                    :placeholder="t('packageForm.hostSelector.searchPlaceholder')"
                    class="flex-1"
                    @enter="applyHostSearch"
                  >
                    <template #prefix>
                      <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
                    </template>
                  </Input>
                  <div class="flex items-center gap-2 shrink-0">
                    <Button variant="secondary" size="md" type="button" @click="applyHostSearch">
                      {{ t('common.search') }}
                    </Button>
                    <Button v-if="hasHostSearch" variant="ghost" size="md" type="button" @click="clearHostSearch">
                      {{ t('resources.packages.clearSearch') }}
                    </Button>
                  </div>
                </div>

                <div class="max-h-72 overflow-y-auto pr-1 space-y-2">
                  <div v-if="hostsLoading" class="py-8 text-center text-sm text-[var(--text-secondary)]">
                    <Loader2 class="w-6 h-6 animate-spin mx-auto text-[var(--accent)] mb-2" />
                    {{ t('common.loading') }}
                  </div>
                  <div v-else-if="displayedHosts.length > 0" class="grid grid-cols-1 lg:grid-cols-2 gap-2">
                    <div
                      v-for="host in displayedHosts"
                      :key="host.id"
                      class="group rounded-xl border p-3 text-left transition-all duration-200 cursor-pointer active:scale-[0.99]"
                      :class="selectedHostIdSet.has(host.id)
                        ? 'border-[var(--accent)] bg-[var(--bg-surface)] ring-1 ring-[var(--accent)] shadow-sm'
                        : 'border-[var(--border-color)] bg-[var(--bg-surface)] hover:border-[var(--border-hover)]'"
                      @click="toggleHostSelection(host.id)"
                    >
                      <div class="flex items-start gap-3">
                        <input
                          :checked="selectedHostIdSet.has(host.id)"
                          type="checkbox"
                          class="mt-1 w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] pointer-events-none"
                          tabindex="-1"
                          readonly
                        />
                        <FlagIcon :code="getHostCountryCode(host)" size="sm" class="mt-0.5 shrink-0" />
                        <div class="min-w-0 flex-1">
                          <div class="flex min-w-0 items-center gap-2">
                            <span class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ host.name }}</span>
                            <Badge :variant="getHostBadgeVariant(host.status)" size="sm">
                              {{ getHostStatusLabel(host.status) }}
                            </Badge>
                          </div>
                          <div class="mt-1 truncate text-xs text-[var(--text-secondary)]">
                            {{ getHostSummary(host) || host.url }}
                          </div>
                        </div>
                      </div>
                    </div>
                  </div>
                  <div v-else class="py-8 text-center text-sm text-[var(--text-secondary)]">
                    {{ hasHostSearch ? t('packageForm.hostSelector.noSearchResults') : t('admin.packages.noHostsAvailable') }}
                  </div>
                </div>
              </div>
            </div>

            <!-- Host Storage Pools & Traffic Multipliers -->
            <div v-if="form.hostIds.length > 0" class="md:col-span-2 space-y-3">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                {{ t('packageForm.fields.hostStoragePools') }}
              </label>
              <div class="space-y-3 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
                <div
                  v-for="host in selectedKnownHosts"
                  :key="host.id"
                  class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 space-y-3"
                >
                  <div class="flex items-center justify-between gap-3">
                    <div class="flex min-w-0 items-center gap-2.5">
                      <FlagIcon :code="getHostCountryCode(host)" size="sm" class="shrink-0" />
                      <div class="min-w-0">
                        <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ getSelectedHostLabel(host.id) }}</div>
                        <div class="text-xs text-[var(--text-tertiary)] font-mono tabular-nums">ID: {{ host.id }}</div>
                      </div>
                    </div>
                    <span v-if="storagePoolsLoadingByHost[host.id]" class="text-xs text-[var(--text-tertiary)] flex items-center gap-1.5">
                      <Loader2 class="w-3.5 h-3.5 animate-spin text-[var(--accent)]" />
                      {{ t('common.loading') }}
                    </span>
                  </div>

                  <template v-if="getSystemStoragePools(host.id).length > 0">
                    <Select
                      v-model="form.hostStoragePools[String(host.id)]"
                      :hint="t('packageForm.hints.hostStoragePools')"
                    >
                      <option value="">{{ t('packageForm.placeholders.autoStoragePool') }}</option>
                      <option
                        v-for="pool in getSystemStoragePools(host.id)"
                        :key="pool.name"
                        :value="pool.name"
                      >
                        {{ pool.name }} ({{ pool.driver.toUpperCase() }})
                      </option>
                    </Select>
                  </template>
                  <p v-else-if="!storagePoolsLoadingByHost[host.id]" class="text-xs text-[var(--warning)] font-medium">
                    {{ t('packageForm.hints.noSystemStoragePools') }}
                  </p>

                  <div>
                    <Input
                      v-model.number="form.hostTrafficMultipliers[String(host.id)]"
                      type="number"
                      :label="t('packageForm.fields.hostTrafficMultiplier')"
                      min="0.001"
                      max="100"
                      step="0.001"
                      placeholder="1"
                      :hint="t('packageForm.hints.hostTrafficMultiplier')"
                      class="font-mono tabular-nums"
                    />
                  </div>
                </div>

                <div
                  v-for="hostId in selectedUnknownHostIds"
                  :key="hostId"
                  class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 space-y-3"
                >
                  <div class="flex items-center justify-between gap-3">
                    <div>
                      <div class="text-sm font-semibold text-[var(--text-primary)]">{{ getSelectedHostLabel(hostId) }}</div>
                      <div class="text-xs text-[var(--text-tertiary)] font-mono tabular-nums">ID: {{ hostId }} · {{ t('packageForm.hostSelector.detailUnavailable') }}</div>
                    </div>
                    <span v-if="storagePoolsLoadingByHost[hostId]" class="text-xs text-[var(--text-tertiary)] flex items-center gap-1.5">
                      <Loader2 class="w-3.5 h-3.5 animate-spin text-[var(--accent)]" />
                      {{ t('common.loading') }}
                    </span>
                  </div>

                  <template v-if="getSystemStoragePools(hostId).length > 0">
                    <Select
                      v-model="form.hostStoragePools[String(hostId)]"
                      :hint="t('packageForm.hints.hostStoragePools')"
                    >
                      <option value="">{{ t('packageForm.placeholders.autoStoragePool') }}</option>
                      <option
                        v-for="pool in getSystemStoragePools(hostId)"
                        :key="pool.name"
                        :value="pool.name"
                      >
                        {{ pool.name }} ({{ pool.driver.toUpperCase() }})
                      </option>
                    </Select>
                  </template>
                  <p v-else-if="!storagePoolsLoadingByHost[hostId]" class="text-xs text-[var(--warning)] font-medium">
                    {{ t('packageForm.hints.noSystemStoragePools') }}
                  </p>

                  <div>
                    <Input
                      v-model.number="form.hostTrafficMultipliers[String(hostId)]"
                      type="number"
                      :label="t('packageForm.fields.hostTrafficMultiplier')"
                      min="0.001"
                      max="100"
                      step="0.001"
                      placeholder="1"
                      :hint="t('packageForm.hints.hostTrafficMultiplier')"
                      class="font-mono tabular-nums"
                    />
                  </div>
                </div>
              </div>
            </div>

            <!-- Active Status Toggle -->
            <div class="md:col-span-2 pt-1">
              <label class="inline-flex items-center gap-3 cursor-pointer select-none">
                <input
                  v-model="form.active"
                  type="checkbox"
                  class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] transition-colors"
                />
                <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('admin.packages.activeLabel') }}</span>
              </label>
            </div>
          </div>
        </Card>

        <!-- 2. 资源配额 Resource Limits Section -->
        <Card v-if="showPackageLevelInstanceDefaults" padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Cpu class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.resourceLimits') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('admin.packages.instanceMaxLimit') }}
                </p>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div>
              <Input
                v-model.number="form.cpuMax"
                type="number"
                :label="`${t('admin.packages.cpu')} (%)`"
                :hint="t('admin.packages.instanceMaxLimit')"
                min="15"
                max="10000"
                step="5"
                class="font-mono tabular-nums"
              />
            </div>
            <div>
              <Input
                v-model.number="form.memoryMax"
                type="number"
                :label="`${t('admin.packages.memory')} (MB)`"
                :hint="t('admin.packages.instanceMaxLimit')"
                min="128"
                step="64"
                class="font-mono tabular-nums"
              />
            </div>
            <div>
              <Input
                v-model.number="form.diskMax"
                type="number"
                :label="`${t('admin.packages.disk')} (MB)`"
                :hint="t('admin.packages.instanceMaxLimit')"
                min="512"
                step="1024"
                class="font-mono tabular-nums"
              />
            </div>
            <div>
              <Input
                v-model="form.monthlyTrafficLimitGB"
                type="text"
                :label="`${t('admin.packages.trafficLimit')} (GB)`"
                :placeholder="t('admin.packages.unlimitedPlaceholder')"
                class="font-mono tabular-nums"
              />
            </div>
            <div class="md:col-span-2">
              <Input
                v-model.number="form.trafficResetPrice"
                type="number"
                label="流量重置价格（元）"
                min="0"
                :max="MAX_TRAFFIC_RESET_PRICE"
                step="0.01"
                hint="单位:元(如填 5 表示 5 元)；用户自助重置月流量时扣费，0 表示免费。"
                class="font-mono tabular-nums"
              />
            </div>
          </div>
        </Card>

        <!-- 3. 实例配额 Instance Quota Section -->
        <Card v-if="showPackageLevelInstanceDefaults" padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Layers class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.instanceQuota') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.hints.instanceQuota') }}
                </p>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 sm:grid-cols-3 gap-5">
            <div>
              <Input
                :model-value="form.portLimit ?? ''"
                type="number"
                :label="t('packageForm.fields.portLimit')"
                min="1"
                max="1000"
                :hint="t('packageForm.hints.portLimit')"
                class="font-mono tabular-nums"
                @update:model-value="form.portLimit = $event === '' ? null : Number($event)"
              />
            </div>
            <div>
              <Input
                :model-value="form.snapshotLimit ?? ''"
                type="number"
                :label="t('packageForm.fields.snapshotLimit')"
                min="0"
                max="1000"
                :hint="t('packageForm.hints.snapshotLimit')"
                class="font-mono tabular-nums"
                @update:model-value="form.snapshotLimit = $event === '' ? null : Number($event)"
              />
            </div>
            <div>
              <Input
                :model-value="form.siteLimit ?? ''"
                type="number"
                :label="t('packageForm.fields.siteLimit')"
                min="0"
                max="1000"
                :hint="t('packageForm.hints.siteLimit')"
                class="font-mono tabular-nums"
                @update:model-value="form.siteLimit = $event === '' ? null : Number($event)"
              />
            </div>
          </div>
        </Card>

        <!-- 4. 存储 I/O Storage I/O Section -->
        <Card padding="md">
          <template #header>
            <div class="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3 w-full">
              <div class="flex items-center gap-3">
                <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                  <HardDrive class="w-5 h-5 stroke-[2]" />
                </div>
                <div>
                  <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                    {{ t('packageForm.sections.storageIO') }}
                  </h3>
                  <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                    {{ t('packageForm.hints.ioLimitMode') }}
                  </p>
                </div>
              </div>
              <div class="inline-flex p-1 rounded-xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] self-start sm:self-auto">
                <button
                  type="button"
                  class="px-3.5 py-1.5 rounded-lg text-xs font-medium transition-all duration-200 active:scale-95 cursor-pointer"
                  :class="form.ioLimitMode === 'throughput'
                    ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
                    : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="form.ioLimitMode = 'throughput'"
                >
                  {{ t('packageForm.ioMode.throughput') }}
                </button>
                <button
                  type="button"
                  class="px-3.5 py-1.5 rounded-lg text-xs font-medium transition-all duration-200 active:scale-95 cursor-pointer"
                  :class="form.ioLimitMode === 'iops'
                    ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm'
                    : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                  @click="form.ioLimitMode = 'iops'"
                >
                  {{ t('packageForm.ioMode.iops') }}
                </button>
              </div>
            </div>
          </template>

          <!-- Throughput fields -->
          <div v-if="form.ioLimitMode === 'throughput'" class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div>
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none mb-1.5 block">
                {{ t('packageForm.fields.limitsRead') }}
              </label>
              <div class="flex gap-2">
                <Input v-model="form.limitsRead" type="text" class="flex-1 min-w-0 font-mono tabular-nums" />
                <Select v-model="form.limitsReadUnit" :options="storageUnits" :block="false" class="w-24 shrink-0" />
              </div>
            </div>
            <div>
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none mb-1.5 block">
                {{ t('packageForm.fields.limitsWrite') }}
              </label>
              <div class="flex gap-2">
                <Input v-model="form.limitsWrite" type="text" class="flex-1 min-w-0 font-mono tabular-nums" />
                <Select v-model="form.limitsWriteUnit" :options="storageUnits" :block="false" class="w-24 shrink-0" />
              </div>
            </div>
          </div>

          <!-- IOPS fields -->
          <div v-if="form.ioLimitMode === 'iops'" class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div>
              <Input
                v-model.number="form.limitsReadIops"
                type="number"
                :label="t('packageForm.fields.limitsReadIops')"
                min="0"
                class="font-mono tabular-nums"
              />
            </div>
            <div>
              <Input
                v-model.number="form.limitsWriteIops"
                type="number"
                :label="t('packageForm.fields.limitsWriteIops')"
                min="0"
                class="font-mono tabular-nums"
              />
            </div>
          </div>
        </Card>

        <!-- 5. 网络规格限制 Network Limits Section -->
        <Card v-if="showPackageLevelInstanceDefaults" padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Network class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.networkLimits') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.sections.networkLimits') }}
                </p>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div>
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none mb-1.5 block">
                {{ t('packageForm.fields.limitsIngress') }}
              </label>
              <div class="flex gap-2">
                <Input v-model="form.limitsIngress" type="text" class="flex-1 min-w-0 font-mono tabular-nums" />
                <Select v-model="form.limitsIngressUnit" :options="networkUnits" :block="false" class="w-28 shrink-0" />
              </div>
            </div>
            <div>
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none mb-1.5 block">
                {{ t('packageForm.fields.limitsEgress') }}
              </label>
              <div class="flex gap-2">
                <Input v-model="form.limitsEgress" type="text" class="flex-1 min-w-0 font-mono tabular-nums" />
                <Select v-model="form.limitsEgressUnit" :options="networkUnits" :block="false" class="w-28 shrink-0" />
              </div>
            </div>
          </div>
        </Card>

        <!-- 6. 进程与调度 Process and CPU Scheduling Section -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Activity class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.processScheduling') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.hints.cpuPriority') }}
                </p>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div>
              <Input
                v-model.number="form.limitsProcesses"
                type="number"
                :label="t('packageForm.fields.limitsProcesses')"
                min="1"
                class="font-mono tabular-nums"
              />
            </div>
            <div>
              <Input
                v-model.number="form.limitsCpuPriority"
                type="number"
                :label="`${t('packageForm.fields.limitsCpuPriority')} (0-10)`"
                :hint="t('packageForm.hints.cpuPriority')"
                min="0"
                max="10"
                class="font-mono tabular-nums"
              />
            </div>
          </div>
        </Card>

        <!-- 7. 启动与自启设置 Boot Settings Section -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Power class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.bootSettings') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.hints.startupDelay') }}
                </p>
              </div>
            </div>
          </template>

          <div class="grid grid-cols-1 md:grid-cols-2 gap-5">
            <div class="md:col-span-2">
              <label class="inline-flex items-center gap-3 cursor-pointer select-none">
                <input
                  v-model="form.bootAutostart"
                  type="checkbox"
                  class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] transition-colors"
                />
                <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('packageForm.fields.bootAutostart') }}</span>
              </label>
            </div>
            <div>
              <Input
                v-model.number="form.bootAutostartPriority"
                type="number"
                :label="`${t('packageForm.fields.bootAutostartPriority')} (0-100)`"
                :hint="t('packageForm.hints.bootPriority')"
                min="0"
                max="100"
                class="font-mono tabular-nums"
              />
            </div>
            <div>
              <Input
                v-model.number="form.bootAutostartDelay"
                type="number"
                :label="`${t('packageForm.fields.bootAutostartDelay')} (${t('packageForm.units.seconds')})`"
                :hint="t('packageForm.hints.startupDelay')"
                min="5"
                max="600"
                class="font-mono tabular-nums"
              />
            </div>
            <div class="md:col-span-2">
              <Input
                v-model.number="form.bootHostShutdownTimeout"
                type="number"
                :label="`${t('packageForm.fields.bootHostShutdownTimeout')} (${t('packageForm.units.seconds')})`"
                :hint="t('packageForm.hints.shutdownTimeout')"
                min="30"
                max="600"
                class="font-mono tabular-nums"
              />
            </div>
          </div>
        </Card>

        <!-- 8. 前置套餐依赖 Prerequisite Section -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Link2 class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.prerequisite') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.hints.requiredPackage') }}
                </p>
              </div>
            </div>
          </template>

          <div class="space-y-2">
            <Select
              :model-value="form.requiredPackageId ?? ''"
              :label="t('packageForm.fields.requiredPackage')"
              :hint="t('packageForm.hints.requiredPackage')"
              @update:model-value="form.requiredPackageId = $event ? Number($event) : null"
            >
              <option value="">{{ t('packageForm.placeholders.noPrerequisite') }}</option>
              <option
                v-for="pkg in prerequisitePackageOptions"
                :key="pkg.id"
                :value="pkg.id"
              >
                {{ pkg.name }}
              </option>
            </Select>
          </div>
        </Card>

        <!-- 9. 可见性与公有额度 Visibility Section -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Eye class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.visibility') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.hints.publicAccess') }}
                </p>
              </div>
            </div>
          </template>

          <div class="space-y-4">
            <label class="inline-flex items-center gap-3 cursor-pointer select-none">
              <input
                v-model="form.globalShared"
                type="checkbox"
                class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] transition-colors"
              />
              <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('packageForm.fields.publicAccess') }}</span>
            </label>

            <div v-if="form.globalShared" class="pl-5 border-l-2 border-[var(--accent)]/40 space-y-3">
              <Select
                :model-value="form.globalMaxInstances"
                :label="t('packageForm.fields.globalMaxInstances')"
                :hint="t('packageForm.hints.globalMaxInstances')"
                @update:model-value="form.globalMaxInstances = Number($event) || 1"
              >
                <option v-for="count in publicPackageMaxInstanceOptions" :key="count" :value="count">
                  {{ count }}
                </option>
              </Select>
            </div>
          </div>
        </Card>

        <!-- 10. 高级特性与硬件加速 Advanced Options Section -->
        <Card padding="md">
          <template #header>
            <div class="flex items-center gap-3">
              <div class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-[var(--bg-surface-soft)] text-[var(--accent)]">
                <Sliders class="w-5 h-5 stroke-[2]" />
              </div>
              <div>
                <h3 class="font-semibold text-base md:text-lg text-[var(--text-primary)] tracking-tight">
                  {{ t('packageForm.sections.advancedOptions') }}
                </h3>
                <p class="text-xs text-[var(--text-secondary)] mt-0.5">
                  {{ t('packageForm.sections.advancedOptions') }}
                </p>
              </div>
            </div>
          </template>

          <div class="space-y-5">
            <div class="flex flex-wrap items-center gap-6">
              <label class="inline-flex items-center gap-3 cursor-pointer select-none">
                <input
                  v-model="form.privileged"
                  type="checkbox"
                  class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] transition-colors"
                />
                <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('admin.packages.privileged') }}</span>
              </label>
              <label class="inline-flex items-center gap-3 cursor-pointer select-none">
                <input
                  v-model="form.nested"
                  type="checkbox"
                  class="w-4 h-4 rounded border-[var(--border-color)] text-[var(--accent)] focus:ring-[var(--accent)] transition-colors"
                />
                <span class="text-sm font-medium text-[var(--text-primary)]">{{ t('admin.packages.nested') }}</span>
              </label>
            </div>

            <!-- GPU Sharing Fields -->
            <div class="pt-2 border-t border-[var(--border-color)]">
              <GpuSharingFields
                :model-value="form.gpuShared"
                :device-ids="form.gpuDeviceIds"
                :host-ids="form.hostIds"
                :allowed="form.instanceType === 'container' && !form.privileged"
                @update:model-value="form.gpuShared = $event === true"
                @update:device-ids="form.gpuDeviceIds = $event"
              />
            </div>
          </div>
        </Card>
      </form>
    </div>

    <!-- Footer with Apple capsule buttons -->
    <div class="modal-footer border-t border-[var(--border-color)] bg-[var(--bg-surface)] px-6 py-4 flex items-center justify-end gap-3 shrink-0">
      <Button variant="secondary" size="md" type="button" @click="goBack">
        {{ t('common.cancel') }}
      </Button>
      <Button
        variant="primary"
        size="md"
        type="submit"
        form="package-form"
        :disabled="saving || !form.name"
        :loading="saving"
        @click="savePackage"
      >
        {{ isEditMode ? t('common.save') : t('common.create') }}
      </Button>
    </div>
  </DrawerModal>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  *,
  *::before,
  *::after {
    transition: none !important;
    animation: none !important;
    transform: none !important;
  }
}
</style>
