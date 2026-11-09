<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  CheckCircle2,
  ChevronRight,
  Coins,
  Copy,
  Plus,
  RotateCw,
  Search,
  Server,
  Sparkles,
  Tag
} from 'lucide-vue-next'
import api from '@/api/admin'
import { useToast } from '@/stores/toast'
import type {
  AdminPromoCode,
  AdminPromoBoundInstance,
  PromoCodeType,
  PromoDiscountType,
  PromoDurationType
} from '@/api/admin'
import type { Package } from '@/types/api'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Pagination,
  Select
} from '@/components/ui'
import DrawerModal from '@/components/DrawerModal.vue'
import { useReveal } from '@/composables/useReveal'

defineOptions({ name: 'AdminPromoCodesView' })

type PackagePlanItem = {
  id: number
  name: string
  cpu: number
  memory: number
  disk: number
  price: number
  billingCycle?: number
}

const { t } = useI18n()
const toast = useToast()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

// 优惠码列表与过滤状态
const promos = ref<AdminPromoCode[]>([])
const loading = ref(false)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const search = ref('')
const selectedType = ref<PromoCodeType | ''>('')
const selectedStatus = ref<'' | 'true' | 'false'>('')

// 过滤选项
const typeOptions = computed(() => [
  { value: '', label: t('promosAdmin.filters.typeAll') },
  { value: 'ADMIN_PROMO', label: t('promosAdmin.filters.typeAdmin') },
  { value: 'AFF_USER', label: t('promosAdmin.filters.typeAff') }
])

const statusOptions = computed(() => [
  { value: '', label: t('promosAdmin.filters.statusAll') },
  { value: 'true', label: t('promosAdmin.filters.statusEnabled') },
  { value: 'false', label: t('promosAdmin.filters.statusDisabled') }
])

const discountTypeOptions = computed(() => [
  { value: 'PERCENTAGE', label: t('promosAdmin.discountType.percentage') },
  { value: 'FIXED_AMOUNT', label: t('promosAdmin.discountType.fixedAmount') }
])

// 统计数据
const activeCount = computed(() => promos.value.filter(p => p.enabled).length)
const totalBindings = computed(() => promos.value.reduce((acc, p) => acc + (p.activeBindingsCount || 0), 0))
const totalDiscounted = computed(() => promos.value.reduce((acc, p) => acc + Number(p.totalDiscountAmount || 0), 0))

// 创建/编辑抽屉状态与表单
const createDrawerOpen = ref(false)
const creating = ref(false)
const isEditing = ref(false)
const editingPromoId = ref<number | null>(null)
const saving = ref(false)
const packagesList = ref<Package[]>([])
const packagesLoading = ref(false)
const expandedPackageIds = ref<Set<number>>(new Set())
const packagePlansMap = ref<Record<number, PackagePlanItem[]>>({})
const loadingPlansMap = ref<Record<number, boolean>>({})

const form = ref({
  code: '',
  name: '',
  discountType: 'PERCENTAGE' as PromoDiscountType,
  percentageValue: 10 as number | undefined,
  fixedAmountValue: 15 as number | undefined,
  durationType: 'REPEATING' as PromoDurationType,
  durationCycles: 6 as number | undefined,
  isGlobal: true,
  selectedScopes: [] as Array<{ packageId: number; packagePlanId: number | null }>,
  maxTotalUses: undefined as number | undefined,
  maxUsesPerUser: 1 as number | undefined,
  startsAt: '',
  expiresAt: '',
  enabled: true
})

// 查看绑定实例抽屉状态
const instancesDrawerOpen = ref(false)
const currentPromo = ref<AdminPromoCode | null>(null)
const boundInstances = ref<AdminPromoBoundInstance[]>([])
const instancesLoading = ref(false)
const instancesPage = ref(1)
const instancesPageSize = ref(20)
const instancesTotal = ref(0)
const unbindingInstanceId = ref<number | null>(null)

// 格式化辅助函数
function formatMoney(amount: number | null | undefined): string {
  return '¥' + (Number(amount) || 0).toFixed(2)
}

function formatDate(dateStr?: string | null): string {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleString()
}

function getDiscountText(promo: AdminPromoCode): string {
  if (promo.discountType === 'PERCENTAGE') {
    const ratePercent = Math.round(Number(promo.discountValue) * 100)
    return t('promosAdmin.discountType.percentageDesc', { rate: ratePercent })
  }
  return t('promosAdmin.discountType.fixedAmountDesc', { amount: Number(promo.discountValue).toFixed(2) })
}

function getDurationText(promo: AdminPromoCode): string {
  if (promo.durationType === 'ONCE') {
    return t('promosAdmin.durationType.once')
  }
  if (promo.durationType === 'REPEATING') {
    return t('promosAdmin.durationType.repeating', { cycles: promo.durationCycles ?? 0 })
  }
  return t('promosAdmin.durationType.forever')
}

function getScopeSummary(promo: AdminPromoCode): string {
  if (promo.isGlobal) {
    return t('promosAdmin.scopeType.global')
  }
  return t('promosAdmin.scopeType.custom', { count: promo.scopes?.length || 0 })
}

// 复制优惠码
async function copyCode(code: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(code)
    toast.success(t('common.copied'))
  } catch {
    toast.error('Copy failed')
  }
}

// 加载列表数据
async function loadPromos(): Promise<void> {
  loading.value = true
  try {
    const res = await api.promos.list({
      page: page.value,
      pageSize: pageSize.value,
      search: search.value.trim() || undefined,
      type: selectedType.value || undefined,
      enabled: selectedStatus.value === '' ? undefined : selectedStatus.value === 'true'
    })
    promos.value = res.promos || res.items || []
    total.value = res.total || 0
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.loadFailed', { message: err?.message || String(err) }))
  } finally {
    loading.value = false
  }
}

// 加载平台套餐与方案（用于创建抽屉的适用范围选择）
async function loadPackagesForScope(): Promise<void> {
  if (packagesList.value.length > 0) return
  packagesLoading.value = true
  try {
    const res = await api.packages.list({ all: true })
    packagesList.value = res.packages || []
  } catch (err: any) {
    console.error('Failed to load packages:', err)
  } finally {
    packagesLoading.value = false
  }
}

function formatForDateTimeInput(dateStr?: string | null): string {
  if (!dateStr) return ''
  const d = new Date(dateStr)
  if (isNaN(d.getTime())) return ''
  const year = d.getFullYear()
  const month = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  const hours = String(d.getHours()).padStart(2, '0')
  const minutes = String(d.getMinutes()).padStart(2, '0')
  return `${year}-${month}-${day}T${hours}:${minutes}`
}

// 打开创建抽屉
function openCreateDrawer(): void {
  isEditing.value = false
  editingPromoId.value = null
  form.value = {
    code: '',
    name: '',
    discountType: 'PERCENTAGE',
    percentageValue: 10,
    fixedAmountValue: 15,
    durationType: 'REPEATING',
    durationCycles: 6,
    isGlobal: true,
    selectedScopes: [],
    maxTotalUses: undefined,
    maxUsesPerUser: 1,
    startsAt: '',
    expiresAt: '',
    enabled: true
  }
  expandedPackageIds.value.clear()
  createDrawerOpen.value = true
}

