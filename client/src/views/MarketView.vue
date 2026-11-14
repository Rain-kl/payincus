<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { usePageSeo } from '@/composables/usePageSeo'
import { useBrand } from '@/composables/useBrand'
import FlagIcon from '@/components/FlagIcon.vue'
import { useAuthStore } from '@/stores/auth'
import { useConfigStore } from '@/stores/config'
import { getLocalizedCountryName } from '@/utils/countryDisplay'
import {
  formatPublicPrice,
  formatPublicTraffic,
  getStartingMonthlyPrice,
  parsePackageIdQuery,
  parseTextQuery,
  type PublicPackage,
  type PublicRegion
} from '@/utils/publicCatalog'
import { freeSiteCopy, getFreeSiteBillingCycleLabel } from '@/utils/freeSiteFun'
import { instanceCreatePath, loginPath } from '@/utils/app-paths'
import { formatCpu } from '@/utils/formatters'
import { useReveal } from '@/composables/useReveal'
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Input,
  PageHeader,
  Select,
  type SelectOption
} from '@/components/ui'
import {
  AlertCircle,
  Check,
  Info,
  PackageOpen,
  Search,
  SearchX
} from 'lucide-vue-next'

defineOptions({
  name: 'MarketView'
})

const route = useRoute()
const router = useRouter()
const { t, locale } = useI18n()
const authStore = useAuthStore()
const configStore = useConfigStore()
const brand = useBrand()
void configStore.loadPublicConfig()

const revealRoot = ref<HTMLElement | null>(null)
useReveal(revealRoot)

type PublicPackageSource = 'official'

function normalizePublicPackageSource(_value: unknown): PublicPackageSource {
  return 'official'
}

const packageSource = ref<PublicPackageSource>(normalizePublicPackageSource(route.query.source))
const selectedRegion = ref<string | null>(parseTextQuery(route.query.region) || null)
const selectedPackageId = ref<number | null>(parsePackageIdQuery(route.query.package))
const selectedPlanId = ref<number | null>(parsePackageIdQuery(route.query.plan))
const searchQuery = ref(parseTextQuery(route.query.q))
const packages = ref<PublicPackage[]>([])
const regions = ref<PublicRegion[]>([])
const loading = ref(true)
const loadError = ref('')

let searchTimer: number | null = null

function getRegionLabel(code: string): string {
  return getLocalizedCountryName(code, locale.value, (key, fallback) => t(key, fallback))
}

const regionSelectOptions = computed<SelectOption[]>(() => [
  { label: t('publicSite.market.allRegions'), value: '' },
  ...regions.value.map(r => ({
    label: getRegionLabel(r.code),
    value: r.code
  }))
])

const filteredPackages = computed(() => {
  const query = searchQuery.value.trim().toLowerCase()
  let result = packages.value

  if (selectedRegion.value) {
    const region = regions.value.find(item => item.code === selectedRegion.value)
    if (region) {
      result = result.filter(pkg => region.packageIds.includes(pkg.id))
    } else {
      result = []
    }
  }

  if (query) {
    result = result.filter(pkg => {
      const text = [
        pkg.name,
        pkg.description || '',
        pkg.instance_type === 'vm' ? 'kvm' : 'lxc'
      ].join(' ').toLowerCase()
      return text.includes(query)
    })
  }

  return [...result].sort((left, right) => {
    if (left.soldOut !== right.soldOut) {
      return left.soldOut ? 1 : -1
    }

    const leftPrice = getStartingMonthlyPrice(left)
    const rightPrice = getStartingMonthlyPrice(right)

    if (leftPrice !== null && rightPrice !== null && leftPrice !== rightPrice) {
      return leftPrice - rightPrice
    }

    if (leftPrice === null && rightPrice !== null) {
      return -1
    }

    if (leftPrice !== null && rightPrice === null) {
      return 1
    }

    return left.name.localeCompare(right.name, 'zh')
  })
})

const selectedPackage = computed(() => {
  if (!selectedPackageId.value) {
    return null
  }

  return filteredPackages.value.find(pkg => pkg.id === selectedPackageId.value)
    || packages.value.find(pkg => pkg.id === selectedPackageId.value)
    || null
})

const selectedPlan = computed(() => {
  if (!selectedPackage.value || !selectedPlanId.value) {
    return null
  }

  return selectedPackage.value.plans.find(plan => plan.id === selectedPlanId.value && !plan.isSoldOut) || null
})

