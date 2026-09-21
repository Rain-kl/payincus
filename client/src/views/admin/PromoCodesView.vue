<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
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

// 优惠码列表与过滤状态
const promos = ref<AdminPromoCode[]>([])
const loading = ref(false)
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const search = ref('')
const selectedType = ref<PromoCodeType | ''>('')
const selectedStatus = ref<'' | 'true' | 'false'>('')

// 统计数据
const activeCount = computed(() => promos.value.filter(p => p.enabled).length)
const totalBindings = computed(() => promos.value.reduce((acc, p) => acc + (p.activeBindingsCount || 0), 0))
const totalDiscounted = computed(() => promos.value.reduce((acc, p) => acc + Number(p.totalDiscountAmount || 0), 0))

// 创建抽屉状态与表单
const createDrawerOpen = ref(false)
const creating = ref(false)
const packagesList = ref<Package[]>([])
const packagesLoading = ref(false)
const expandedPackageIds = ref<Set<number>>(new Set())
const packagePlansMap = ref<Record<number, PackagePlanItem[]>>({})
const loadingPlansMap = ref<Record<number, boolean>>({})

const form = ref({
  code: '',
  name: '',
  discountType: 'PERCENTAGE' as PromoDiscountType,
  percentageValue: 10 as number | null,
  fixedAmountValue: 15 as number | null,
  durationType: 'REPEATING' as PromoDurationType,
  durationCycles: 6 as number | null,
  isGlobal: true,
  selectedScopes: [] as Array<{ packageId: number; packagePlanId: number | null }>,
  maxTotalUses: null as number | null,
  maxUsesPerUser: 1 as number | null,
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

// 打开创建抽屉
function openCreateDrawer(): void {
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
    maxTotalUses: null,
    maxUsesPerUser: 1,
    startsAt: '',
    expiresAt: '',
    enabled: true
  }
  expandedPackageIds.value.clear()
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
    form.value.durationCycles = null
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
    loadPackagesForScope()
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
    // 勾选套餐节点（移除其下具体的方案节点，冒泡包含整套餐）
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
    // 百分比转为 0-1.0 浮点数存入后端
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
  loadPromos()
})
</script>