// 打开编辑抽屉
async function openEditDrawer(promo: AdminPromoCode): Promise<void> {
  isEditing.value = true
  editingPromoId.value = promo.id

  let percentageValue: number | undefined = undefined
  let fixedAmountValue: number | undefined = undefined
  if (promo.discountType === 'PERCENTAGE') {
    percentageValue = Math.round(Number(promo.discountValue) * 100)
  } else {
    fixedAmountValue = Number(promo.discountValue)
  }

  form.value = {
    code: promo.code,
    name: promo.name || '',
    discountType: promo.discountType,
    percentageValue,
    fixedAmountValue,
    durationType: promo.durationType,
    durationCycles: promo.durationCycles ?? undefined,
    isGlobal: promo.isGlobal,
    selectedScopes: (promo.scopes || [])
      .filter((s): s is typeof s & { packageId: number } => typeof s.packageId === 'number')
      .map(s => ({
        packageId: s.packageId,
        packagePlanId: s.packagePlanId ?? null
      })),
    maxTotalUses: promo.maxTotalUses ?? undefined,
    maxUsesPerUser: promo.maxUsesPerUser ?? undefined,
    startsAt: formatForDateTimeInput(promo.startsAt),
    expiresAt: formatForDateTimeInput(promo.expiresAt),
    enabled: promo.enabled
  }

  expandedPackageIds.value.clear()
  if (!promo.isGlobal) {
    await loadPackagesForScope()
    for (const s of promo.scopes || []) {
      if (typeof s.packageId === 'number') {
        expandedPackageIds.value.add(s.packageId)
        if (!packagePlansMap.value[s.packageId] && !loadingPlansMap.value[s.packageId]) {
          void toggleExpandPackage(s.packageId)
        }
      }
    }
  }
  createDrawerOpen.value = true
}

// 设置有效周期模式
function setDurationType(type: PromoDurationType): void {
  form.value.durationType = type
  if (type === 'ONCE') {
    form.value.durationCycles = 1
  } else if (type === 'REPEATING' && (!form.value.durationCycles || form.value.durationCycles < 1)) {
    form.value.durationCycles = 6
  } else if (type === 'FOREVER') {
    form.value.durationCycles = undefined
  }
}

// 监听折扣类型变化：固定金额强制锁定为 ONCE
watch(() => form.value.discountType, (newType) => {
  if (newType === 'FIXED_AMOUNT') {
    form.value.durationType = 'ONCE'
    form.value.durationCycles = 1
  }
})

// 监听适用范围切换
watch(() => form.value.isGlobal, (isGlobal) => {
  if (!isGlobal) {
    void loadPackagesForScope()
  }
})

// 随机生成 8 位大写券码
function generateRandomCode(): void {
  const chars = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  let result = 'CP'
  for (let i = 0; i < 8; i++) {
    result += chars.charAt(Math.floor(Math.random() * chars.length))
  }
  form.value.code = result
}

// 展开/折叠单个套餐
async function toggleExpandPackage(packageId: number): Promise<void> {
  if (expandedPackageIds.value.has(packageId)) {
    expandedPackageIds.value.delete(packageId)
  } else {
    expandedPackageIds.value.add(packageId)
    if (!packagePlansMap.value[packageId]) {
      loadingPlansMap.value[packageId] = true
      try {
        const res = await api.packages.getPlans(packageId)
        packagePlansMap.value[packageId] = res.plans || []
      } catch (err: any) {
        console.error('Failed to load plans for package:', packageId, err)
      } finally {
        loadingPlansMap.value[packageId] = false
      }
    }
  }
}

// 全部展开所有套餐
async function expandAllPackages(): Promise<void> {
  const missingPlanPackageIds: number[] = []
  for (const pkg of packagesList.value) {
    expandedPackageIds.value.add(pkg.id)
    if (!packagePlansMap.value[pkg.id] && !loadingPlansMap.value[pkg.id]) {
      missingPlanPackageIds.push(pkg.id)
    }
  }
  if (missingPlanPackageIds.length > 0) {
    await Promise.all(
      missingPlanPackageIds.map(async (pkgId) => {
        loadingPlansMap.value[pkgId] = true
        try {
          const res = await api.packages.getPlans(pkgId)
          packagePlansMap.value[pkgId] = res.plans || []
        } catch (err: any) {
          console.error('Failed to load plans for package:', pkgId, err)
        } finally {
          loadingPlansMap.value[pkgId] = false
        }
      })
    )
  }
}

// 全部收起所有套餐
function collapseAllPackages(): void {
  expandedPackageIds.value.clear()
}

// 切换套餐勾选状态
function isPackageSelected(packageId: number): boolean {
  return form.value.selectedScopes.some(s => s.packageId === packageId && s.packagePlanId === null)
}

function isPlanSelected(packageId: number, planId: number): boolean {
  return isPackageSelected(packageId) || form.value.selectedScopes.some(s => s.packageId === packageId && s.packagePlanId === planId)
}

function getSelectedPlansCountForPackage(packageId: number): number {
  return form.value.selectedScopes.filter(s => s.packageId === packageId && s.packagePlanId !== null).length
}

function togglePackageSelection(packageId: number): void {
  if (isPackageSelected(packageId)) {
    form.value.selectedScopes = form.value.selectedScopes.filter(s => s.packageId !== packageId)
  } else {
    form.value.selectedScopes = form.value.selectedScopes.filter(s => s.packageId !== packageId)
    form.value.selectedScopes.push({ packageId, packagePlanId: null })
  }
}

function togglePlanSelection(packageId: number, planId: number): void {
  if (isPackageSelected(packageId)) {
    const plans = packagePlansMap.value[packageId] || []
    form.value.selectedScopes = form.value.selectedScopes.filter(s => s.packageId !== packageId)
    for (const p of plans) {
      if (p.id !== planId) {
        form.value.selectedScopes.push({ packageId, packagePlanId: p.id })
      }
    }
  } else {
    const idx = form.value.selectedScopes.findIndex(s => s.packageId === packageId && s.packagePlanId === planId)
    if (idx >= 0) {
      form.value.selectedScopes.splice(idx, 1)
    } else {
      form.value.selectedScopes.push({ packageId, packagePlanId: planId })
    }
  }
}

const selectedScopeSummary = computed(() => {
  const packagesCount = form.value.selectedScopes.filter(s => s.packagePlanId === null).length
  const plansCount = form.value.selectedScopes.filter(s => s.packagePlanId !== null).length
  if (packagesCount === 0 && plansCount === 0) return ''
  return t('promosAdmin.drawer.scopeSelectedSummary', { packages: packagesCount, plans: plansCount })
})