const summaryCards = computed(() => [
  { label: t('publicSite.market.summary.total'), value: String(packages.value.length) },
  { label: t('publicSite.market.summary.available'), value: String(packages.value.filter(pkg => !pkg.soldOut).length) },
  { label: t('publicSite.market.summary.regions'), value: String(regions.value.length) },
  { label: t('publicSite.market.summary.source'), value: t('publicSite.market.official') }
])

usePageSeo(() => {
  const selected = selectedPackage.value
  const plan = selectedPlan.value
  const origin = window.location.origin
  const canonical = selected
    ? `${origin}/market?source=official&package=${selected.id}${plan ? `&plan=${plan.id}` : ''}`
    : `${origin}/market`

  if (selected) {
    return {
      title: t('publicSite.seo.marketPackageTitle', { name: selected.name }).replace(/Incudal/g, brand.brandName),
      description: t('publicSite.seo.marketPackageDescription', {
        name: selected.name,
        type: selected.instance_type === 'vm' ? 'KVM' : 'LXC',
        traffic: formatTraffic(selected.monthly_traffic_limit)
      }),
      canonical,
      keywords: t('publicSite.seo.keywords').replace(/Incudal/g, brand.brandName)
    }
  }

  return {
    title: t('publicSite.seo.marketTitle').replace(/Incudal/g, brand.brandName),
    description: t('publicSite.seo.marketDescription'),
    canonical,
    keywords: t('publicSite.seo.keywords').replace(/Incudal/g, brand.brandName)
  }
})

const infoBannerText = computed(() => {
  if (!authStore.isAuthenticated && parsePackageIdQuery(route.query.package)) {
    return t('publicSite.market.buyLinkNotice')
  }

  return t('publicSite.market.publicNotice')
})

function formatTraffic(bytes: string | null): string {
  return formatPublicTraffic(bytes, t('common.unlimited'))
}

function formatMemory(mb: number): string {
  if (mb >= 1024) {
    return `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB`
  }

  return `${mb} MB`
}

function formatDisk(mb: number): string {
  if (mb >= 1024) {
    return `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB`
  }

  return `${mb} MB`
}

function getNetworkLabel(pkg: PublicPackage): string {
  return t(`common.networkMode.${pkg.network_mode}`)
}

function getPackagePriceLabel(pkg: PublicPackage): string {
  if (configStore.freeSiteMode) {
    return freeSiteCopy.marketPriceFree
  }

  const startPrice = getStartingMonthlyPrice(pkg)
  if (startPrice === null) {
    return t('publicSite.market.free')
  }

  return t('publicSite.market.fromMonthly', { price: formatPublicPrice(startPrice) })
}

function getPlanLabel(pkg: PublicPackage): string {
  if (!pkg.isPaid || pkg.plans.length === 0) {
    return t('publicSite.market.free')
  }

  if (configStore.freeSiteMode) {
    return freeSiteCopy.marketPlanCount.replace('{count}', String(pkg.plans.length))
  }

  return t('publicSite.market.planCount', { count: pkg.plans.length })
}

function getMarketPlanCycleLabel(months: number): string {
  return configStore.freeSiteMode ? getFreeSiteBillingCycleLabel(months) : t('publicSite.market.planCycle', { months })
}

function getMarketMonthlyPriceLabel(price: number): string {
  return configStore.freeSiteMode ? freeSiteCopy.marketMonthlyPrice : `≈ ¥${formatPublicPrice(price)}/${t('common.month')}`
}

function getMarketCtaText(pkg: PublicPackage): string {
  if (pkg.soldOut) return t('publicSite.market.soldOut')
  if (configStore.freeSiteMode) {
    return authStore.isAuthenticated ? freeSiteCopy.marketCreateNow : freeSiteCopy.marketLoginToOrder
  }
  return authStore.isAuthenticated ? t('publicSite.market.createNow') : t('publicSite.market.loginToOrder')
}

function getPackageRegions(pkg: PublicPackage): PublicRegion[] {
  return regions.value.filter(region => region.packageIds.includes(pkg.id)).slice(0, 3)
}

function getRouteSignatureFromState(): string {
  return JSON.stringify({
    source: packageSource.value,
    region: selectedRegion.value || '',
    packageId: selectedPackageId.value || '',
    planId: selectedPlanId.value || '',
    q: searchQuery.value.trim()
  })
}

