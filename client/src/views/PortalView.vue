<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import api from '@/api'
import { usePageSeo } from '@/composables/usePageSeo'
import { useAuthStore } from '@/stores/auth'
import { useBrand } from '@/composables/useBrand'
import {
  formatPublicPrice,
  formatPublicTraffic,
  getStartingMonthlyPrice,
  type PackageSource,
  type PublicPackage
} from '@/utils/publicCatalog'
import { dashboardPath, loginPath, marketPath } from '@/utils/app-paths'
import { useReveal } from '@/composables/useReveal'
import {
  Badge,
  Button,
  Card,
  EmptyState
} from '@/components/ui'
import {
  Activity,
  ArrowRight,
  Check,
  Cpu,
  Network,
  ShieldCheck
} from 'lucide-vue-next'

defineOptions({
  name: 'PortalView'
})

const router = useRouter()
const { t } = useI18n()
const authStore = useAuthStore()
const brand = useBrand()

// 门面首页滚动揭示（GSAP + IntersectionObserver，渐进增强、内容默认可见）
const portalRoot = ref<HTMLElement | null>(null)
useReveal(portalRoot)

const packages = ref<PublicPackage[]>([])
const loading = ref(true)
type ProductLineKey = PackageSource | 'all'
const activeProductLine = ref<ProductLineKey>('all')

const heroTitlePrimary = computed(() => t('publicSite.portal.heroTitlePrimary'))
const heroTitlePrefix = computed(() => t('publicSite.portal.heroTitlePrefix'))
const heroTitleAccent = computed(() => t('publicSite.portal.heroTitleAccent'))

const spotlightPackages = computed(() => {
  const allPackages = [...packages.value].sort((left, right) => {
    if (left.soldOut !== right.soldOut) {
      return left.soldOut ? 1 : -1
    }

    const leftPrice = getStartingMonthlyPrice(left)
    const rightPrice = getStartingMonthlyPrice(right)

    if (leftPrice !== null && rightPrice !== null && leftPrice !== rightPrice) {
      return leftPrice - rightPrice
    }

    return left.name.localeCompare(right.name, 'zh')
  })

  return allPackages.slice(0, 4)
})

const heroFeatureCards = computed(() => [
  {
    icon: 'network',
    title: t('publicSite.portal.experienceNoLoginTitle'),
    description: t('publicSite.portal.experienceNoLoginDescription')
  },
  {
    icon: 'compute',
    title: t('publicSite.portal.experienceRoutingTitle'),
    description: t('publicSite.portal.experienceRoutingDescription')
  },
  {
    icon: 'secure',
    title: t('publicSite.portal.officialTitle'),
    description: t('publicSite.portal.officialPoint2')
  },
  {
    icon: 'ops',
    title: t('publicSite.portal.previewTitle'),
    description: t('publicSite.portal.previewDescription')
  }
])

const ecosystemCards = computed(() => [
  {
    key: 'official',
    source: 'official' as PackageSource,
    title: t('publicSite.portal.officialTitle'),
    description: t('publicSite.portal.officialDescription'),
    points: [
      t('publicSite.portal.officialPoint1'),
      t('publicSite.portal.officialPoint2'),
      t('publicSite.portal.officialPoint3')
    ]
  }
])

const consoleActionLabel = computed(() => (
  authStore.isAuthenticated ? t('publicSite.actions.console') : t('publicSite.actions.signIn')
))

const consoleActionCompactLabel = computed(() => (
  authStore.isAuthenticated ? t('publicSite.actions.consoleCompact') : t('publicSite.actions.signIn')
))

usePageSeo(() => ({
  title: `${brand.brandName} - ${brand.brandSubtitle}`,
  description: brand.brandSubtitle,
  canonical: `${window.location.origin}/`,
  keywords: t('publicSite.seo.keywords').replace(/Incudal/g, brand.brandName)
}))

function formatTraffic(bytes: string | null): string {
  return formatPublicTraffic(bytes, t('common.unlimited'))
}

function getPriceLabel(pkg: PublicPackage): string {
  const startPrice = getStartingMonthlyPrice(pkg)
  if (startPrice === null) {
    return t('publicSite.market.free')
  }

  return t('publicSite.market.fromMonthly', { price: formatPublicPrice(startPrice) })
}

function setActiveProductLine(source: ProductLineKey): void {
  activeProductLine.value = source
}

function browseCatalog(source?: PackageSource): void {
  void router.push({
    path: marketPath(),
    query: source ? { source } : undefined
  })
}

function openPackage(pkg: PublicPackage): void {
  void router.push({
    path: marketPath(),
    query: {
      source: pkg.sourceType,
      package: String(pkg.id)
    }
  })
}