// 提交创建
async function submitCreate(): Promise<void> {
  const trimmedCode = form.value.code.trim().toUpperCase()
  if (!trimmedCode || trimmedCode.length < 3 || trimmedCode.length > 32) {
    toast.warning(t('promosAdmin.toast.invalidCode'))
    return
  }

  let finalDiscountValue: number
  if (form.value.discountType === 'PERCENTAGE') {
    const p = Number(form.value.percentageValue)
    if (!Number.isFinite(p) || p <= 0 || p > 100) {
      toast.warning(t('promosAdmin.toast.invalidPercentage'))
      return
    }
    finalDiscountValue = Number((p / 100).toFixed(4))
  } else {
    const a = Number(form.value.fixedAmountValue)
    if (!Number.isFinite(a) || a <= 0) {
      toast.warning(t('promosAdmin.toast.invalidFixedAmount'))
      return
    }
    finalDiscountValue = Number(a.toFixed(2))
  }

  let durationType: PromoDurationType
  let durationCycles: number | null = null

  if (form.value.discountType === 'FIXED_AMOUNT') {
    durationType = 'ONCE'
    durationCycles = null
  } else {
    durationType = form.value.durationType
    if (durationType === 'REPEATING') {
      const cycles = Number(form.value.durationCycles)
      if (!Number.isInteger(cycles) || cycles < 1) {
        toast.warning(t('promosAdmin.toast.invalidCycles'))
        return
      }
      durationCycles = cycles
    } else {
      durationCycles = null
    }
  }

  if (!form.value.isGlobal && form.value.selectedScopes.length === 0) {
    toast.warning(t('promosAdmin.toast.scopeRequired'))
    return
  }

  creating.value = true
  try {
    await api.promos.create({
      code: trimmedCode,
      name: form.value.name.trim() || undefined,
      type: 'ADMIN_PROMO',
      discountType: form.value.discountType,
      discountValue: finalDiscountValue,
      durationType,
      durationCycles,
      isGlobal: form.value.isGlobal,
      scopes: form.value.isGlobal ? undefined : form.value.selectedScopes,
      maxTotalUses: form.value.maxTotalUses ? Number(form.value.maxTotalUses) : null,
      maxUsesPerUser: form.value.maxUsesPerUser ? Number(form.value.maxUsesPerUser) : null,
      startsAt: form.value.startsAt ? new Date(form.value.startsAt).toISOString() : null,
      expiresAt: form.value.expiresAt ? new Date(form.value.expiresAt).toISOString() : null,
      enabled: form.value.enabled
    })
    toast.success(t('promosAdmin.toast.createSuccess'))
    createDrawerOpen.value = false
    await loadPromos()
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.createFailed', { message: err?.response?.data?.message || err?.message || String(err) }))
  } finally {
    creating.value = false
  }
}

// 提交修改
async function submitEdit(): Promise<void> {
  if (!editingPromoId.value) return

  let maxTotalUses: number | null = null
  if (form.value.maxTotalUses !== undefined && (form.value.maxTotalUses as unknown as string) !== '') {
    const parsed = Number(form.value.maxTotalUses)
    if (!Number.isSafeInteger(parsed) || parsed <= 0) {
      toast.warning(t('promosAdmin.toast.invalidDiscount'))
      return
    }
    maxTotalUses = parsed
  }

  let maxUsesPerUser: number | null = null
  if (form.value.maxUsesPerUser !== undefined && (form.value.maxUsesPerUser as unknown as string) !== '') {
    const parsed = Number(form.value.maxUsesPerUser)
    if (!Number.isSafeInteger(parsed) || parsed <= 0) {
      toast.warning(t('promosAdmin.toast.invalidDiscount'))
      return
    }
    maxUsesPerUser = parsed
  }

  const startsAt = form.value.startsAt ? new Date(form.value.startsAt).toISOString() : null
  const expiresAt = form.value.expiresAt ? new Date(form.value.expiresAt).toISOString() : null
  if (startsAt && expiresAt && new Date(startsAt) >= new Date(expiresAt)) {
    toast.warning(t('promosAdmin.toast.loadFailed', { message: t('promosAdmin.drawer.startsAtLabel') + ' >= ' + t('promosAdmin.drawer.expiresAtLabel') }))
    return
  }

  saving.value = true
  try {
    await api.promos.update(editingPromoId.value, {
      name: form.value.name.trim() || null,
      maxTotalUses,
      maxUsesPerUser,
      startsAt,
      expiresAt,
      enabled: form.value.enabled
    })
    toast.success(t('promosAdmin.toast.editSuccess'))
    createDrawerOpen.value = false
    await loadPromos()
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.editFailed', { message: err?.response?.data?.error || err?.response?.data?.message || err?.message || String(err) }))
  } finally {
    saving.value = false
  }
}

// 切换启用/停用状态
async function togglePromo(promo: AdminPromoCode): Promise<void> {
  const targetState = !promo.enabled
  try {
    await api.promos.toggle(promo.id, targetState)
    promo.enabled = targetState
    toast.success(t('promosAdmin.toast.toggleSuccess'))
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.toggleFailed', { message: err?.message || String(err) }))
  }
}

// 删除优惠码
async function deletePromo(promo: AdminPromoCode): Promise<void> {
  if (!confirm(`${t('promosAdmin.toast.deleteConfirmTitle')}\n\n${t('promosAdmin.toast.deleteConfirmMessage')}`)) {
    return
  }

  try {
    await api.promos.delete(promo.id)
    toast.success(t('promosAdmin.toast.deleteSuccess'))
    await loadPromos()
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.deleteFailed', { message: err?.message || String(err) }))
  }
}

// 打开绑定实例详情抽屉
async function openInstancesDrawer(promo: AdminPromoCode): Promise<void> {
  currentPromo.value = promo
  instancesPage.value = 1
  instancesDrawerOpen.value = true
  await loadBoundInstances()
}

// 加载指定券码绑定的实例
async function loadBoundInstances(): Promise<void> {
  if (!currentPromo.value) return
  instancesLoading.value = true
  try {
    const res = await api.promos.listInstances(currentPromo.value.id, {
      page: instancesPage.value,
      pageSize: instancesPageSize.value
    })
    boundInstances.value = res.instances || res.items || []
    instancesTotal.value = res.total || 0
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.loadFailed', { message: err?.message || String(err) }))
  } finally {
    instancesLoading.value = false
  }
}

// 解绑单个实例
async function unbindInstance(instanceId: number): Promise<void> {
  if (!currentPromo.value) return
  if (!confirm(`${t('promosAdmin.instancesDrawer.unbindConfirmTitle')}\n\n${t('promosAdmin.instancesDrawer.unbindConfirmMessage')}`)) {
    return
  }

  unbindingInstanceId.value = instanceId
  try {
    await api.promos.unbindInstance(currentPromo.value.id, instanceId)
    toast.success(t('promosAdmin.toast.unbindSuccess'))
    await loadBoundInstances()
    await loadPromos()
  } catch (err: any) {
    toast.error(t('promosAdmin.toast.unbindFailed', { message: err?.message || String(err) }))
  } finally {
    unbindingInstanceId.value = null
  }
}

onMounted(() => {
  void loadPromos()
})
</script>