function getRouteSignatureFromQuery(): string {
  return JSON.stringify({
    source: normalizePublicPackageSource(route.query.source),
    region: parseTextQuery(route.query.region),
    packageId: parsePackageIdQuery(route.query.package) || '',
    planId: parsePackageIdQuery(route.query.plan) || '',
    q: parseTextQuery(route.query.q)
  })
}

function syncRouteQuery(): void {
  if (getRouteSignatureFromState() === getRouteSignatureFromQuery()) {
    return
  }

  const nextQuery: Record<string, string> = {}

  if (selectedRegion.value) {
    nextQuery.region = selectedRegion.value
  }

  if (selectedPackageId.value) {
    nextQuery.package = String(selectedPackageId.value)
  }

  if (selectedPlanId.value) {
    nextQuery.plan = String(selectedPlanId.value)
  }

  const trimmedSearch = searchQuery.value.trim()
  if (trimmedSearch) {
    nextQuery.q = trimmedSearch
  }

  void router.replace({
    name: 'market',
    query: nextQuery
  })
}

function ensureSelectedPackage(
  preferredPackageId: number | null = selectedPackageId.value,
  options: { syncRegionWithPackage?: boolean } = {}
): void {
  const { syncRegionWithPackage = true } = options

  if (selectedRegion.value && !regions.value.some(region => region.code === selectedRegion.value)) {
    selectedRegion.value = null
  }

  if (preferredPackageId && syncRegionWithPackage) {
    const preferredPackage = packages.value.find(pkg => pkg.id === preferredPackageId)
    if (preferredPackage) {
      const currentRegion = selectedRegion.value
        ? regions.value.find(region => region.code === selectedRegion.value)
        : null

      if (currentRegion && !currentRegion.packageIds.includes(preferredPackage.id)) {
        const preferredRegion = regions.value.find(region => region.packageIds.includes(preferredPackage.id))
        if (preferredRegion) {
          selectedRegion.value = preferredRegion.code
        }
      }
    }
  }

  if (filteredPackages.value.length === 0 && selectedRegion.value !== null) {
    selectedRegion.value = null
  }

  if (filteredPackages.value.length === 0) {
    selectedPackageId.value = null
    return
  }

  if (!selectedPackageId.value || !filteredPackages.value.some(pkg => pkg.id === selectedPackageId.value)) {
    selectedPackageId.value = filteredPackages.value[0].id
  }
}

function ensureSelectedPlan(preferredPlanId: number | null = selectedPlanId.value): void {
  if (!selectedPackage.value || selectedPackage.value.plans.length === 0) {
    selectedPlanId.value = null
    return
  }

  const matchedPlan = preferredPlanId
    ? selectedPackage.value.plans.find(plan => plan.id === preferredPlanId && !plan.isSoldOut)
    : null

  const firstAvailablePlan = selectedPackage.value.plans.find(plan => !plan.isSoldOut)
  selectedPlanId.value = matchedPlan?.id || firstAvailablePlan?.id || null
}

async function loadData(
  preferredPackageId: number | null = selectedPackageId.value,
  preferredPlanId: number | null = selectedPlanId.value
): Promise<void> {
  loading.value = true
  loadError.value = ''

  try {
    const [packagesResponse, regionsResponse] = await Promise.all([
      api.packages.listPublic({ source: packageSource.value }),
      api.packages.getPublicRegions({ source: packageSource.value })
    ])

    packages.value = (packagesResponse.packages || []) as unknown as PublicPackage[]
    regions.value = regionsResponse.regions || []

    ensureSelectedPackage(preferredPackageId)
    ensureSelectedPlan(preferredPlanId)
    syncRouteQuery()
  } catch (error) {
    console.error('Failed to load public market:', error)
    packages.value = []
    regions.value = []
    loadError.value = t('common.loadFailed')
  } finally {
    loading.value = false
  }
}

async function syncFromRoute(): Promise<void> {
  const nextSource = normalizePublicPackageSource(route.query.source)
  const nextRegion = parseTextQuery(route.query.region) || null
  const nextPackageId = parsePackageIdQuery(route.query.package)
  const nextPlanId = parsePackageIdQuery(route.query.plan)
  const nextSearch = parseTextQuery(route.query.q)
  const sourceChanged = nextSource !== packageSource.value

  packageSource.value = nextSource
  selectedRegion.value = nextRegion
  selectedPackageId.value = nextPackageId
  selectedPlanId.value = nextPlanId
  searchQuery.value = nextSearch

  if (sourceChanged || packages.value.length === 0) {
    await loadData(nextPackageId, nextPlanId)
    return
  }

  ensureSelectedPackage(nextPackageId)
  ensureSelectedPlan(nextPlanId)
  syncRouteQuery()
}