function goToConsole(): void {
  if (authStore.isAuthenticated) {
    void router.push(dashboardPath())
    return
  }

  void router.push(loginPath())
}

async function loadCatalog(): Promise<void> {
  loading.value = true

  try {
    const packagesResponse = await api.packages.listPublic()
    packages.value = (packagesResponse.packages || []) as unknown as PublicPackage[]
  } catch (error) {
    console.error('Failed to load public catalog:', error)
    packages.value = []
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void loadCatalog()
})
</script>

<template>
  <div ref="portalRoot" class="relative min-h-screen bg-[var(--bg-primary)] text-[var(--text-primary)]">
    <!-- 1. HERO SECTION -->
    <section class="relative overflow-hidden py-20 sm:py-28 lg:py-32">
      <div class="relative mx-auto max-w-5xl px-4 sm:px-6 lg:px-8 text-center" data-reveal>
        <div class="inline-flex items-center justify-center">
          <Badge variant="info" :dot="true" size="md">
            {{ t('publicSite.portal.badge') }}
          </Badge>
        </div>

        <h1 class="mt-8 text-4xl sm:text-6xl md:text-7xl font-semibold tracking-tight text-[var(--text-primary)] leading-[1.08]">
          <span class="block">{{ heroTitlePrimary }}</span>
          <span class="block mt-2 sm:mt-3">{{ heroTitlePrefix }}<span class="text-[var(--accent)]">{{ heroTitleAccent }}</span></span>
        </h1>

        <p class="mt-6 text-base sm:text-lg md:text-xl text-[var(--text-secondary)] max-w-2xl mx-auto font-normal leading-relaxed">
          {{ t('publicSite.portal.description') }}
        </p>

        <div class="mt-10 flex flex-wrap items-center justify-center gap-4">
          <Button size="lg" class="px-8 shadow-sm group" @click="browseCatalog()">
            <span>{{ t('publicSite.actions.browseProducts') }}</span>
            <ArrowRight class="w-4 h-4 ml-1.5 transition-transform duration-200 group-hover:translate-x-1" />
          </Button>
          <Button variant="secondary" size="lg" class="px-8 shadow-sm" @click="goToConsole">
            <span class="sm:hidden">{{ consoleActionCompactLabel }}</span>
            <span class="hidden sm:inline">{{ consoleActionLabel }}</span>
          </Button>
        </div>

        <div class="mt-12 flex flex-wrap items-center justify-center gap-2.5">
          <Badge variant="default" size="md">LXC / KVM Ready</Badge>
          <Badge variant="default" size="md">LXC / KVM</Badge>
          <Badge variant="default" size="md">IPv4 NAT / BGP</Badge>
        </div>
      </div>
    </section>

    <!-- 2. STAT STRIP -->
    <section class="max-w-5xl mx-auto px-4 sm:px-6 lg:px-8 py-8" data-reveal>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-5">
        <Card padding="md" class="text-center">
          <div class="text-3xl sm:text-4xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ packages.length }}
          </div>
          <div class="mt-2 text-sm font-medium text-[var(--text-secondary)]">
            {{ t('publicSite.portal.stats.packages') }}
          </div>
        </Card>
        <Card padding="md" class="text-center">
          <div class="text-3xl sm:text-4xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            {{ packages.filter((item) => item.sourceType === 'official').length }}
          </div>
          <div class="mt-2 text-sm font-medium text-[var(--text-secondary)]">
            {{ t('publicSite.portal.stats.official') }}
          </div>
        </Card>
        <Card padding="md" class="text-center">
          <div class="text-3xl sm:text-4xl font-semibold tracking-tight text-[var(--text-primary)] font-mono tabular-nums">
            8+
          </div>
          <div class="mt-2 text-sm font-medium text-[var(--text-secondary)]">
            {{ t('publicSite.portal.stats.regions') }}
          </div>
        </Card>
      </div>
    </section>

    <!-- 3. OFFICIAL DIRECT / CATALOG SECTION -->
    <section class="py-16 sm:py-24">
      <div class="max-w-5xl mx-auto px-4 sm:px-6 lg:px-8">
        <div class="text-center max-w-3xl mx-auto mb-14" data-reveal>
          <p class="text-xs font-semibold uppercase tracking-widest text-[var(--accent)] mb-3">
            {{ t('publicSite.portal.catalogLabel') }}
          </p>
          <h2 class="text-3xl sm:text-4xl md:text-5xl font-semibold tracking-tight text-[var(--text-primary)]">
            {{ t('publicSite.portal.catalogTitle') }}
          </h2>
          <p class="mt-4 text-base sm:text-lg text-[var(--text-secondary)] leading-relaxed">
            {{ t('publicSite.portal.catalogDescription') }}
          </p>
        </div>

        <div class="grid grid-cols-1 gap-8" data-reveal @mouseleave="setActiveProductLine('all')">
          <Card
            v-for="line in ecosystemCards"
            :key="line.key"
            :hoverable="true"
            padding="lg"
            class="relative transition-all duration-300 group"
            :class="{ 'ring-2 ring-[var(--accent)]': activeProductLine === line.source }"
            tabindex="0"
            role="button"
            @mouseenter="setActiveProductLine(line.source)"
            @focusin="setActiveProductLine(line.source)"
            @click="browseCatalog(line.source)"
            @keydown.enter="browseCatalog(line.source)"
          >
            <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4 pb-6 border-b border-[var(--border-color)]">
              <div class="flex items-center gap-4">
                <div class="w-12 h-12 rounded-2xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0">
                  <ShieldCheck class="w-6 h-6 stroke-[1.8]" />
                </div>
                <div>
                  <h3 class="text-xl sm:text-2xl font-semibold tracking-tight text-[var(--text-primary)]">
                    {{ line.title }}
                  </h3>
                  <p class="text-sm text-[var(--text-secondary)] mt-0.5">
                    {{ line.description }}
                  </p>
                </div>
              </div>
              <div class="flex flex-wrap items-center gap-2 shrink-0">
                <Badge variant="info" size="sm">
                  {{ t('publicSite.market.official') }}
                </Badge>
                <Badge variant="default" size="sm">
                  {{ t('publicSite.portal.stableSupply') }}
                </Badge>
              </div>
            </div>

            <div class="py-6">
              <ul class="grid grid-cols-1 sm:grid-cols-3 gap-4">
                <li
                  v-for="point in line.points.slice(0, 3)"
                  :key="point"
                  class="flex items-start gap-3 text-sm text-[var(--text-primary)]"
                >
                  <div class="w-5 h-5 rounded-full bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center shrink-0 mt-0.5">
                    <Check class="w-3 h-3 stroke-[2.2]" />
                  </div>
                  <span class="leading-relaxed">{{ point }}</span>
                </li>
              </ul>
            </div>

            <div class="pt-6 border-t border-[var(--border-color)] flex items-center justify-end">
              <Button
                variant="primary"
                size="md"
                @click.stop="browseCatalog(line.source)"
              >
                <span>{{ t('publicSite.actions.browseOfficial') }}</span>
                <ArrowRight class="w-4 h-4 ml-1.5" />
              </Button>
            </div>
          </Card>
        </div>
      </div>
    </section>

    <!-- 4. FEATURE HIGHLIGHTS -->
    <section class="py-16 sm:py-24">
      <div class="max-w-5xl mx-auto px-4 sm:px-6 lg:px-8">
        <div class="text-center max-w-3xl mx-auto mb-14" data-reveal>
          <p class="text-xs font-semibold uppercase tracking-widest text-[var(--accent)] mb-3">
            {{ t('publicSite.portal.previewLabel') }}
          </p>
          <h2 class="text-3xl sm:text-4xl md:text-5xl font-semibold tracking-tight text-[var(--text-primary)]">
            {{ t('publicSite.portal.title') }}
          </h2>
          <p class="mt-4 text-base sm:text-lg text-[var(--text-secondary)] leading-relaxed">
            {{ t('publicSite.portal.previewDescription') }}
          </p>
        </div>

        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-6" data-reveal>
          <Card
            v-for="feature in heroFeatureCards"
            :key="feature.title"
            padding="md"
            class="h-full flex flex-col transition-all duration-300"
          >
            <div class="w-12 h-12 rounded-2xl bg-[color-mix(in_srgb,var(--accent)_10%,var(--bg-surface))] text-[var(--accent)] flex items-center justify-center mb-5 shrink-0">
              <Network v-if="feature.icon === 'network'" class="w-6 h-6 stroke-[1.8]" />
              <Cpu v-else-if="feature.icon === 'compute'" class="w-6 h-6 stroke-[1.8]" />
              <ShieldCheck v-else-if="feature.icon === 'secure'" class="w-6 h-6 stroke-[1.8]" />
              <Activity v-else class="w-6 h-6 stroke-[1.8]" />
            </div>
            <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] mb-2">
              {{ feature.title }}
            </h3>
            <p class="text-sm text-[var(--text-secondary)] leading-relaxed flex-1">
              {{ feature.description }}
            </p>
          </Card>
        </div>
      </div>
    </section>

    <!-- 5. PACKAGE SPOTLIGHT -->
    <section class="py-16 sm:py-24">
      <div class="max-w-5xl mx-auto px-4 sm:px-6 lg:px-8">
        <div class="flex flex-col md:flex-row md:items-end justify-between gap-6 mb-12" data-reveal>
          <div class="max-w-2xl">
            <p class="text-xs font-semibold uppercase tracking-widest text-[var(--accent)] mb-3">
              {{ t('publicSite.portal.browseLabel') }}
            </p>
            <h2 class="text-3xl sm:text-4xl md:text-5xl font-semibold tracking-tight text-[var(--text-primary)]">
              {{ t('publicSite.portal.browseTitle') }}
            </h2>
            <p class="mt-4 text-base sm:text-lg text-[var(--text-secondary)] leading-relaxed">
              {{ t('publicSite.portal.browseDescription') }}
            </p>
          </div>

          <Button variant="secondary" size="md" class="self-start md:self-auto shrink-0 shadow-sm" @click="browseCatalog()">
            <span>{{ t('publicSite.actions.browseProducts') }}</span>
            <ArrowRight class="w-4 h-4 ml-1.5" />
          </Button>
        </div>

        <div class="grid grid-cols-1 md:grid-cols-2 gap-6" data-reveal>
          <template v-if="loading">
            <Card
              v-for="index in 4"
              :key="index"
              :loading="true"
              padding="lg"
              class="min-h-[180px]"
            />
          </template>

          <template v-else-if="spotlightPackages.length > 0">
            <Card
              v-for="pkg in spotlightPackages"
              :key="pkg.id"
              :hoverable="true"
              padding="md"
              role="button"
              tabindex="0"
              class="flex flex-col justify-between group transition-all duration-300"
              @click="openPackage(pkg)"
              @keydown.enter="openPackage(pkg)"
            >
              <div>
                <div class="flex items-center justify-between gap-2 mb-4">
                  <Badge variant="info" size="sm">
                    {{ t('publicSite.market.official') }}
                  </Badge>
                  <Badge variant="default" size="sm">
                    {{ pkg.instance_type === 'vm' ? 'KVM' : 'LXC' }}
                  </Badge>
                </div>

                <div class="text-lg font-semibold tracking-tight text-[var(--text-primary)] group-hover:text-[var(--accent)] transition-colors truncate">
                  {{ pkg.name }}
                </div>
                <p class="mt-2 text-sm text-[var(--text-secondary)] line-clamp-2 leading-relaxed">
                  {{ pkg.description || t('publicSite.portal.packageFallback') }}
                </p>
              </div>

              <div class="mt-6 pt-4 border-t border-[var(--border-color)] flex items-baseline justify-between gap-4">
                <span class="text-lg font-semibold tracking-tight text-[var(--text-primary)]">
                  {{ getPriceLabel(pkg) }}
                </span>
                <span class="text-xs text-[var(--text-tertiary)] font-mono">
                  {{ formatTraffic(pkg.monthly_traffic_limit) }}
                </span>
              </div>
            </Card>
          </template>

          <div v-else class="col-span-full">
            <EmptyState :title="t('publicSite.portal.emptyPackages')" />
          </div>
        </div>
      </div>
    </section>

    <!-- 6. CLOSING CTA BAND -->
    <section class="py-16 sm:py-24">
      <div class="max-w-5xl mx-auto px-4 sm:px-6 lg:px-8">
        <Card
          padding="lg"
          class="relative text-center bg-[var(--bg-surface)] border border-[var(--border-color)] rounded-3xl shadow-sm"
          data-reveal
        >
          <div class="max-w-3xl mx-auto py-4 sm:py-6">
            <h2 class="text-3xl sm:text-4xl md:text-5xl font-semibold tracking-tight text-[var(--text-primary)] leading-[1.12]">
              {{ heroTitlePrefix }}<span class="text-[var(--accent)]">{{ heroTitleAccent }}</span>
            </h2>
            <p class="mt-4 text-base sm:text-lg text-[var(--text-secondary)] max-w-xl mx-auto leading-relaxed">
              {{ t('publicSite.portal.authPanelDescription') }}
            </p>
            <div class="mt-10 flex flex-wrap items-center justify-center gap-4">
              <Button size="lg" class="px-8 shadow-sm group" @click="browseCatalog()">
                <span>{{ t('publicSite.actions.browseProducts') }}</span>
                <ArrowRight class="w-4 h-4 ml-1.5 transition-transform duration-200 group-hover:translate-x-1" />
              </Button>
              <Button variant="secondary" size="lg" class="px-8 shadow-sm" @click="goToConsole">
                <span class="sm:hidden">{{ consoleActionCompactLabel }}</span>
                <span class="hidden sm:inline">{{ consoleActionLabel }}</span>
              </Button>
            </div>
          </div>
        </Card>
      </div>
    </section>
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  * {
    transition-duration: 0.01ms !important;
    animation-duration: 0.01ms !important;
  }
}
</style>