<template>
  <div ref="revealRoot" class="space-y-6 animate-fade-in">
    <!-- 页面头部 -->
    <PageHeader
      :title="t('promosAdmin.title')"
      :description="t('promosAdmin.subtitle')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="loadPromos"
        >
          <template #icon>
            <RotateCw class="w-4 h-4" :class="{ 'animate-spin': loading }" />
          </template>
          {{ t('promosAdmin.refreshButton') }}
        </Button>
        <Button
          variant="primary"
          size="sm"
          @click="openCreateDrawer"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2.5]" />
          </template>
          {{ t('promosAdmin.createButton') }}
        </Button>
      </template>
    </PageHeader>

    <!-- 统计指标卡 -->
    <div data-reveal class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('promosAdmin.stats.totalCodes') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Tag class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ total }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('promosAdmin.stats.activeCodes') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <CheckCircle2 class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--success)]">
          {{ activeCount }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('promosAdmin.stats.activeBindings') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Server class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ totalBindings }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('promosAdmin.stats.totalDiscounted') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Coins class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--warning)]">
          {{ formatMoney(totalDiscounted) }}
        </div>
      </Card>
    </div>

    <!-- 过滤器与搜索栏 -->
    <Card data-reveal padding="sm">
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
        <div>
          <Input
            v-model="search"
            :placeholder="t('promosAdmin.filters.searchPlaceholder')"
            clearable
            size="sm"
            @enter="loadPromos"
            @clear="loadPromos"
          >
            <template #prefix>
              <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
            </template>
          </Input>
        </div>
        <div>
          <Select
            v-model="selectedType"
            :options="typeOptions"
            size="sm"
            @change="loadPromos"
          />
        </div>
        <div>
          <Select
            v-model="selectedStatus"
            :options="statusOptions"
            size="sm"
            @change="loadPromos"
          />
        </div>
      </div>
    </Card>

    <!-- 数据列表：PC 端定宽表格 -->
    <div data-reveal class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] shadow-[0_2px_8px_rgba(0,0,0,0.04)] hidden overflow-hidden lg:block">
      <table class="w-full table-fixed divide-y divide-[var(--border-color)]">
        <colgroup>
          <col class="w-[18%]" />
          <col class="w-[12%]" />
          <col class="w-[12%]" />
          <col class="w-[14%]" />
          <col class="w-[10%]" />
          <col class="w-[10%]" />
          <col class="w-[8%]" />
          <col class="w-[8%]" />
          <col class="w-[8%]" />
        </colgroup>
        <thead class="bg-[var(--bg-surface-soft)]">
          <tr class="text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] select-none">
            <th scope="col" class="px-4 py-3">{{ t('promosAdmin.table.code') }}</th>
            <th scope="col" class="px-4 py-3">{{ t('promosAdmin.table.discount') }}</th>
            <th scope="col" class="px-4 py-3">{{ t('promosAdmin.table.duration') }}</th>
            <th scope="col" class="px-4 py-3">{{ t('promosAdmin.table.scope') }}</th>
            <th scope="col" class="px-4 py-3">{{ t('promosAdmin.table.usage') }}</th>
            <th scope="col" class="px-4 py-3">{{ t('promosAdmin.table.totalDiscount') }}</th>
            <th scope="col" class="px-4 py-3 text-center">{{ t('promosAdmin.table.activeBindings') }}</th>
            <th scope="col" class="px-4 py-3 text-center">{{ t('promosAdmin.table.status') }}</th>
            <th scope="col" class="px-4 py-3 text-right">{{ t('promosAdmin.table.actions') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-[var(--border-color)]">
          <!-- 加载中状态 -->
          <template v-if="loading && promos.length === 0">
            <tr v-for="i in 5" :key="`loading-${i}`" class="animate-pulse">
              <td colspan="9" class="px-4 py-4">
                <div class="h-4 bg-[var(--bg-surface-soft)] rounded w-3/4"></div>
              </td>
            </tr>
          </template>

          <!-- 空数据状态 -->
          <tr v-else-if="promos.length === 0">
            <td colspan="9" class="py-12">
              <EmptyState :title="t('promosAdmin.table.emptyText')" />
            </td>
          </tr>

          <!-- 数据行 -->
          <tr
            v-for="promo in promos"
            :key="promo.id"
            class="hover:bg-[var(--bg-surface-soft)] transition-colors duration-150"
          >
            <!-- 券码与备注 -->
            <td class="px-4 py-3.5 truncate">
              <div class="flex items-center gap-1.5 min-w-0">
                <span class="font-mono font-bold text-sm text-[var(--text-primary)] truncate">{{ promo.code }}</span>
                <button
                  type="button"
                  class="text-[var(--text-tertiary)] hover:text-[var(--accent)] transition-colors shrink-0 p-0.5 rounded active:scale-95"
                  :title="t('common.copy')"
                  @click="copyCode(promo.code)"
                >
                  <Copy class="w-3.5 h-3.5" />
                </button>
              </div>
              <div class="text-xs text-[var(--text-tertiary)] truncate mt-0.5">{{ promo.name || '-' }}</div>
            </td>

            <!-- 折扣力度 -->
            <td class="px-4 py-3.5 text-sm font-semibold text-[var(--text-primary)]">
              {{ getDiscountText(promo) }}
            </td>

            <!-- 有效周期 -->
            <td class="px-4 py-3.5 text-sm text-[var(--text-secondary)]">
              {{ getDurationText(promo) }}
            </td>

            <!-- 适用范围 -->
            <td class="px-4 py-3.5 text-xs text-[var(--text-secondary)] truncate" :title="getScopeSummary(promo)">
              {{ getScopeSummary(promo) }}
            </td>

            <!-- 使用进度 -->
            <td class="px-4 py-3.5 text-xs font-mono text-[var(--text-secondary)]">
              {{ promo.usedTotalCount }} / {{ promo.maxTotalUses !== null ? promo.maxTotalUses : '∞' }}
            </td>

            <!-- 累计减免 -->
            <td class="px-4 py-3.5 text-sm font-mono font-semibold text-[var(--warning)]">
              {{ formatMoney(promo.totalDiscountAmount) }}
            </td>

            <!-- 绑定实例 -->
            <td class="px-4 py-3.5 text-center">
              <button
                type="button"
                class="inline-flex items-center justify-center gap-1 px-2.5 py-1 rounded-full text-xs font-mono font-semibold bg-[var(--bg-surface-soft)] text-[var(--text-primary)] hover:bg-[var(--bg-tertiary)] border border-[var(--border-color)] transition-all active:scale-95"
                @click="openInstancesDrawer(promo)"
              >
                <span>{{ promo.activeBindingsCount }}</span>
                <ChevronRight class="w-3 h-3 text-[var(--text-tertiary)]" />
              </button>
            </td>

            <!-- 状态切换 -->
            <td class="px-4 py-3.5 text-center">
              <Badge
                :variant="promo.enabled ? 'success' : 'default'"
                dot
                class="cursor-pointer hover:opacity-80 active:scale-95 transition-all select-none"
                @click="togglePromo(promo)"
              >
                {{ promo.enabled ? t('promosAdmin.filters.statusEnabled') : t('promosAdmin.filters.statusDisabled') }}
              </Badge>
            </td>

            <!-- 操作按钮 -->
            <td class="px-4 py-3.5 text-right">
              <div class="flex items-center justify-end gap-1">
                <Button
                  variant="ghost"
                  size="sm"
                  class="h-7 px-2.5 text-xs"
                  @click="openEditDrawer(promo)"
                >
                  {{ t('promosAdmin.table.edit') }}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  class="h-7 px-2.5 text-xs text-[var(--error)] hover:bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))]"
                  @click="deletePromo(promo)"
                >
                  {{ t('promosAdmin.table.delete') }}
                </Button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>

      <!-- PC 端分页栏 -->
      <div v-if="total > pageSize" class="p-4 border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
        <Pagination
          v-model:page="page"
          v-model:page-size="pageSize"
          :total="total"
          @change="loadPromos"
        />
      </div>
    </div>

    <!-- 数据列表：移动端响应式卡片 -->
    <div data-reveal class="space-y-3 lg:hidden">
      <!-- 移动端骨架屏 -->
      <div v-if="loading && promos.length === 0" class="space-y-3">
        <Card v-for="i in 3" :key="`mob-loading-${i}`" loading padding="sm" />
      </div>

      <!-- 移动端空数据 -->
      <EmptyState v-else-if="promos.length === 0" :title="t('promosAdmin.table.emptyText')" />

      <!-- 移动端卡片列表 -->
      <Card
        v-for="promo in promos"
        :key="promo.id"
        padding="sm"
        class="space-y-3"
      >
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5">
              <span class="font-mono text-base font-bold text-[var(--text-primary)] truncate">{{ promo.code }}</span>
              <button
                type="button"
                class="text-[var(--text-tertiary)] hover:text-[var(--accent)] transition-colors p-0.5 rounded active:scale-95"
                :title="t('common.copy')"
                @click="copyCode(promo.code)"
              >
                <Copy class="w-3.5 h-3.5" />
              </button>
            </div>
            <div class="text-xs text-[var(--text-tertiary)] truncate mt-0.5">{{ promo.name || '-' }}</div>
          </div>
          <Badge
            :variant="promo.enabled ? 'success' : 'default'"
            dot
            class="cursor-pointer hover:opacity-80 active:scale-95 transition-all select-none shrink-0"
            @click="togglePromo(promo)"
          >
            {{ promo.enabled ? t('promosAdmin.filters.statusEnabled') : t('promosAdmin.filters.statusDisabled') }}
          </Badge>
        </div>

        <div class="grid grid-cols-2 gap-2 text-xs border-y border-[var(--border-color)] py-2.5">
          <div>
            <span class="text-[var(--text-tertiary)]">{{ t('promosAdmin.table.discount') }}:</span>
            <span class="ml-1 font-semibold text-[var(--text-primary)]">{{ getDiscountText(promo) }}</span>
          </div>
          <div>
            <span class="text-[var(--text-tertiary)]">{{ t('promosAdmin.table.duration') }}:</span>
            <span class="ml-1 text-[var(--text-secondary)]">{{ getDurationText(promo) }}</span>
          </div>
          <div class="truncate">
            <span class="text-[var(--text-tertiary)]">{{ t('promosAdmin.table.scope') }}:</span>
            <span class="ml-1 text-[var(--text-secondary)] truncate">{{ getScopeSummary(promo) }}</span>
          </div>
          <div>
            <span class="text-[var(--text-tertiary)]">{{ t('promosAdmin.table.usage') }}:</span>
            <span class="ml-1 font-mono text-[var(--text-secondary)]">{{ promo.usedTotalCount }}/{{ promo.maxTotalUses ?? '∞' }}</span>
          </div>
        </div>

        <div class="flex flex-wrap items-center justify-between gap-2 text-xs pt-0.5">
          <div>
            <span class="text-[var(--text-tertiary)]">{{ t('promosAdmin.table.totalDiscount') }}:</span>
            <span class="ml-1 font-mono text-sm font-semibold text-[var(--warning)]">{{ formatMoney(promo.totalDiscountAmount) }}</span>
          </div>
          <div class="flex items-center gap-1.5 flex-wrap">
            <Button
              variant="secondary"
              size="sm"
              class="h-7 px-2.5 text-xs"
              @click="openInstancesDrawer(promo)"
            >
              {{ t('promosAdmin.table.viewInstances') }} ({{ promo.activeBindingsCount }})
            </Button>
            <Button
              variant="secondary"
              size="sm"
              class="h-7 px-2.5 text-xs"
              @click="openEditDrawer(promo)"
            >
              {{ t('promosAdmin.table.edit') }}
            </Button>
            <Button
              variant="danger"
              size="sm"
              class="h-7 px-2.5 text-xs"
              @click="deletePromo(promo)"
            >
              {{ t('promosAdmin.table.delete') }}
            </Button>
          </div>
        </div>
      </Card>

      <!-- 移动端分页栏 -->
      <div v-if="total > pageSize" class="pt-2">
        <Pagination
          v-model:page="page"
          v-model:page-size="pageSize"
          :total="total"
          @change="loadPromos"
        />
      </div>
    </div>

    <!-- 创建/编辑优惠码抽屉（DrawerModal） -->
    <DrawerModal
      v-model="createDrawerOpen"
      :title="isEditing ? t('promosAdmin.drawer.editTitle') : t('promosAdmin.drawer.createTitle')"
      max-width="max-w-xl"
    >
      <div class="space-y-4 text-sm">
        <!-- 编辑模式下的不可修改字段提示 -->
        <div v-if="isEditing" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 text-xs text-[var(--text-secondary)] leading-relaxed">
          {{ t('promosAdmin.drawer.editImmutableNotice') }}
        </div>

        <!-- 券码与随机生成 -->
        <div>
          <div class="flex items-center justify-between mb-1.5">
            <label class="text-xs font-semibold text-[var(--text-secondary)]">
              {{ t('promosAdmin.drawer.codeLabel') }} <span v-if="!isEditing" class="text-[var(--error)]">*</span>
            </label>
            <span v-if="isEditing" class="text-xs text-[var(--text-tertiary)] font-normal">
              ({{ t('promosAdmin.drawer.immutableNotice') }})
            </span>
          </div>
          <div class="flex items-center gap-2">
            <Input
              v-model="form.code"
              type="text"
              :disabled="isEditing"
              :maxlength="32"
              :placeholder="t('promosAdmin.drawer.codePlaceholder')"
              class="font-mono uppercase flex-1"
            />
            <Button
              v-if="!isEditing"
              variant="secondary"
              size="md"
              type="button"
              class="shrink-0"
              @click="generateRandomCode"
            >
              <template #icon>
                <Sparkles class="w-4 h-4 text-[var(--accent)]" />
              </template>
              {{ t('promosAdmin.drawer.generateRandom') }}
            </Button>
          </div>
        </div>

        <!-- 名称/活动备注 -->
        <div>
          <Input
            v-model="form.name"
            type="text"
            :maxlength="128"
            :label="t('promosAdmin.drawer.nameLabel')"
            :placeholder="t('promosAdmin.drawer.namePlaceholder')"
          />
        </div>

        <!-- 折扣类型与数值 -->
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div>
            <div class="flex items-center justify-between mb-1.5">
              <label class="text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('promosAdmin.drawer.discountTypeLabel') }}
              </label>
              <span v-if="isEditing" class="text-xs text-[var(--text-tertiary)] font-normal">
                ({{ t('promosAdmin.drawer.immutableNotice') }})
              </span>
            </div>
            <Select
              v-model="form.discountType"
              :options="discountTypeOptions"
              :disabled="isEditing"
            />
          </div>
          <div>
            <div class="flex items-center justify-between mb-1.5">
              <label class="text-xs font-semibold text-[var(--text-secondary)]">
                {{ form.discountType === 'PERCENTAGE' ? t('promosAdmin.drawer.discountPercentageLabel') : t('promosAdmin.drawer.discountFixedLabel') }} <span v-if="!isEditing" class="text-[var(--error)]">*</span>
              </label>
              <span v-if="isEditing" class="text-xs text-[var(--text-tertiary)] font-normal">
                ({{ t('promosAdmin.drawer.immutableNotice') }})
              </span>
            </div>
            <Input
              v-if="form.discountType === 'PERCENTAGE'"
              v-model.number="form.percentageValue"
              type="number"
              min="0.01"
              max="100"
              step="0.1"
              :disabled="isEditing"
              :placeholder="t('promosAdmin.drawer.discountPercentagePlaceholder')"
              class="font-mono"
            />
            <Input
              v-else
              v-model.number="form.fixedAmountValue"
              type="number"
              min="0.01"
              step="0.01"
              :disabled="isEditing"
              :placeholder="t('promosAdmin.drawer.discountFixedPlaceholder')"
              class="font-mono"
            />
          </div>
        </div>

        <!-- 折扣力度即时计算预览 -->
        <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 text-xs text-[var(--text-secondary)] flex items-center gap-2">
          <div class="w-1.5 h-1.5 rounded-full bg-[var(--accent)] shrink-0"></div>
          <span class="font-medium text-[var(--text-primary)] shrink-0">折扣效果示例：</span>
          <span v-if="form.discountType === 'PERCENTAGE'">
            {{ t('promosAdmin.drawer.discountPreviewPercentage', {
              discount: ((100 * Math.min(100, Math.max(0, Number(form.percentageValue || 0)))) / 100).toFixed(2),
              final: Math.max(0, 100 - (100 * Math.min(100, Math.max(0, Number(form.percentageValue || 0)))) / 100).toFixed(2)
            }) }}
          </span>
          <span v-else>
            {{ t('promosAdmin.drawer.discountPreviewFixed', {
              discount: Math.min(100, Math.max(0, Number(form.fixedAmountValue || 0))).toFixed(2),
              final: Math.max(0, 100 - Math.max(0, Number(form.fixedAmountValue || 0))).toFixed(2)
            }) }}
          </span>
        </div>

        <!-- 固定金额锁定提示 -->
        <div v-if="form.discountType === 'FIXED_AMOUNT'" class="rounded-xl border border-[color-mix(in_srgb,var(--warning)_30%,var(--border-color))] bg-[color-mix(in_srgb,var(--warning)_10%,var(--bg-surface))] p-3 text-xs text-[var(--warning)] leading-relaxed">
          {{ t('promosAdmin.drawer.fixedAmountNotice') }}
        </div>

        <!-- 有效周期（支持用户手动填写数字） -->
        <div v-if="form.discountType === 'PERCENTAGE'" class="space-y-3">
          <div>
            <div class="flex items-center justify-between mb-1.5">
              <label class="text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('promosAdmin.drawer.durationModeLabel') }}
              </label>
              <span v-if="isEditing" class="text-xs text-[var(--text-tertiary)] font-normal">
                ({{ t('promosAdmin.drawer.immutableNotice') }})
              </span>
            </div>
            <div class="grid grid-cols-3 gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)]">
              <button
                type="button"
                :disabled="isEditing"
                class="rounded-xl py-2 px-3 text-xs font-medium transition-all duration-200 text-center active:scale-95"
                :class="form.durationType === 'ONCE' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                @click="!isEditing && setDurationType('ONCE')"
              >
                {{ t('promosAdmin.drawer.durationOnce') }}
              </button>
              <button
                type="button"
                :disabled="isEditing"
                class="rounded-xl py-2 px-3 text-xs font-medium transition-all duration-200 text-center active:scale-95"
                :class="form.durationType === 'REPEATING' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                @click="!isEditing && setDurationType('REPEATING')"
              >
                {{ t('promosAdmin.drawer.durationRepeating') }}
              </button>
              <button
                type="button"
                :disabled="isEditing"
                class="rounded-xl py-2 px-3 text-xs font-medium transition-all duration-200 text-center active:scale-95"
                :class="form.durationType === 'FOREVER' ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold' : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)]'"
                @click="!isEditing && setDurationType('FOREVER')"
              >
                {{ t('promosAdmin.drawer.durationForever') }}
              </button>
            </div>
          </div>

          <!-- 手动输入期数数字 -->
          <div v-if="form.durationType === 'REPEATING'" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5 space-y-2">
            <div class="flex items-center justify-between">
              <label class="text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('promosAdmin.drawer.durationCyclesLabel') }} <span v-if="!isEditing" class="text-[var(--error)]">*</span>
              </label>
              <span v-if="isEditing" class="text-xs text-[var(--text-tertiary)] font-normal">
                ({{ t('promosAdmin.drawer.immutableNotice') }})
              </span>
            </div>
            <div class="flex items-center gap-2">
              <Input
                v-model.number="form.durationCycles"
                type="number"
                min="1"
                max="120"
                step="1"
                :disabled="isEditing"
                :placeholder="t('promosAdmin.drawer.durationCyclesPlaceholder')"
                class="font-mono flex-1"
              />
              <span class="text-xs text-[var(--text-tertiary)] shrink-0 font-medium select-none">期 (月)</span>
            </div>
            <p class="text-xs text-[var(--text-tertiary)] leading-relaxed">
              {{ t('promosAdmin.drawer.durationCyclesHint') }}
            </p>
          </div>
        </div>

        <!-- 适用范围配置 -->
        <div class="border-t border-[var(--border-color)] pt-4 space-y-3">
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2">
              <label class="text-xs font-semibold text-[var(--text-secondary)]">
                {{ t('promosAdmin.drawer.scopeTypeLabel') }}
              </label>
              <span v-if="isEditing" class="text-xs text-[var(--text-tertiary)] font-normal">
                ({{ t('promosAdmin.drawer.immutableNotice') }})
              </span>
            </div>
            <div v-if="!form.isGlobal && packagesList.length > 0" class="flex items-center gap-2">
              <button
                type="button"
                class="text-xs text-[var(--text-secondary)] hover:text-[var(--text-primary)] font-medium transition-colors"
                @click="expandAllPackages"
              >
                {{ t('promosAdmin.drawer.expandAll') }}
              </button>
              <span class="text-[var(--text-tertiary)] text-xs">·</span>
              <button
                type="button"
                class="text-xs text-[var(--text-secondary)] hover:text-[var(--text-primary)] font-medium transition-colors"
                @click="collapseAllPackages"
              >
                {{ t('promosAdmin.drawer.collapseAll') }}
              </button>
            </div>
          </div>

          <div class="grid grid-cols-2 gap-2">
            <button
              type="button"
              :disabled="isEditing"
              class="py-2.5 px-3 rounded-xl border text-xs font-medium transition-all duration-200 text-center active:scale-95"
              :class="form.isGlobal ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-semibold shadow-sm' : 'border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:border-[var(--border-hover)]'"
              @click="!isEditing && (form.isGlobal = true)"
            >
              {{ t('promosAdmin.drawer.scopeGlobal') }}
            </button>
            <button
              type="button"
              :disabled="isEditing"
              class="py-2.5 px-3 rounded-xl border text-xs font-medium transition-all duration-200 text-center active:scale-95"
              :class="!form.isGlobal ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] font-semibold shadow-sm' : 'border-[var(--border-color)] bg-[var(--bg-surface)] text-[var(--text-secondary)] hover:border-[var(--border-hover)]'"
              @click="!isEditing && (form.isGlobal = false)"
            >
              {{ t('promosAdmin.drawer.scopeCustom') }}
            </button>
          </div>

          <!-- 自定义范围套餐树形多选 -->
          <div v-if="!form.isGlobal" class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3 space-y-2 max-h-72 overflow-y-auto">
            <div class="flex items-center justify-between text-xs text-[var(--text-tertiary)] mb-2">
              <span>{{ t('promosAdmin.drawer.scopeSelectionHint') }}</span>
              <span v-if="selectedScopeSummary" class="font-mono text-[var(--text-primary)] font-medium">
                {{ selectedScopeSummary }}
              </span>
            </div>

            <div v-if="packagesLoading" class="text-xs text-[var(--text-tertiary)] py-4 text-center">
              {{ t('promosAdmin.drawer.loadingPackages') }}
            </div>
            <div v-else-if="packagesList.length === 0" class="text-xs text-[var(--text-tertiary)] py-4 text-center">
              {{ t('promosAdmin.drawer.noPackages') }}
            </div>

            <!-- 套餐列表与下属方案展开 -->
            <div
              v-for="pkg in packagesList"
              :key="pkg.id"
              class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface)] mb-2 overflow-hidden shadow-sm"
            >
              <!-- 套餐行 -->
              <div class="flex items-center justify-between p-2.5 hover:bg-[var(--bg-surface-soft)] transition-colors">
                <div class="flex items-center gap-2 flex-1 min-w-0">
                  <!-- 展开/折叠箭头按钮 -->
                  <button
                    type="button"
                    class="p-1 rounded-lg text-[var(--text-tertiary)] hover:text-[var(--text-primary)] transition-colors"
                    :title="expandedPackageIds.has(pkg.id) ? t('promosAdmin.drawer.collapsePlans') : t('promosAdmin.drawer.expandPlans', { count: '' })"
                    @click.stop="toggleExpandPackage(pkg.id)"
                  >
                    <ChevronRight
                      class="w-3.5 h-3.5 transition-transform duration-200"
                      :class="{ 'rotate-90': expandedPackageIds.has(pkg.id) }"
                    />
                  </button>

                  <!-- 整套餐勾选框 -->
                  <label class="inline-flex items-center gap-2 text-xs truncate select-none" :class="isEditing ? 'cursor-not-allowed text-[var(--text-tertiary)]' : 'text-[var(--text-primary)] cursor-pointer'">
                    <input
                      type="checkbox"
                      :checked="isPackageSelected(pkg.id)"
                      :disabled="isEditing"
                      class="rounded text-[var(--accent)] focus:ring-[var(--accent)]"
                      @change="!isEditing && togglePackageSelection(pkg.id)"
                    />
                    <span class="font-medium truncate">{{ pkg.name }}</span>
                  </label>
                </div>

                <div class="flex items-center gap-2 shrink-0">
                  <!-- 选中状态 Badge -->
                  <Badge
                    v-if="isPackageSelected(pkg.id)"
                    variant="success"
                    size="sm"
                  >
                    {{ t('promosAdmin.drawer.packageScopeWhole') }}
                  </Badge>
                  <Badge
                    v-else-if="getSelectedPlansCountForPackage(pkg.id) > 0"
                    variant="info"
                    size="sm"
                  >
                    {{ t('promosAdmin.drawer.scopeSelectedSummary', { packages: 0, plans: getSelectedPlansCountForPackage(pkg.id) }) }}
                  </Badge>

                  <!-- 展开方案文字按钮 -->
                  <button
                    type="button"
                    class="text-[11px] text-[var(--text-tertiary)] hover:text-[var(--text-primary)] font-mono ml-1 hover:underline"
                    @click.stop="toggleExpandPackage(pkg.id)"
                  >
                    {{ expandedPackageIds.has(pkg.id) ? t('promosAdmin.drawer.collapsePlans') : t('promosAdmin.drawer.expandPlans', { count: packagePlansMap[pkg.id]?.length ?? '...' }) }}
                  </button>
                  <span class="text-xs font-mono text-[var(--text-tertiary)]">ID: {{ pkg.id }}</span>
                </div>
              </div>

              <!-- 展开方案子列表 -->
              <div
                v-if="expandedPackageIds.has(pkg.id)"
                class="border-t border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-2.5 pl-8 space-y-1.5"
              >
                <div v-if="loadingPlansMap[pkg.id]" class="text-xs text-[var(--text-tertiary)] py-2 flex items-center gap-2">
                  <RotateCw class="w-3.5 h-3.5 animate-spin text-[var(--text-tertiary)]" />
                  <span>{{ t('promosAdmin.drawer.loadingPlans') }}</span>
                </div>

                <div v-else-if="!packagePlansMap[pkg.id] || packagePlansMap[pkg.id].length === 0" class="text-xs text-[var(--text-tertiary)] py-1">
                  {{ t('promosAdmin.drawer.noPlansForPackage') }}
                </div>

                <div
                  v-for="plan in packagePlansMap[pkg.id]"
                  :key="plan.id"
                  class="flex items-center justify-between py-1 border-b border-[var(--border-color)] last:border-0"
                >
                  <label class="inline-flex items-center gap-2 text-xs min-w-0 flex-1 truncate select-none" :class="isEditing || isPackageSelected(pkg.id) ? 'cursor-not-allowed' : 'cursor-pointer'">
                    <!-- 如果整套餐已选中，子方案显示为选中且禁用，并提示已由整套餐覆盖 -->
                    <input
                      type="checkbox"
                      :checked="isPlanSelected(pkg.id, plan.id)"
                      :disabled="isEditing || isPackageSelected(pkg.id)"
                      class="rounded text-[var(--accent)] focus:ring-[var(--accent)]"
                      @change="!isEditing && togglePlanSelection(pkg.id, plan.id)"
                    />
                    <span :class="['truncate', isEditing || isPackageSelected(pkg.id) ? 'text-[var(--text-tertiary)]' : 'text-[var(--text-primary)] font-medium']">
                      {{ plan.name }}
                    </span>
                    <span v-if="isPackageSelected(pkg.id)" class="text-[10px] text-[var(--success)]">
                      {{ t('promosAdmin.drawer.planCoveredByPackage') }}
                    </span>
                  </label>

                  <div class="flex items-center gap-2 shrink-0 font-mono text-[11px] text-[var(--text-tertiary)]">
                    <span>{{ plan.cpu }}核 {{ plan.memory }}MB {{ plan.disk }}GB</span>
                    <span class="text-[var(--text-secondary)] font-medium">¥{{ Number(plan.price).toFixed(2) }}</span>
                    <span>ID: {{ plan.id }}</span>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- 发行配额与限用次数 -->
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4 border-t border-[var(--border-color)] pt-4">
          <div>
            <Input
              v-model.number="form.maxTotalUses"
              type="number"
              min="1"
              :label="t('promosAdmin.drawer.maxTotalUsesLabel')"
              :placeholder="t('promosAdmin.drawer.maxTotalUsesPlaceholder')"
              class="font-mono"
            />
          </div>
          <div>
            <Input
              v-model.number="form.maxUsesPerUser"
              type="number"
              min="1"
              :label="t('promosAdmin.drawer.maxUsesPerUserLabel')"
              :placeholder="t('promosAdmin.drawer.maxUsesPerUserPlaceholder')"
              class="font-mono"
            />
          </div>
        </div>

        <!-- 起止时间 -->
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div>
            <Input
              v-model="form.startsAt"
              type="datetime-local"
              :label="t('promosAdmin.drawer.startsAtLabel')"
            />
          </div>
          <div>
            <Input
              v-model="form.expiresAt"
              type="datetime-local"
              :label="t('promosAdmin.drawer.expiresAtLabel')"
            />
          </div>
        </div>

        <!-- 立即启用 -->
        <div class="border-t border-[var(--border-color)] pt-3">
          <label class="inline-flex items-center gap-2 text-xs font-medium text-[var(--text-primary)] cursor-pointer select-none">
            <input v-model="form.enabled" type="checkbox" class="w-4 h-4 rounded text-[var(--accent)] focus:ring-[var(--accent)]" />
            <span>{{ t('promosAdmin.drawer.enabledLabel') }}</span>
          </label>
        </div>
      </div>

      <template #footer>
        <div class="flex items-center justify-end gap-3">
          <Button
            variant="secondary"
            size="sm"
            type="button"
            :disabled="creating || saving"
            @click="createDrawerOpen = false"
          >
            {{ t('promosAdmin.drawer.cancelButton') }}
          </Button>
          <Button
            variant="primary"
            size="sm"
            :loading="creating || saving"
            type="button"
            @click="isEditing ? submitEdit() : submitCreate()"
          >
            {{ isEditing ? (saving ? t('promosAdmin.drawer.saving') : t('promosAdmin.drawer.saveButton')) : (creating ? t('promosAdmin.drawer.submitting') : t('promosAdmin.drawer.submitButton')) }}
          </Button>
        </div>
      </template>
    </DrawerModal>

    <!-- 查看已绑定实例详情抽屉（DrawerModal） -->
    <DrawerModal
      v-model="instancesDrawerOpen"
      :title="t('promosAdmin.instancesDrawer.title', { code: currentPromo?.code || '' })"
      max-width="max-w-3xl"
    >
      <div class="space-y-4 text-sm">
        <div v-if="instancesLoading" class="py-12 flex flex-col items-center justify-center gap-3">
          <RotateCw class="w-6 h-6 animate-spin text-[var(--accent)]" />
          <span class="text-xs text-[var(--text-secondary)]">{{ t('giftCards.loading') }}</span>
        </div>

        <EmptyState
          v-else-if="boundInstances.length === 0"
          :title="t('promosAdmin.instancesDrawer.emptyText')"
        />

        <div v-else class="space-y-4">
          <div class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] overflow-hidden shadow-[0_2px_8px_rgba(0,0,0,0.04)]">
            <table class="w-full table-fixed divide-y divide-[var(--border-color)] text-xs">
              <colgroup>
                <col class="w-[30%]" />
                <col class="w-[20%]" />
                <col class="w-[20%]" />
                <col class="w-[18%]" />
                <col class="w-[12%]" />
              </colgroup>
              <thead class="bg-[var(--bg-surface-soft)]">
                <tr class="text-left font-semibold text-[var(--text-secondary)] uppercase tracking-wider select-none">
                  <th scope="col" class="px-3.5 py-2.5">{{ t('promosAdmin.instancesDrawer.instanceName') }}</th>
                  <th scope="col" class="px-3.5 py-2.5">{{ t('promosAdmin.instancesDrawer.username') }}</th>
                  <th scope="col" class="px-3.5 py-2.5">{{ t('promosAdmin.instancesDrawer.cyclesProgress') }}</th>
                  <th scope="col" class="px-3.5 py-2.5">{{ t('promosAdmin.instancesDrawer.boundAt') }}</th>
                  <th scope="col" class="px-3.5 py-2.5 text-right">{{ t('promosAdmin.instancesDrawer.actions') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr
                  v-for="item in boundInstances"
                  :key="item.instanceId"
                  class="hover:bg-[var(--bg-surface-soft)] transition-colors"
                >
                  <td class="px-3.5 py-2.5 truncate">
                    <div class="font-medium text-[var(--text-primary)] truncate">{{ item.instanceName }}</div>
                    <div class="font-mono text-[var(--text-tertiary)] text-[11px]">ID: {{ item.instanceId }}</div>
                  </td>
                  <td class="px-3.5 py-2.5 truncate font-mono text-[var(--text-secondary)]">
                    {{ item.username }}
                  </td>
                  <td class="px-3.5 py-2.5 font-mono text-[var(--text-secondary)]">
                    第 {{ item.usedCycles }} / {{ item.totalCycles || '∞' }} 期
                    <span class="text-[var(--text-tertiary)] block text-[11px]">(剩余 {{ item.remainingCycles !== null ? item.remainingCycles : '∞' }} 期)</span>
                  </td>
                  <td class="px-3.5 py-2.5 text-[var(--text-tertiary)]">
                    {{ formatDate(item.boundAt) }}
                  </td>
                  <td class="px-3.5 py-2.5 text-right">
                    <Button
                      variant="danger"
                      size="sm"
                      class="h-7 px-2.5 text-xs"
                      :disabled="unbindingInstanceId === item.instanceId"
                      :loading="unbindingInstanceId === item.instanceId"
                      @click="unbindInstance(item.instanceId)"
                    >
                      {{ unbindingInstanceId === item.instanceId ? t('promosAdmin.instancesDrawer.unbinding') : t('promosAdmin.instancesDrawer.unbindButton') }}
                    </Button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <div v-if="instancesTotal > instancesPageSize" class="pt-1">
            <Pagination
              v-model:page="instancesPage"
              :page-size="instancesPageSize"
              :total="instancesTotal"
              @change="loadBoundInstances"
            />
          </div>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end">
          <Button
            variant="secondary"
            size="sm"
            type="button"
            @click="instancesDrawerOpen = false"
          >
            {{ t('common.close') }}
          </Button>
        </div>
      </template>
    </DrawerModal>
  </div>
</template>