function selectRegion(code: string | null): void {
  const currentPackageId = selectedPackageId.value
  selectedRegion.value = code

  const preferredPackageId = currentPackageId !== null && filteredPackages.value.some(pkg => pkg.id === currentPackageId)
    ? currentPackageId
    : null

  if (preferredPackageId === null) {
    selectedPlanId.value = null
  }

  ensureSelectedPackage(preferredPackageId, { syncRegionWithPackage: false })
  ensureSelectedPlan(selectedPlanId.value)
  syncRouteQuery()
}

function handleRegionSelect(value: string | number | boolean): void {
  selectRegion(value ? String(value) : null)
}

function selectPackage(pkg: PublicPackage): void {
  selectedPackageId.value = pkg.id
  ensureSelectedPlan(null)
  syncRouteQuery()
}

function selectPlan(planId: number): void {
  const plan = selectedPackage.value?.plans.find(item => item.id === planId)
  if (!plan || plan.isSoldOut) {
    return
  }

  selectedPlanId.value = planId
  syncRouteQuery()
}

function handleSearchInput(): void {
  ensureSelectedPackage(selectedPackageId.value)

  if (searchTimer !== null) {
    window.clearTimeout(searchTimer)
  }

  searchTimer = window.setTimeout(() => {
    syncRouteQuery()
    searchTimer = null
  }, 120)
}

function clearSearch(): void {
  searchQuery.value = ''
  ensureSelectedPackage(selectedPackageId.value)
  syncRouteQuery()
}

function clearFilters(): void {
  searchQuery.value = ''
  selectedRegion.value = null
  ensureSelectedPackage(selectedPackageId.value)
  syncRouteQuery()
}

function createInstance(pkg: PublicPackage): void {
  const source = pkg.sourceType === 'official' ? 'official' : 'market'
  const query: Record<string, string> = {
    source,
    package: String(pkg.id)
  }

  if (selectedPlan.value) {
    query.plan = String(selectedPlan.value.id)
  }

  if (authStore.isAuthenticated) {
    void router.push({
      path: instanceCreatePath(),
      query
    })
    return
  }

  const redirect = `${instanceCreatePath()}?${new URLSearchParams(query).toString()}`
  void router.push({
    path: loginPath(),
    query: { redirect }
  })
}

watch(
  () => route.fullPath,
  () => {
    void syncFromRoute()
  }
)

onMounted(() => {
  void loadData(selectedPackageId.value)
})

onUnmounted(() => {
  if (searchTimer !== null) {
    window.clearTimeout(searchTimer)
  }
})
</script>