<template>
  <div class="space-y-6">
    <!-- 顶部标题与主要操作 -->
    <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <div>
        <h1 class="text-2xl font-bold text-themed-primary">{{ t('promosAdmin.title') }}</h1>
        <p class="mt-1 text-sm text-themed-muted">{{ t('promosAdmin.subtitle') }}</p>
      </div>
      <div class="flex items-center gap-3">
        <button class="btn btn-secondary" :disabled="loading" @click="loadPromos">
          <svg class="h-4 w-4" :class="{ 'animate-spin': loading }" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.58m15.36 2A8 8 0 005.07 8.11M20 20v-5h-.58m0 0A8 8 0 014.06 12.03" />
          </svg>
          {{ t('promosAdmin.refreshButton') }}
        </button>
        <button class="btn btn-primary" @click="openCreateDrawer">
          <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4v16m8-8H4" />
          </svg>
          {{ t('promosAdmin.createButton') }}
        </button>
      </div>
    </div>

    <!-- 统计卡片指标 -->
    <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <div class="card p-4">
        <div class="text-sm font-medium text-themed-muted">{{ t('promosAdmin.stats.totalCodes') }}</div>
        <div class="mt-2 text-2xl font-bold text-themed-primary">{{ total }}</div>
      </div>
      <div class="card p-4">
        <div class="text-sm font-medium text-themed-muted">{{ t('promosAdmin.stats.activeCodes') }}</div>
        <div class="mt-2 text-2xl font-bold text-emerald-600 dark:text-emerald-400">{{ activeCount }}</div>
      </div>
      <div class="card p-4">
        <div class="text-sm font-medium text-themed-muted">{{ t('promosAdmin.stats.activeBindings') }}</div>
        <div class="mt-2 text-2xl font-bold text-themed-primary">{{ totalBindings }}</div>
      </div>
      <div class="card p-4">
        <div class="text-sm font-medium text-themed-muted">{{ t('promosAdmin.stats.totalDiscounted') }}</div>
        <div class="mt-2 text-2xl font-bold text-amber-600 dark:text-amber-400">{{ formatMoney(totalDiscounted) }}</div>
      </div>
    </div>

    <!-- 过滤器与搜索栏 -->
    <div class="card p-4">
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <div>
          <input
            v-model="search"
            type="text"
            class="input w-full"
            :placeholder="t('promosAdmin.filters.searchPlaceholder')"
            @keyup.enter="loadPromos"
          />
        </div>
        <div>
          <select v-model="selectedType" class="input w-full" @change="loadPromos">
            <option value="">{{ t('promosAdmin.filters.typeAll') }}</option>
            <option value="ADMIN_PROMO">{{ t('promosAdmin.filters.typeAdmin') }}</option>
            <option value="AFF_USER">{{ t('promosAdmin.filters.typeAff') }}</option>
          </select>
        </div>
        <div>
          <select v-model="selectedStatus" class="input w-full" @change="loadPromos">
            <option value="">{{ t('promosAdmin.filters.statusAll') }}</option>
            <option value="true">{{ t('promosAdmin.filters.statusEnabled') }}</option>
            <option value="false">{{ t('promosAdmin.filters.statusDisabled') }}</option>
          </select>
        </div>
      </div>
    </div>

    <!-- 数据列表：PC 端定宽表格 -->
    <div class="card hidden overflow-hidden lg:block">
      <table class="w-full table-fixed divide-y divide-themed">
        <thead>
          <tr class="bg-themed-secondary text-left text-xs font-semibold text-themed-muted">
            <th class="w-[18%] px-4 py-3">{{ t('promosAdmin.table.code') }}</th>
            <th class="w-[12%] px-4 py-3">{{ t('promosAdmin.table.discount') }}</th>
            <th class="w-[12%] px-4 py-3">{{ t('promosAdmin.table.duration') }}</th>
            <th class="w-[14%] px-4 py-3">{{ t('promosAdmin.table.scope') }}</th>
            <th class="w-[10%] px-4 py-3">{{ t('promosAdmin.table.usage') }}</th>
            <th class="w-[10%] px-4 py-3">{{ t('promosAdmin.table.totalDiscount') }}</th>
            <th class="w-[8%] px-4 py-3 text-center">{{ t('promosAdmin.table.activeBindings') }}</th>
            <th class="w-[8%] px-4 py-3 text-center">{{ t('promosAdmin.table.status') }}</th>
            <th class="w-[8%] px-4 py-3 text-right">{{ t('promosAdmin.table.actions') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-themed">
          <tr v-if="promos.length === 0 && !loading" class="text-center text-sm text-themed-muted">
            <td colspan="9" class="py-8">{{ t('promosAdmin.table.emptyText') }}</td>
          </tr>
          <tr v-for="promo in promos" :key="promo.id" class="hover:bg-themed-hover transition-colors">
            <td class="px-4 py-3 truncate">
              <div class="font-mono font-bold text-themed-primary">{{ promo.code }}</div>
              <div class="text-xs text-themed-muted truncate">{{ promo.name || '-' }}</div>
            </td>
            <td class="px-4 py-3 text-sm font-semibold text-themed-primary">
              {{ getDiscountText(promo) }}
            </td>
            <td class="px-4 py-3 text-sm text-themed-secondary">
              {{ getDurationText(promo) }}
            </td>
            <td class="px-4 py-3 text-xs text-themed-secondary truncate">
              {{ getScopeSummary(promo) }}
            </td>
            <td class="px-4 py-3 text-sm text-themed-secondary font-mono">
              {{ promo.usedTotalCount }} / {{ promo.maxTotalUses !== null ? promo.maxTotalUses : '∞' }}
            </td>
            <td class="px-4 py-3 text-sm font-mono text-amber-600 dark:text-amber-400">
              {{ formatMoney(promo.totalDiscountAmount) }}
            </td>
            <td class="px-4 py-3 text-center">
              <button
                class="inline-flex items-center justify-center rounded px-2 py-0.5 text-xs font-mono font-semibold bg-themed-secondary text-themed-primary hover:bg-themed-hover border border-themed"
                @click="openInstancesDrawer(promo)"
              >
                {{ promo.activeBindingsCount }}
              </button>
            </td>
            <td class="px-4 py-3 text-center">
              <button
                class="inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold cursor-pointer border border-themed"
                :class="promo.enabled ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300' : 'bg-themed-secondary text-themed-muted'"
                @click="togglePromo(promo)"
              >
                {{ promo.enabled ? t('promosAdmin.filters.statusEnabled') : t('promosAdmin.filters.statusDisabled') }}
              </button>
            </td>
            <td class="px-4 py-3 text-right">
              <button
                class="text-xs font-medium text-rose-600 dark:text-rose-400 hover:underline"
                @click="deletePromo(promo)"
              >
                {{ t('promosAdmin.table.delete') }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- 数据列表：移动端响应式卡片 -->
    <div class="space-y-3 lg:hidden">
      <div v-if="promos.length === 0 && !loading" class="card py-8 text-center text-sm text-themed-muted">
        {{ t('promosAdmin.table.emptyText') }}
      </div>
      <div v-for="promo in promos" :key="promo.id" class="card p-4 space-y-3">
        <div class="flex items-start justify-between gap-2">
          <div>
            <div class="font-mono text-lg font-bold text-themed-primary">{{ promo.code }}</div>
            <div class="text-xs text-themed-muted">{{ promo.name || '-' }}</div>
          </div>
          <button
            class="inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold border border-themed"
            :class="promo.enabled ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300' : 'bg-themed-secondary text-themed-muted'"
            @click="togglePromo(promo)"
          >
            {{ promo.enabled ? t('promosAdmin.filters.statusEnabled') : t('promosAdmin.filters.statusDisabled') }}
          </button>
        </div>

        <div class="grid grid-cols-2 gap-2 text-xs border-t border-b border-themed py-2">
          <div>
            <span class="text-themed-muted">{{ t('promosAdmin.table.discount') }}:</span>
            <span class="ml-1 font-semibold text-themed-primary">{{ getDiscountText(promo) }}</span>
          </div>
          <div>
            <span class="text-themed-muted">{{ t('promosAdmin.table.duration') }}:</span>
            <span class="ml-1 text-themed-secondary">{{ getDurationText(promo) }}</span>
          </div>
          <div>
            <span class="text-themed-muted">{{ t('promosAdmin.table.scope') }}:</span>
            <span class="ml-1 text-themed-secondary">{{ getScopeSummary(promo) }}</span>
          </div>
          <div>
            <span class="text-themed-muted">{{ t('promosAdmin.table.usage') }}:</span>
            <span class="ml-1 font-mono text-themed-secondary">{{ promo.usedTotalCount }}/{{ promo.maxTotalUses ?? '∞' }}</span>
          </div>
        </div>

        <div class="flex items-center justify-between text-xs pt-1">
          <div>
            <span class="text-themed-muted">{{ t('promosAdmin.table.totalDiscount') }}:</span>
            <span class="ml-1 font-mono text-amber-600 dark:text-amber-400 font-semibold">{{ formatMoney(promo.totalDiscountAmount) }}</span>
          </div>
          <div class="flex items-center gap-2">
            <button
              class="btn btn-secondary text-xs px-2.5 py-1"
              @click="openInstancesDrawer(promo)"
            >
              {{ t('promosAdmin.table.viewInstances') }} ({{ promo.activeBindingsCount }})
            </button>
            <button
              class="text-xs text-rose-600 dark:text-rose-400 font-medium px-2 py-1"
              @click="deletePromo(promo)"
            >
              {{ t('promosAdmin.table.delete') }}
            </button>
          </div>
        </div>
      </div>
    </div>

    <!-- 创建优惠码抽屉（DrawerModal） -->
    <DrawerModal
      v-model="createDrawerOpen"
      :title="t('promosAdmin.drawer.createTitle')"
      maxWidth="max-w-xl"
    >
      <div class="space-y-4 text-sm">
        <!-- 券码与随机生成 -->
        <div>
          <label class="block mb-1 text-xs font-semibold text-themed-primary">
            {{ t('promosAdmin.drawer.codeLabel') }} <span class="text-rose-600 dark:text-rose-400">*</span>
          </label>
          <div class="flex gap-2">
            <input
              v-model="form.code"
              type="text"
              class="input font-mono uppercase flex-1"
              :placeholder="t('promosAdmin.drawer.codePlaceholder')"
              maxlength="32"
            />
            <button class="btn btn-secondary text-xs shrink-0" type="button" @click="generateRandomCode">
              {{ t('promosAdmin.drawer.generateRandom') }}
            </button>
          </div>
        </div>

        <!-- 名称/活动备注 -->
        <div>
          <label class="block mb-1 text-xs font-semibold text-themed-primary">
            {{ t('promosAdmin.drawer.nameLabel') }}
          </label>
          <input
            v-model="form.name"
            type="text"
            class="input w-full"
            :placeholder="t('promosAdmin.drawer.namePlaceholder')"
            maxlength="128"
          />
        </div>

        <!-- 折扣类型与数值 -->
        <div class="grid grid-cols-2 gap-4">
          <div>
            <label class="block mb-1 text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.discountTypeLabel') }}
            </label>
            <select v-model="form.discountType" class="input w-full">
              <option value="PERCENTAGE">{{ t('promosAdmin.discountType.percentage') }}</option>
              <option value="FIXED_AMOUNT">{{ t('promosAdmin.discountType.fixedAmount') }}</option>
            </select>
          </div>
          <div>
            <label class="block mb-1 text-xs font-semibold text-themed-primary">
              {{ form.discountType === 'PERCENTAGE' ? t('promosAdmin.drawer.discountPercentageLabel') : t('promosAdmin.drawer.discountFixedLabel') }} <span class="text-rose-600 dark:text-rose-400">*</span>
            </label>
            <input
              v-if="form.discountType === 'PERCENTAGE'"
              v-model.number="form.percentageValue"
              type="number"
              min="0.01"
              max="100"
              step="0.1"
              class="input w-full font-mono"
              :placeholder="t('promosAdmin.drawer.discountPercentagePlaceholder')"
            />
            <input
              v-else
              v-model.number="form.fixedAmountValue"
              type="number"
              min="0.01"
              step="0.01"
              class="input w-full font-mono"
              :placeholder="t('promosAdmin.drawer.discountFixedPlaceholder')"
            />
          </div>
        </div>

        <!-- 折扣力度即时计算预览 -->
        <div class="rounded border border-themed bg-themed-secondary p-2.5 text-xs text-themed-secondary">
          <span class="font-medium text-themed-primary">折扣效果示例：</span>
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
        <div v-if="form.discountType === 'FIXED_AMOUNT'" class="rounded border border-amber-600 bg-themed-tertiary p-2.5 text-xs text-amber-600 dark:text-amber-400">
          {{ t('promosAdmin.drawer.fixedAmountNotice') }}
        </div>

        <!-- 有效周期（支持用户手动填写数字） -->
        <div v-if="form.discountType === 'PERCENTAGE'" class="space-y-3">
          <div>
            <label class="block mb-1.5 text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.durationModeLabel') }}
            </label>
            <div class="grid grid-cols-3 gap-2">
              <button
                type="button"
                :class="['btn text-xs py-1.5 px-2 text-center transition-colors', form.durationType === 'ONCE' ? 'btn-primary font-semibold' : 'btn-secondary text-themed-secondary']"
                @click="setDurationType('ONCE')"
              >
                {{ t('promosAdmin.drawer.durationOnce') }}
              </button>
              <button
                type="button"
                :class="['btn text-xs py-1.5 px-2 text-center transition-colors', form.durationType === 'REPEATING' ? 'btn-primary font-semibold' : 'btn-secondary text-themed-secondary']"
                @click="setDurationType('REPEATING')"
              >
                {{ t('promosAdmin.drawer.durationRepeating') }}
              </button>
              <button
                type="button"
                :class="['btn text-xs py-1.5 px-2 text-center transition-colors', form.durationType === 'FOREVER' ? 'btn-primary font-semibold' : 'btn-secondary text-themed-secondary']"
                @click="setDurationType('FOREVER')"
              >
                {{ t('promosAdmin.drawer.durationForever') }}
              </button>
            </div>
          </div>

          <!-- 手动输入期数数字 -->
          <div v-if="form.durationType === 'REPEATING'" class="rounded border border-themed bg-themed-secondary p-3 space-y-1.5">
            <label class="block text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.durationCyclesLabel') }} <span class="text-rose-600 dark:text-rose-400">*</span>
            </label>
            <div class="flex items-center gap-2">
              <input
                v-model.number="form.durationCycles"
                type="number"
                min="1"
                max="120"
                step="1"
                class="input w-full font-mono"
                :placeholder="t('promosAdmin.drawer.durationCyclesPlaceholder')"
              />
              <span class="text-xs text-themed-muted shrink-0 font-medium">期 (月)</span>
            </div>
            <p class="text-xs text-themed-muted">
              {{ t('promosAdmin.drawer.durationCyclesHint') }}
            </p>
          </div>
        </div>

        <!-- 适用范围配置 -->
        <div class="border-t border-themed pt-3 space-y-2">
          <div class="flex items-center justify-between">
            <label class="text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.scopeTypeLabel') }}
            </label>
            <div v-if="!form.isGlobal && packagesList.length > 0" class="flex items-center gap-2">
              <button
                type="button"
                class="text-xs text-themed-secondary hover:text-themed-primary font-medium"
                @click="expandAllPackages"
              >
                {{ t('promosAdmin.drawer.expandAll') }}
              </button>
              <span class="text-themed-muted text-xs">|</span>
              <button
                type="button"
                class="text-xs text-themed-secondary hover:text-themed-primary font-medium"
                @click="collapseAllPackages"
              >
                {{ t('promosAdmin.drawer.collapseAll') }}
              </button>
            </div>
          </div>

          <div class="flex items-center gap-4">
            <label class="inline-flex items-center gap-2 text-xs text-themed-primary cursor-pointer">
              <input v-model="form.isGlobal" type="radio" :value="true" />
              <span>{{ t('promosAdmin.drawer.scopeGlobal') }}</span>
            </label>
            <label class="inline-flex items-center gap-2 text-xs text-themed-primary cursor-pointer">
              <input v-model="form.isGlobal" type="radio" :value="false" />
              <span>{{ t('promosAdmin.drawer.scopeCustom') }}</span>
            </label>
          </div>

          <!-- 自定义范围套餐树形多选 -->
          <div v-if="!form.isGlobal" class="rounded border border-themed bg-themed-secondary p-3 space-y-2 max-h-72 overflow-y-auto">
            <div class="flex items-center justify-between text-xs text-themed-muted mb-2">
              <span>{{ t('promosAdmin.drawer.scopeSelectionHint') }}</span>
              <span v-if="selectedScopeSummary" class="font-mono text-themed-primary font-medium">
                {{ selectedScopeSummary }}
              </span>
            </div>

            <div v-if="packagesLoading" class="text-xs text-themed-muted py-4 text-center">
              {{ t('promosAdmin.drawer.loadingPackages') }}
            </div>
            <div v-else-if="packagesList.length === 0" class="text-xs text-themed-muted py-4 text-center">
              {{ t('promosAdmin.drawer.noPackages') }}
            </div>

            <!-- 套餐列表与下属方案展开 -->
            <div
              v-for="pkg in packagesList"
              :key="pkg.id"
              class="rounded border border-themed bg-themed-primary mb-2 overflow-hidden"
            >
              <!-- 套餐行 -->
              <div class="flex items-center justify-between p-2.5 hover:bg-themed-tertiary transition-colors">
                <div class="flex items-center gap-2 flex-1 min-w-0">
                  <!-- 展开/折叠箭头按钮 -->
                  <button
                    type="button"
                    class="p-1 rounded text-themed-muted hover:text-themed-primary"
                    :title="expandedPackageIds.has(pkg.id) ? t('promosAdmin.drawer.collapsePlans') : t('promosAdmin.drawer.expandPlans', { count: '' })"
                    @click.stop="toggleExpandPackage(pkg.id)"
                  >
                    <svg
                      class="w-3.5 h-3.5 transition-transform"
                      :class="{ 'rotate-90': expandedPackageIds.has(pkg.id) }"
                      fill="none"
                      stroke="currentColor"
                      viewBox="0 0 24 24"
                    >
                      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5l7 7-7 7" />
                    </svg>
                  </button>

                  <!-- 整套餐勾选框 -->
                  <label class="inline-flex items-center gap-2 text-xs text-themed-primary cursor-pointer truncate">
                    <input
                      type="checkbox"
                      :checked="isPackageSelected(pkg.id)"
                      @change="togglePackageSelection(pkg.id)"
                    />
                    <span class="font-medium truncate">{{ pkg.name }}</span>
                  </label>
                </div>

                <div class="flex items-center gap-2 shrink-0">
                  <!-- 选中状态 Badge -->
                  <span
                    v-if="isPackageSelected(pkg.id)"
                    class="text-[11px] px-1.5 py-0.5 rounded border border-green-600 bg-themed-tertiary text-green-600 dark:text-green-400 font-medium"
                  >
                    {{ t('promosAdmin.drawer.packageScopeWhole') }}
                  </span>
                  <span
                    v-else-if="getSelectedPlansCountForPackage(pkg.id) > 0"
                    class="text-[11px] px-1.5 py-0.5 rounded border border-themed bg-themed-tertiary text-themed-primary font-medium"
                  >
                    {{ t('promosAdmin.drawer.scopeSelectedSummary', { packages: 0, plans: getSelectedPlansCountForPackage(pkg.id) }) }}
                  </span>

                  <!-- 展开方案文字按钮 -->
                  <button
                    type="button"
                    class="text-[11px] text-themed-muted hover:text-themed-primary font-mono ml-1 underline"
                    @click.stop="toggleExpandPackage(pkg.id)"
                  >
                    {{ expandedPackageIds.has(pkg.id) ? t('promosAdmin.drawer.collapsePlans') : t('promosAdmin.drawer.expandPlans', { count: packagePlansMap[pkg.id]?.length ?? '...' }) }}
                  </button>
                  <span class="text-xs font-mono text-themed-muted">#{{ pkg.id }}</span>
                </div>
              </div>

              <!-- 展开方案子列表 -->
              <div
                v-if="expandedPackageIds.has(pkg.id)"
                class="border-t border-themed bg-themed-secondary p-2.5 pl-8 space-y-1.5"
              >
                <div v-if="loadingPlansMap[pkg.id]" class="text-xs text-themed-muted py-2 flex items-center gap-2">
                  <svg class="w-3.5 h-3.5 animate-spin text-themed-muted" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                    <path fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"></path>
                  </svg>
                  <span>{{ t('promosAdmin.drawer.loadingPlans') }}</span>
                </div>

                <div v-else-if="!packagePlansMap[pkg.id] || packagePlansMap[pkg.id].length === 0" class="text-xs text-themed-muted py-1">
                  {{ t('promosAdmin.drawer.noPlansForPackage') }}
                </div>

                <div
                  v-for="plan in packagePlansMap[pkg.id]"
                  :key="plan.id"
                  class="flex items-center justify-between py-1 border-b border-themed last:border-0"
                >
                  <label class="inline-flex items-center gap-2 text-xs cursor-pointer min-w-0 flex-1 truncate">
                    <!-- 如果整套餐已选中，子方案显示为选中且禁用，并提示已由整套餐覆盖 -->
                    <input
                      type="checkbox"
                      :checked="isPlanSelected(pkg.id, plan.id)"
                      :disabled="isPackageSelected(pkg.id)"
                      @change="togglePlanSelection(pkg.id, plan.id)"
                    />
                    <span :class="['truncate', isPackageSelected(pkg.id) ? 'text-themed-muted' : 'text-themed-primary font-medium']">
                      {{ plan.name }}
                    </span>
                    <span v-if="isPackageSelected(pkg.id)" class="text-[10px] text-green-600 dark:text-green-400">
                      {{ t('promosAdmin.drawer.planCoveredByPackage') }}
                    </span>
                  </label>

                  <div class="flex items-center gap-2 shrink-0 font-mono text-[11px] text-themed-muted">
                    <span>{{ plan.cpu }}核 {{ plan.memory }}MB {{ plan.disk }}GB</span>
                    <span class="text-themed-secondary font-medium">¥{{ Number(plan.price).toFixed(2) }}</span>
                    <span>#{{ plan.id }}</span>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- 发行配额与限用次数 -->
        <div class="grid grid-cols-2 gap-4 border-t border-themed pt-3">
          <div>
            <label class="block mb-1 text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.maxTotalUsesLabel') }}
            </label>
            <input
              v-model.number="form.maxTotalUses"
              type="number"
              min="1"
              class="input w-full font-mono"
              :placeholder="t('promosAdmin.drawer.maxTotalUsesPlaceholder')"
            />
          </div>
          <div>
            <label class="block mb-1 text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.maxUsesPerUserLabel') }}
            </label>
            <input
              v-model.number="form.maxUsesPerUser"
              type="number"
              min="1"
              class="input w-full font-mono"
              :placeholder="t('promosAdmin.drawer.maxUsesPerUserPlaceholder')"
            />
          </div>
        </div>

        <!-- 起止时间 -->
        <div class="grid grid-cols-2 gap-4">
          <div>
            <label class="block mb-1 text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.startsAtLabel') }}
            </label>
            <input v-model="form.startsAt" type="datetime-local" class="input w-full text-xs" />
          </div>
          <div>
            <label class="block mb-1 text-xs font-semibold text-themed-primary">
              {{ t('promosAdmin.drawer.expiresAtLabel') }}
            </label>
            <input v-model="form.expiresAt" type="datetime-local" class="input w-full text-xs" />
          </div>
        </div>

        <!-- 立即启用 -->
        <div class="border-t border-themed pt-3">
          <label class="inline-flex items-center gap-2 text-xs font-medium text-themed-primary cursor-pointer">
            <input v-model="form.enabled" type="checkbox" />
            <span>{{ t('promosAdmin.drawer.enabledLabel') }}</span>
          </label>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button class="btn btn-secondary text-xs" type="button" @click="createDrawerOpen = false">
            {{ t('promosAdmin.drawer.cancelButton') }}
          </button>
          <button class="btn btn-primary text-xs" :disabled="creating" type="button" @click="submitCreate">
            {{ creating ? t('promosAdmin.drawer.submitting') : t('promosAdmin.drawer.submitButton') }}
          </button>
        </div>
      </template>
    </DrawerModal>

    <!-- 查看已绑定实例详情抽屉（DrawerModal） -->
    <DrawerModal
      v-model="instancesDrawerOpen"
      :title="t('promosAdmin.instancesDrawer.title', { code: currentPromo?.code || '' })"
      maxWidth="max-w-3xl"
    >
      <div class="space-y-4 text-sm">
        <div v-if="instancesLoading" class="py-8 text-center text-themed-muted text-xs">
          <svg class="h-5 w-5 animate-spin mx-auto mb-2" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.58m15.36 2A8 8 0 005.07 8.11M20 20v-5h-.58m0 0A8 8 0 014.06 12.03" />
          </svg>
          {{ t('giftCards.loading') }}
        </div>
        <div v-else-if="boundInstances.length === 0" class="py-8 text-center text-themed-muted text-xs">
          {{ t('promosAdmin.instancesDrawer.emptyText') }}
        </div>
        <div v-else class="border border-themed rounded-lg overflow-hidden">
          <table class="w-full table-fixed divide-y divide-themed text-xs">
            <thead>
              <tr class="bg-themed-secondary text-left font-semibold text-themed-muted">
                <th class="w-[30%] px-3 py-2">{{ t('promosAdmin.instancesDrawer.instanceName') }}</th>
                <th class="w-[20%] px-3 py-2">{{ t('promosAdmin.instancesDrawer.username') }}</th>
                <th class="w-[20%] px-3 py-2">{{ t('promosAdmin.instancesDrawer.cyclesProgress') }}</th>
                <th class="w-[20%] px-3 py-2">{{ t('promosAdmin.instancesDrawer.boundAt') }}</th>
                <th class="w-[10%] px-3 py-2 text-right">{{ t('promosAdmin.instancesDrawer.actions') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-themed">
              <tr v-for="item in boundInstances" :key="item.instanceId" class="hover:bg-themed-hover transition-colors">
                <td class="px-3 py-2.5 truncate">
                  <div class="font-medium text-themed-primary truncate">{{ item.instanceName }}</div>
                  <div class="font-mono text-themed-muted">#{{ item.instanceId }}</div>
                </td>
                <td class="px-3 py-2.5 truncate text-themed-secondary font-mono">
                  {{ item.username }}
                </td>
                <td class="px-3 py-2.5 text-themed-secondary font-mono">
                  第 {{ item.usedCycles }} / {{ item.totalCycles || '∞' }} 期
                  <span class="text-themed-muted block">(剩余 {{ item.remainingCycles !== null ? item.remainingCycles : '∞' }} 期)</span>
                </td>
                <td class="px-3 py-2.5 text-themed-muted">
                  {{ formatDate(item.boundAt) }}
                </td>
                <td class="px-3 py-2.5 text-right">
                  <button
                    class="text-xs text-rose-600 dark:text-rose-400 font-medium hover:underline"
                    :disabled="unbindingInstanceId === item.instanceId"
                    @click="unbindInstance(item.instanceId)"
                  >
                    {{ unbindingInstanceId === item.instanceId ? t('promosAdmin.instancesDrawer.unbinding') : t('promosAdmin.instancesDrawer.unbindButton') }}
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end">
          <button class="btn btn-secondary text-xs" type="button" @click="instancesDrawerOpen = false">
            {{ t('common.close') }}
          </button>
        </div>
      </template>
    </DrawerModal>
  </div>
</template>