<template>
  <div ref="revealRoot" class="relative max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8 sm:py-12">
    <!-- Page Header (StyleKit Apple Style) -->
    <PageHeader data-reveal>
      <template #title>
        <div class="space-y-3">
          <div>
            <Badge variant="info" :dot="true" size="md">
              {{ t('publicSite.market.badge') }}
            </Badge>
          </div>
          <h1 class="text-3xl sm:text-4xl lg:text-5xl font-semibold tracking-tight text-[var(--text-primary)]">
            {{ t('publicSite.market.title') }}
          </h1>
        </div>
      </template>
      <template #description>
        <p class="text-base sm:text-lg text-[var(--text-secondary)] max-w-2xl leading-relaxed mt-2">
          {{ t('publicSite.market.description') }}
        </p>
      </template>
    </PageHeader>

    <!-- Summary Stats Strip -->
    <div class="grid grid-cols-2 lg:grid-cols-4 gap-4 my-8" data-reveal>
      <Card
        v-for="card in summaryCards"
        :key="card.label"
        padding="sm"
        class="relative transition-all duration-300"
      >
        <div class="text-xs font-medium text-[var(--text-secondary)]">
          {{ card.label }}
        </div>
        <div class="mt-1.5 text-2xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
          {{ card.value }}
        </div>
      </Card>
    </div>

    <!-- Guest Purchase Intent Banner -->
    <div
      v-if="!authStore.isAuthenticated && parsePackageIdQuery(route.query.package)"
      class="mb-8 flex items-start gap-3 rounded-2xl border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--accent)_8%,var(--bg-surface))] px-5 py-4 text-sm leading-6 text-[var(--text-primary)] shadow-sm"
      data-reveal
    >
      <Info class="mt-0.5 h-5 w-5 shrink-0 text-[var(--accent)]" />
      <span>{{ infoBannerText }}</span>
    </div>

    <!-- Filter & Search Toolbar -->
    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 py-4 mb-8 border-y border-[var(--border-color)]" data-reveal>
      <!-- Mobile Region Select -->
      <div v-if="regions.length > 0" class="sm:hidden w-full">
        <Select
          :model-value="selectedRegion || ''"
          :options="regionSelectOptions"
          size="md"
          @update:model-value="handleRegionSelect"
        />
      </div>

      <!-- Desktop Region Pills -->
      <div v-if="regions.length > 0" class="hidden sm:flex min-w-0 flex-1 items-center gap-2 overflow-x-auto py-1 scrollbar-none">
        <Button
          :variant="selectedRegion === null ? 'primary' : 'secondary'"
          size="sm"
          class="shrink-0"
          @click="selectRegion(null)"
        >
          <Check v-if="selectedRegion === null" class="w-3.5 h-3.5" />
          {{ t('publicSite.market.allRegions') }}
        </Button>

        <Button
          v-for="region in regions"
          :key="region.code"
          :variant="selectedRegion === region.code ? 'primary' : 'secondary'"
          size="sm"
          class="shrink-0"
          @click="selectRegion(region.code)"
        >
          <Check v-if="selectedRegion === region.code" class="w-3.5 h-3.5" />
          <FlagIcon :code="region.code" class="h-3 w-4" />
          <span>{{ getRegionLabel(region.code) }}</span>
        </Button>
      </div>

      <!-- Search Input -->
      <div class="w-full sm:w-72 shrink-0">
        <Input
          v-model="searchQuery"
          :placeholder="t('publicSite.market.searchPlaceholder')"
          clearable
          size="md"
          @update:model-value="handleSearchInput"
          @clear="clearSearch"
        >
          <template #prefix>
            <Search class="h-4 w-4 text-[var(--text-tertiary)]" />
          </template>
        </Input>
      </div>
    </div>

    <!-- Load Error State -->
    <div
      v-if="loadError"
      class="my-8 flex items-start gap-3 rounded-2xl border border-[color-mix(in_srgb,var(--error)_28%,var(--bg-surface))] bg-[color-mix(in_srgb,var(--error)_10%,var(--bg-surface))] p-5 text-sm leading-6 text-[var(--error)] shadow-sm"
    >
      <AlertCircle class="mt-0.5 h-5 w-5 shrink-0" />
      <span>{{ loadError }}</span>
    </div>

    <!-- Loading Skeleton State -->
    <div v-else-if="loading" class="grid gap-6 lg:grid-cols-[minmax(0,1.18fr)_minmax(22rem,0.82fr)]">
      <div class="grid gap-4 md:grid-cols-2">
        <Card v-for="index in 6" :key="index" padding="md" :loading="true" class="h-64" />
      </div>
      <Card padding="md" :loading="true" class="h-[36rem]" />
    </div>

    <!-- Empty State: No Packages Total -->
    <EmptyState
      v-else-if="packages.length === 0"
      :title="t('publicSite.market.noPackages')"
      class="mt-8"
    >
      <template #icon>
        <PackageOpen class="w-7 h-7 stroke-[1.5]" />
      </template>
    </EmptyState>

    <!-- Main Catalog Content -->
    <div v-else class="grid items-start gap-6 lg:grid-cols-[minmax(0,1.18fr)_minmax(22rem,0.82fr)]">
      <!-- Left: Package Grid or Search Empty State -->
      <div class="min-w-0">
        <!-- Filter/Search Empty State -->
        <EmptyState
          v-if="filteredPackages.length === 0"
          :title="t('publicSite.market.noResults')"
          class="py-16"
        >
          <template #icon>
            <SearchX class="w-7 h-7 stroke-[1.5]" />
          </template>
          <template #action>
            <Button
              v-if="searchQuery || selectedRegion"
              variant="secondary"
              size="md"
              @click="clearFilters"
            >
              {{ t('common.reset') }}
            </Button>
          </template>
        </EmptyState>

        <!-- Package Card Grid -->
        <div v-else class="grid gap-4 md:grid-cols-2">
          <Card
            v-for="pkg in filteredPackages"
            :key="pkg.id"
            :hoverable="true"
            padding="sm"
            role="button"
            tabindex="0"
            class="relative text-left transition-all duration-300"
            :class="[
              selectedPackage?.id === pkg.id
                ? 'border-[var(--accent)] ring-2 ring-[var(--accent)]/20 shadow-md'
                : 'hover:border-[var(--border-hover)]',
              pkg.soldOut ? 'opacity-70' : ''
            ]"
            @click="selectPackage(pkg)"
            @keydown.enter="selectPackage(pkg)"
            @keydown.space.prevent="selectPackage(pkg)"
          >
            <!-- Card Top Row: Badges & Price -->
            <div class="flex items-start justify-between gap-3">
              <div class="flex flex-wrap items-center gap-1.5 min-w-0">
                <Badge :variant="pkg.sourceType === 'official' ? 'info' : 'default'" size="sm">
                  {{ t('publicSite.market.official') }}
                </Badge>
                <Badge variant="default" size="sm">
                  {{ pkg.instance_type === 'vm' ? 'KVM' : 'LXC' }}
                </Badge>
                <Badge :variant="pkg.soldOut ? 'error' : 'success'" size="sm" :dot="true">
                  {{ pkg.soldOut ? t('publicSite.market.soldOut') : t('publicSite.market.inStock') }}
                </Badge>
              </div>

              <div class="shrink-0 text-right">
                <div class="text-sm sm:text-base font-semibold text-[var(--accent)] font-mono tabular-nums">
                  {{ getPackagePriceLabel(pkg) }}
                </div>
                <div class="mt-0.5 text-xs text-[var(--text-tertiary)] font-mono">
                  {{ formatTraffic(pkg.monthly_traffic_limit) }}
                </div>
              </div>
            </div>

            <!-- Package Name -->
            <div class="mt-3.5 line-clamp-2 text-lg font-semibold tracking-tight text-[var(--text-primary)]">
              {{ pkg.name }}
            </div>

            <!-- Package Description -->
            <p class="mt-1.5 line-clamp-2 text-xs sm:text-sm text-[var(--text-secondary)] leading-relaxed">
              {{ pkg.description || t('publicSite.portal.packageFallback') }}
            </p>

            <!-- Region Badges -->
            <div v-if="getPackageRegions(pkg).length > 0" class="mt-3.5 flex flex-wrap gap-1.5">
              <Badge
                v-for="region in getPackageRegions(pkg)"
                :key="region.code"
                variant="default"
                size="sm"
              >
                <FlagIcon :code="region.code" class="h-3 w-4 mr-1" />
                {{ getRegionLabel(region.code) }}
              </Badge>
            </div>

            <!-- Specs Divider & Stats -->
            <div class="mt-4 grid grid-cols-2 gap-3 border-t border-[var(--border-color)] pt-3 text-xs">
              <div>
                <div class="text-[11px] font-medium text-[var(--text-tertiary)]">
                  {{ t('publicSite.market.labels.plans') }}
                </div>
                <div class="mt-0.5 font-semibold text-[var(--text-primary)] font-mono">
                  {{ getPlanLabel(pkg) }}
                </div>
              </div>
              <div>
                <div class="text-[11px] font-medium text-[var(--text-tertiary)]">
                  {{ t('publicSite.market.labels.network') }}
                </div>
                <div class="mt-0.5 font-semibold text-[var(--text-primary)] truncate">
                  {{ getNetworkLabel(pkg) }}
                </div>
              </div>
            </div>
          </Card>
        </div>
      </div>

      <!-- Right: Sticky Order & Plan Detail Sidebar -->
      <aside class="min-w-0 lg:self-start">
        <Card padding="md" class="lg:sticky lg:top-24 lg:max-h-[calc(100vh-7rem)] lg:overflow-y-auto">
          <template v-if="selectedPackage">
            <!-- Header Badges -->
            <div class="flex flex-wrap items-center gap-1.5">
              <Badge :variant="selectedPackage.sourceType === 'official' ? 'info' : 'default'" size="sm">
                {{ t('publicSite.market.official') }}
              </Badge>
              <Badge variant="default" size="sm">
                {{ selectedPackage.instance_type === 'vm' ? 'KVM' : 'LXC' }}
              </Badge>
              <Badge :variant="selectedPackage.soldOut ? 'error' : 'success'" size="sm" :dot="true">
                {{ selectedPackage.soldOut ? t('publicSite.market.soldOut') : t('publicSite.market.inStock') }}
              </Badge>
            </div>

            <!-- Selected Package Title -->
            <h2 class="mt-4 text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
              {{ selectedPackage.name }}
            </h2>

            <!-- Selected Package Description -->
            <p class="mt-2 text-sm text-[var(--text-secondary)] leading-relaxed">
              {{ selectedPackage.description || t('publicSite.portal.packageFallback') }}
            </p>

            <!-- Selected Package Regions -->
            <div v-if="getPackageRegions(selectedPackage).length > 0" class="mt-3.5 flex flex-wrap gap-1.5">
              <Badge
                v-for="region in getPackageRegions(selectedPackage)"
                :key="region.code"
                variant="default"
                size="sm"
              >
                <FlagIcon :code="region.code" class="h-3 w-4 mr-1" />
                {{ getRegionLabel(region.code) }}
              </Badge>
            </div>

            <!-- Key Metric Tiles -->
            <div class="mt-6 grid grid-cols-2 gap-3">
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
                <div class="text-xs font-medium text-[var(--text-secondary)]">
                  {{ t('publicSite.market.labels.startingPrice') }}
                </div>
                <div class="mt-1 text-lg font-semibold text-[var(--accent)] font-mono tabular-nums">
                  {{ getPackagePriceLabel(selectedPackage) }}
                </div>
              </div>
              <div class="rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-3.5">
                <div class="text-xs font-medium text-[var(--text-secondary)]">
                  {{ t('publicSite.market.labels.traffic') }}
                </div>
                <div class="mt-1 text-lg font-semibold text-[var(--text-primary)] font-mono tabular-nums">
                  {{ formatTraffic(selectedPackage.monthly_traffic_limit) }}
                </div>
              </div>
            </div>

            <!-- Plan Selection Section -->
            <div class="mt-6">
              <div class="flex items-center justify-between gap-4">
                <div class="text-sm font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ t('publicSite.market.plansTitle') }}
                </div>
                <div class="text-xs text-[var(--text-tertiary)]">
                  {{ getNetworkLabel(selectedPackage) }}
                </div>
              </div>

              <!-- Available Plans List -->
              <div v-if="selectedPackage.isPaid && selectedPackage.plans.length > 0" class="mt-3 space-y-2">
                <button
                  v-for="plan in selectedPackage.plans"
                  :key="plan.id"
                  type="button"
                  class="w-full text-left rounded-xl p-3 border transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
                  :class="[
                    selectedPlan?.id === plan.id
                      ? 'border-[var(--accent)] bg-[color-mix(in_srgb,var(--accent)_6%,var(--bg-surface))] ring-1 ring-[var(--accent)]'
                      : 'border-[var(--border-color)] bg-[var(--bg-surface-soft)] hover:border-[var(--border-hover)] hover:bg-[var(--bg-surface)]',
                    plan.isSoldOut ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer active:scale-[0.99]'
                  ]"
                  :disabled="plan.isSoldOut"
                  @click="selectPlan(plan.id)"
                >
                  <div class="flex items-start gap-3">
                    <div
                      class="mt-0.5 h-4 w-4 shrink-0 rounded-full border flex items-center justify-center transition-colors"
                      :class="selectedPlan?.id === plan.id ? 'border-[var(--accent)] bg-[var(--accent)]' : 'border-[var(--border-color)] bg-[var(--bg-surface)]'"
                    >
                      <div v-if="selectedPlan?.id === plan.id" class="h-1.5 w-1.5 rounded-full bg-white" />
                    </div>

                    <div class="min-w-0 flex-1">
                      <div class="flex items-start justify-between gap-3">
                        <div class="min-w-0">
                          <div class="text-sm font-semibold text-[var(--text-primary)] truncate">
                            {{ plan.name }}
                          </div>
                          <div class="mt-0.5 flex flex-wrap items-center gap-1.5 text-xs text-[var(--text-secondary)]">
                            <span>{{ getMarketPlanCycleLabel(plan.billingCycle) }}</span>
                            <Badge v-if="plan.isSoldOut" variant="error" size="sm">
                              {{ t('publicSite.market.soldOut') }}
                            </Badge>
                          </div>
                        </div>
                        <div class="shrink-0 text-right">
                          <div class="font-mono text-sm font-semibold text-[var(--text-primary)] tabular-nums">
                            {{ configStore.freeSiteMode ? freeSiteCopy.moneyJustForShow : `¥${formatPublicPrice(plan.price)}` }}
                          </div>
                          <div class="mt-0.5 text-xs text-[var(--text-secondary)] font-mono">
                            {{ getMarketMonthlyPriceLabel(plan.monthlyPrice) }}
                          </div>
                        </div>
                      </div>

                      <div class="mt-2.5 flex flex-wrap gap-1.5 text-[11px] font-mono tabular-nums">
                        <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">
                          CPU {{ formatCpu(plan.cpu) }}
                        </span>
                        <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">
                          {{ formatMemory(plan.memory) }}
                        </span>
                        <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">
                          {{ formatDisk(plan.disk) }}
                        </span>
                        <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">
                          {{ formatTraffic(plan.trafficLimit) }}
                        </span>
                      </div>
                    </div>
                  </div>
                </button>
              </div>

              <!-- Free or Custom Config Notice -->
              <div v-else class="mt-3 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-sm leading-6">
                <div class="font-semibold text-[var(--text-primary)]">
                  {{ t('publicSite.market.customConfigTitle') }}
                </div>
                <p class="mt-1 text-xs sm:text-sm text-[var(--text-secondary)]">
                  {{ t('publicSite.market.customConfigDescription') }}
                </p>
              </div>
            </div>

            <!-- Selected Plan Summary Card -->
            <div v-if="selectedPlan" class="mt-5 rounded-xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4">
              <div class="flex items-start justify-between gap-4">
                <div class="min-w-0">
                  <div class="text-xs font-medium text-[var(--text-secondary)]">
                    {{ t('publicSite.market.selectedPlanTitle') }}
                  </div>
                  <div class="mt-1 truncate text-base font-semibold text-[var(--text-primary)]">
                    {{ selectedPlan.name }}
                  </div>
                  <div class="mt-0.5 text-xs text-[var(--text-secondary)]">
                    {{ getMarketPlanCycleLabel(selectedPlan.billingCycle) }} · {{ getNetworkLabel(selectedPackage) }}
                  </div>
                </div>
                <div class="shrink-0 text-right">
                  <div class="font-mono text-xl font-semibold text-[var(--accent)] tabular-nums">
                    {{ configStore.freeSiteMode ? freeSiteCopy.moneyJustForShow : `¥${formatPublicPrice(selectedPlan.price)}` }}
                  </div>
                  <div class="mt-0.5 text-xs text-[var(--text-secondary)] font-mono">
                    {{ getMarketMonthlyPriceLabel(selectedPlan.monthlyPrice) }}
                  </div>
                </div>
              </div>

              <div class="mt-3 flex flex-wrap gap-1.5 font-mono text-[11px] tabular-nums">
                <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">CPU {{ formatCpu(selectedPlan.cpu) }}</span>
                <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">{{ formatMemory(selectedPlan.memory) }}</span>
                <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">{{ formatDisk(selectedPlan.disk) }}</span>
                <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">{{ formatTraffic(selectedPlan.trafficLimit) }}</span>
                <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">{{ t('publicSite.market.labels.hosts') }} {{ selectedPackage.host_ids.length }}</span>
                <span class="rounded-md border border-[var(--border-color)] bg-[var(--bg-surface)] px-2 py-0.5 text-[var(--text-secondary)]">{{ t('publicSite.market.labels.nesting') }} {{ selectedPackage.nested ? t('common.yes') : t('common.no') }}</span>
              </div>
            </div>

            <!-- CTA Order Button -->
            <Button
              variant="primary"
              size="lg"
              :block="true"
              class="mt-6 font-semibold"
              :disabled="selectedPackage.soldOut || (selectedPackage.isPaid && !selectedPlan)"
              @click="createInstance(selectedPackage)"
            >
              {{ getMarketCtaText(selectedPackage) }}
            </Button>

            <p v-if="!authStore.isAuthenticated" class="mt-3 text-center text-xs text-[var(--text-tertiary)]">
              {{ t('publicSite.market.loginHint') }}
            </p>
          </template>

          <!-- No Package Selected Placeholder -->
          <template v-else>
            <div class="flex min-h-[20rem] items-center justify-center text-center text-sm text-[var(--text-tertiary)]">
              {{ t('publicSite.market.choosePackage') }}
            </div>
          </template>
        </Card>
      </aside>
    </div>
  </div>
</template>

<style scoped>
.scrollbar-none {
  scrollbar-width: none;
  -ms-overflow-style: none;
}
.scrollbar-none::-webkit-scrollbar {
  display: none;
}
</style>
