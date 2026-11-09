<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import {
  Search,
  BookOpen,
  ChevronRight,
  ArrowLeft,
  Clock,
  AlertCircle
} from 'lucide-vue-next'
import api from '@/api'
import { parseMarkdown } from '@/utils/markdown'
import { helpPath } from '@/utils/app-paths'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  EmptyState,
  Input,
  Pagination
} from '@/components/ui'

defineOptions({ name: 'HelpView' })

const route = useRoute()
const router = useRouter()
const { locale, t } = useI18n()

// 列表视图
interface HelpArticle {
  id: number
  title: string
  slug: string
  content: string
  category: string
  sort_order: number
  published: number
  created_at: string
  updated_at: string
}
const articles = ref<HelpArticle[]>([])
const loading = ref<boolean>(true)
const page = ref<number>(1)
const pageSize = ref<number>(10)
const total = ref<number>(0)
const totalPages = ref<number>(0)
const searchQuery = ref(typeof route.query.q === 'string' ? route.query.q : '')
const loadError = ref('')

// 详情视图
const currentArticle = ref<HelpArticle | null>(null)
const articleLoading = ref<boolean>(false)
const articleError = ref('')
// 标记是否已完成首次加载（用于区分初始状态和真正的404）
const articleLoaded = ref<boolean>(false)

// 分类筛选
interface CategoryWithCount {
  category: string
  count: number
}
const categories = ref<CategoryWithCount[]>([])
const selectedCategory = ref<string>(typeof route.query.category === 'string' ? route.query.category : '')

// 分类配置（从服务器加载，默认语义色彩 Token）
interface Category {
  id: string
  name: string
  color: string
}
const categoryConfig = ref<Category[]>([
  { id: 'general', name: '', color: 'var(--text-tertiary)' },
  { id: 'getting-started', name: '', color: 'var(--success)' },
  { id: 'instances', name: '', color: 'var(--accent)' },
  { id: 'networking', name: '', color: 'var(--chart-1)' },
  { id: 'billing', name: '', color: 'var(--warning)' },
  { id: 'faq', name: '', color: 'var(--error)' }
])

// 计算属性：是否正在查看文章？
const isDetailView = computed<boolean>(() => !!(route.params.slug as string | undefined))

function isHelpArticleNotFoundError(err: unknown): boolean {
  if (!err || typeof err !== 'object') return false
  const maybeError = err as { code?: unknown; statusCode?: unknown; status?: unknown }
  return maybeError.code === 'ARTICLE_NOT_FOUND' ||
    maybeError.statusCode === 404 ||
    maybeError.status === 404
}

onMounted(async (): Promise<void> => {
  // 并行加载分类配置和分类列表
  await Promise.all([loadCategoryConfig(), loadCategories()])
  
  if (route.params.slug) {
    await loadArticle(route.params.slug as string)
  } else {
    await loadArticles()
  }
})

async function loadCategoryConfig(): Promise<void> {
  try {
    const response = await api.help.categoryConfig()
    if (response.categories && response.categories.length > 0) {
      categoryConfig.value = response.categories
    }
  } catch (err) {
    console.error('Failed to load category config:', err)
    // 加载失败时使用默认配置
  }
}

watch(() => route.params.slug, async (newSlug) => {
  if (newSlug) {
    await loadArticle(newSlug as string)
  } else {
    currentArticle.value = null
    articleLoaded.value = false
    await loadArticles()
  }
})

async function loadCategories(): Promise<void> {
  try {
    const response = await api.help.categories()
    const cats = (response as { categories?: CategoryWithCount[] | string[] }).categories || []
    // 处理 string[] 和 CategoryWithCount[] 两种格式
    if (cats.length > 0 && typeof cats[0] === 'string') {
      // 将 string[] 转换为 CategoryWithCount[]
      categories.value = (cats as string[]).map(cat => ({ category: cat, count: 0 }))
    } else {
      categories.value = cats as CategoryWithCount[]
    }
  } catch (err) {
    console.error('Failed to load categories:', err)
  }
}

async function loadArticles(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    const params: { page: number; pageSize: number; category?: string; search?: string } = {
      page: page.value,
      pageSize: pageSize.value
    }
    if (selectedCategory.value) {
      params.category = selectedCategory.value
    }
    if (searchQuery.value.trim()) {
      params.search = searchQuery.value.trim()
    }
    const response = await api.help.list(params)
    const data = response as { articles?: HelpArticle[]; total?: number; totalPages?: number }
    articles.value = data.articles || []
    total.value = data.total || 0
    totalPages.value = data.totalPages || 1
  } catch (err) {
    console.error('Failed to load articles:', err)
    loadError.value = t('help.loadFailed')
  } finally {
    loading.value = false
  }
}

async function loadArticle(slug: string): Promise<void> {
  articleLoading.value = true
  articleLoaded.value = false
  articleError.value = ''
  try {
    const response = await api.help.getBySlug(slug)
    currentArticle.value = (response as { article?: HelpArticle }).article || null
  } catch (err) {
    console.error('Failed to load article:', err)
    articleError.value = isHelpArticleNotFoundError(err) ? t('help.articleNotFound') : t('help.loadFailed')
    currentArticle.value = null
  } finally {
    articleLoading.value = false
    articleLoaded.value = true
  }
}

function syncListQuery(): void {
  router.replace({
    name: 'help',
    query: {
      ...(selectedCategory.value ? { category: selectedCategory.value } : {}),
      ...(searchQuery.value.trim() ? { q: searchQuery.value.trim() } : {})
    }
  })
}

function filterByCategory(cat: string): void {
  selectedCategory.value = cat
  page.value = 1
  syncListQuery()
  loadArticles()
}

function applySearch(): void {
  page.value = 1
  syncListQuery()
  loadArticles()
}

function clearSearch(): void {
  searchQuery.value = ''
  page.value = 1
  syncListQuery()
  loadArticles()
}

function handlePageChange(newPage: number): void {
  page.value = newPage
  loadArticles()
}

function goBack(): void {
  router.push(helpPath())
}

// 计算文章 HTML
const articleHtml = computed<string>(() => {
  if (!currentArticle.value?.content) return ''
  return parseMarkdown(currentArticle.value.content)
})

function getCategoryLabel(catId: string): string {
  const cat = categoryConfig.value.find(c => c.id === catId)
  if (cat?.name) return cat.name

  const key = `help.categories.${catId.replace(/-([a-z])/g, (_, letter: string) => letter.toUpperCase())}`
  const label = t(key)
  return label === key ? catId : label
}

function getCategoryColor(catId: string): string {
  const cat = categoryConfig.value.find(c => c.id === catId)
  return cat ? cat.color : 'var(--accent)'
}

function formatDate(dateStr: string | null | undefined): string {
  if (!dateStr) return ''
  // 支援 zh-CN, zh-TW, en 三種語言
  const localeMap: Record<string, string> = {
    'zh-CN': 'zh-CN',
    'zh-TW': 'zh-TW',
    'en': 'en-US'
  }
  const localeCode = localeMap[locale.value] || 'en-US'
  return new Date(dateStr).toLocaleDateString(localeCode, { 
    year: 'numeric', month: 'long', day: 'numeric'
  })
}
</script>

<template>
  <div class="mx-auto w-full max-w-5xl px-4 py-8 sm:px-6 sm:py-12 lg:px-8 space-y-8 animate-fade-in">
    <!-- Header: Detail View -->
    <PageHeader
      v-if="isDetailView"
      :back="true"
      :back-label="$t('help.backToHelp')"
      @back="goBack"
    >
      <template #title>
        <div v-if="articleLoading || !articleLoaded" class="h-8 w-60 bg-[var(--bg-surface-soft)] rounded-xl animate-pulse" />
        <h1 v-else-if="currentArticle" class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)]">
          {{ currentArticle.title }}
        </h1>
        <h1 v-else class="text-2xl md:text-3xl font-semibold tracking-tight text-[var(--text-primary)]">
          {{ $t('help.title') }}
        </h1>
      </template>
      <template v-if="isDetailView && currentArticle" #description>
        <div class="flex flex-wrap items-center gap-2 mt-2">
          <Badge variant="default" size="sm">
            <span
              class="w-1.5 h-1.5 rounded-full mr-1 shrink-0"
              :style="{ backgroundColor: getCategoryColor(currentArticle.category) }"
            />
            {{ getCategoryLabel(currentArticle.category) }}
          </Badge>
          <span class="text-xs text-[var(--text-tertiary)] flex items-center gap-1 font-mono">
            <Clock class="w-3.5 h-3.5 shrink-0" />
            {{ $t('help.updatedAt', { date: formatDate(currentArticle.updated_at) }) }}
          </span>
        </div>
      </template>
    </PageHeader>

    <!-- Header: List View -->
    <PageHeader
      v-else
      :title="$t('help.title')"
      :description="$t('help.description')"
    >
      <template #extra>
        <Badge v-if="total > 0" variant="default" size="md">
          {{ $t('help.totalArticles', { count: total }) }}
        </Badge>
      </template>
    </PageHeader>

    <!-- ==================== Article List View ==================== -->
    <div v-if="!isDetailView" class="space-y-6">
      <!-- Search Bar -->
      <Card padding="sm" class="shadow-sm">
        <form @submit.prevent="applySearch">
          <div class="flex flex-col sm:flex-row items-stretch sm:items-center gap-2.5">
            <div class="flex-1">
              <Input
                id="help-search"
                v-model="searchQuery"
                type="search"
                size="md"
                :placeholder="$t('help.search')"
                :clearable="true"
                @clear="clearSearch"
                @enter="applySearch"
              >
                <template #prefix>
                  <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
                </template>
              </Input>
            </div>
            <div class="flex items-center gap-2 shrink-0">
              <Button
                type="submit"
                variant="primary"
                size="md"
                class="flex-1 sm:flex-none"
              >
                {{ $t('common.search') }}
              </Button>
              <Button
                v-if="searchQuery"
                type="button"
                variant="secondary"
                size="md"
                class="flex-1 sm:flex-none"
                @click="clearSearch"
              >
                {{ $t('help.clearSearch') }}
              </Button>
            </div>
          </div>
        </form>
      </Card>

      <!-- Category Filter Capsules -->
      <div v-if="categories.length > 0" class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          class="inline-flex items-center gap-1.5 px-3.5 py-1.5 rounded-full text-xs font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="!selectedCategory
            ? 'bg-[var(--accent)] text-white shadow-sm font-semibold'
            : 'bg-[var(--bg-surface)] text-[var(--text-secondary)] border border-[var(--border-color)] hover:border-[var(--border-hover)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)]'"
          @click="filterByCategory('')"
        >
          <span>{{ $t('help.all') }}</span>
        </button>
        <button
          v-for="cat in categories"
          :key="cat.category"
          type="button"
          class="inline-flex items-center gap-1.5 px-3.5 py-1.5 rounded-full text-xs font-medium transition-all duration-200 active:scale-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
          :class="selectedCategory === cat.category
            ? 'bg-[var(--accent)] text-white shadow-sm font-semibold'
            : 'bg-[var(--bg-surface)] text-[var(--text-secondary)] border border-[var(--border-color)] hover:border-[var(--border-hover)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)]'"
          @click="filterByCategory(cat.category)"
        >
          <span
            class="w-1.5 h-1.5 rounded-full shrink-0"
            :style="{ backgroundColor: selectedCategory === cat.category ? 'currentColor' : getCategoryColor(cat.category) }"
          />
          <span>{{ getCategoryLabel(cat.category) }}</span>
          <span
            v-if="cat.count > 0"
            class="ml-0.5 px-1.5 py-0.5 rounded-full text-[10px] font-mono tabular-nums leading-none"
            :class="selectedCategory === cat.category ? 'bg-white/20 text-white' : 'bg-[var(--bg-tertiary)] text-[var(--text-tertiary)]'"
          >
            {{ cat.count }}
          </span>
        </button>
      </div>

      <!-- Load Error Alert -->
      <div
        v-if="loadError"
        class="rounded-2xl border border-[var(--error)]/30 bg-[color-mix(in_srgb,var(--error)_8%,var(--bg-surface))] p-4 text-sm text-[var(--error)] shadow-sm"
      >
        <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <div class="flex items-center gap-2">
            <AlertCircle class="w-4 h-4 shrink-0 text-[var(--error)]" />
            <span>{{ loadError }}</span>
          </div>
          <Button
            variant="secondary"
            size="sm"
            @click="loadArticles"
          >
            {{ $t('common.retry') }}
          </Button>
        </div>
      </div>

      <!-- Loading Skeletons -->
      <div v-if="loading" class="space-y-3">
        <Card v-for="i in 4" :key="i" padding="md">
          <div class="animate-pulse flex items-start justify-between gap-4">
            <div class="flex-1 space-y-2.5">
              <div class="h-5 bg-[var(--bg-surface-soft)] rounded-md w-2/5"></div>
              <div class="flex items-center gap-3">
                <div class="h-4 bg-[var(--bg-surface-soft)] rounded-full w-16"></div>
                <div class="h-3.5 bg-[var(--bg-surface-soft)] rounded w-24"></div>
              </div>
            </div>
            <div class="h-5 w-5 bg-[var(--bg-surface-soft)] rounded-full"></div>
          </div>
        </Card>
      </div>

      <!-- Empty State -->
      <Card v-else-if="articles.length === 0" padding="none">
        <EmptyState
          :title="searchQuery.trim() ? $t('help.noSearchResults') : $t('help.noArticles')"
          :description="searchQuery.trim() ? $t('help.description') : ''"
        >
          <template #icon>
            <BookOpen class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template v-if="searchQuery.trim()" #action>
            <Button variant="secondary" size="sm" @click="clearSearch">
              {{ $t('help.clearSearch') }}
            </Button>
          </template>
        </EmptyState>
      </Card>

      <!-- Article List Cards -->
      <div v-else class="space-y-3">
        <RouterLink
          v-for="article in articles"
          :key="article.id"
          :to="{ name: 'help-article', params: { slug: article.slug } }"
          class="group block rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-5 md:p-6 shadow-[0_2px_8px_rgba(0,0,0,0.04)] hover:shadow-[0_8px_24px_rgba(0,0,0,0.08)] hover:border-[var(--border-hover)] transition-all duration-300 ease-[cubic-bezier(0.25,0.1,0.25,1)] active:scale-[0.99] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--accent)]"
        >
          <div class="flex items-start justify-between gap-4">
            <div class="min-w-0 flex-1 space-y-2.5">
              <h3 class="text-base md:text-lg font-semibold tracking-tight text-[var(--text-primary)] group-hover:text-[var(--accent)] transition-colors duration-200 break-words">
                {{ article.title }}
              </h3>
              <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs text-[var(--text-secondary)]">
                <Badge variant="default" size="sm">
                  <span
                    class="w-1.5 h-1.5 rounded-full shrink-0"
                    :style="{ backgroundColor: getCategoryColor(article.category) }"
                  />
                  <span>{{ getCategoryLabel(article.category) }}</span>
                </Badge>
                <span class="font-mono tabular-nums text-[var(--text-tertiary)] flex items-center gap-1">
                  <Clock class="w-3.5 h-3.5 shrink-0" />
                  {{ formatDate(article.updated_at) }}
                </span>
              </div>
            </div>
            <div class="p-2 rounded-full text-[var(--text-tertiary)] group-hover:text-[var(--accent)] group-hover:bg-[var(--bg-surface-soft)] group-hover:translate-x-0.5 transition-all duration-200 shrink-0">
              <ChevronRight class="w-5 h-5" />
            </div>
          </div>
        </RouterLink>
      </div>

      <!-- Pagination -->
      <Pagination
        v-if="totalPages > 1"
        :page="page"
        :page-size="pageSize"
        :total="total"
        @update:page="handlePageChange"
      >
        <template #total>
          {{ $t('help.totalArticles', { count: total }) }}
        </template>
      </Pagination>
    </div>

    <!-- ==================== Article Detail View ==================== -->
    <div v-else class="space-y-6">
      <!-- Loading Skeleton -->
      <Card v-if="articleLoading || !articleLoaded" padding="lg">
        <div class="animate-pulse space-y-5 py-4">
          <div class="h-7 bg-[var(--bg-surface-soft)] rounded-xl w-3/4"></div>
          <div class="flex items-center gap-3">
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-full w-20"></div>
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-28"></div>
          </div>
          <div class="pt-6 space-y-3.5">
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-full"></div>
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-11/12"></div>
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-4/5"></div>
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-full"></div>
            <div class="h-4 bg-[var(--bg-surface-soft)] rounded-md w-2/3"></div>
          </div>
        </div>
      </Card>

      <!-- Not Found / Error State -->
      <Card v-else-if="articleLoaded && !currentArticle" padding="none">
        <EmptyState
          :title="articleError || $t('help.articleNotFound')"
          :description="$t('help.loadFailed')"
        >
          <template #icon>
            <AlertCircle class="w-7 h-7 stroke-[1.5] text-[var(--error)]" />
          </template>
          <template #action>
            <Button variant="secondary" size="md" @click="goBack">
              <ArrowLeft class="w-4 h-4 mr-1.5" />
              {{ $t('help.backToHelp') }}
            </Button>
          </template>
        </EmptyState>
      </Card>

      <!-- Article Content View -->
      <div v-else class="space-y-6">
        <Card padding="none" class="shadow-sm">
          <div class="p-6 md:p-10 markdown-body" v-html="articleHtml" />
        </Card>

        <!-- Back to help navigation button -->
        <div class="flex items-center justify-between pt-2">
          <Button variant="secondary" size="md" @click="goBack">
            <ArrowLeft class="w-4 h-4 mr-1.5" />
            {{ $t('help.backToHelp') }}
          </Button>
        </div>
      </div>
    </div>
  </div>
</template>

<style>
/* ============ Markdown Apple HIG 基础样式 ============ */
.markdown-body {
  color: var(--text-primary);
  line-height: 1.75;
  font-size: 15px;
}

.markdown-body > *:first-child {
  margin-top: 0 !important;
}

.markdown-body > *:last-child {
  margin-bottom: 0 !important;
}

/* 标题 */
.markdown-body h1,
.markdown-body h2,
.markdown-body h3,
.markdown-body h4,
.markdown-body h5,
.markdown-body h6 {
  color: var(--text-primary);
  margin-top: 1.6em;
  margin-bottom: 0.6em;
  font-weight: 600;
  letter-spacing: -0.02em;
  line-height: 1.3;
}

.markdown-body h1 { font-size: 1.75em; }
.markdown-body h2 { 
  font-size: 1.4em; 
  border-bottom: 1px solid var(--border-color); 
  padding-bottom: 0.35em; 
}
.markdown-body h3 { font-size: 1.2em; }
.markdown-body h4 { font-size: 1.05em; }
.markdown-body h5 { font-size: 0.95em; }
.markdown-body h6 { font-size: 0.875em; color: var(--text-secondary); }

/* 段落 */
.markdown-body p {
  margin: 1em 0;
}

/* 链接 */
.markdown-body a {
  color: var(--accent);
  text-decoration: none;
  transition: color 0.15s;
}

.markdown-body a:hover {
  text-decoration: underline;
}

/* 图片 */
.markdown-body img {
  max-width: 100%;
  border-radius: 12px;
  border: 1px solid var(--border-color);
}

.markdown-body figure {
  margin: 1.5em 0;
}

/* 列表 */
.markdown-body ul,
.markdown-body ol {
  padding-left: 1.5em;
  margin: 1em 0;
}

.markdown-body ul {
  list-style-type: disc;
}

.markdown-body ol {
  list-style-type: decimal;
}

.markdown-body li {
  margin: 0.5em 0;
}

.markdown-body li > ul,
.markdown-body li > ol {
  margin: 0.25em 0;
}

/* 任务列表 */
.markdown-body ul:has(li > span:first-child > svg) {
  list-style: none;
  padding-left: 0;
}

/* 行内代码 */
.markdown-body code {
  background: var(--bg-surface-soft);
  padding: 0.2em 0.45em;
  border: 1px solid var(--border-color);
  border-radius: 6px;
  font-size: 0.875em;
  font-family: 'JetBrains Mono Variable', 'JetBrains Mono', 'SF Mono', 'Fira Code', 'Consolas', monospace;
}

/* 代码块 */
.markdown-body pre {
  background: var(--bg-surface-soft);
  padding: 1.1em 1.25em;
  border-radius: 12px;
  overflow-x: auto;
  margin: 1.2em 0;
  border: 1px solid var(--border-color);
}

.markdown-body pre code {
  background: none;
  padding: 0;
  border: none;
  font-size: 0.875em;
  line-height: 1.6;
}

/* 引用块 */
.markdown-body blockquote {
  border-left: 3px solid var(--accent);
  background: var(--bg-surface-soft);
  border-radius: 0 10px 10px 0;
  padding: 0.75em 1.25em;
  margin: 1.25em 0;
  color: var(--text-secondary);
}

.markdown-body blockquote p {
  margin: 0.5em 0;
}

/* 表格 */
.markdown-body table {
  width: 100%;
  border-collapse: collapse;
  margin: 1.25em 0;
  font-size: 0.875em;
  border-radius: 10px;
  overflow: hidden;
}

.markdown-body th,
.markdown-body td {
  border: 1px solid var(--border-color);
  padding: 0.65em 1em;
  text-align: left;
}

.markdown-body th {
  background: var(--bg-surface-soft);
  font-weight: 600;
}

.markdown-body tr:hover td {
  background: var(--bg-surface-soft);
}

/* 水平线 */
.markdown-body hr {
  border: none;
  border-top: 1px solid var(--border-color);
  margin: 2em 0;
}

/* 删除线 */
.markdown-body del {
  color: var(--text-tertiary);
}

/* 强调 */
.markdown-body strong {
  font-weight: 600;
  color: var(--text-primary);
}

.markdown-body em {
  font-style: italic;
}

/* 键盘按键 */
.markdown-body kbd {
  display: inline-block;
  padding: 0.2em 0.4em;
  font-size: 0.85em;
  font-family: 'JetBrains Mono Variable', 'JetBrains Mono', 'SF Mono', 'Fira Code', 'Consolas', monospace;
  background: var(--bg-surface-soft);
  border: 1px solid var(--border-color);
  border-radius: 4px;
  box-shadow: 0 1px 0 var(--border-color);
}

/* ============ 自定义 Alert 样式 ============ */
.md-alert {
  display: flex;
  align-items: flex-start;
  gap: 0.75rem;
  padding: 1rem 1.25rem;
  margin: 1.25rem 0;
  border-radius: 12px;
  border-width: 1px;
}

.md-alert svg {
  flex-shrink: 0;
  width: 1.25rem;
  height: 1.25rem;
  margin-top: 0.125rem;
}

.md-alert > div {
  flex: 1;
  font-size: 0.875rem;
  line-height: 1.5;
}

/* Alert 颜色变体 - 暗色主题 */
.dark .md-alert-info {
  background-color: var(--alert-info-bg);
  border-color: var(--alert-border);
}
.dark .md-alert-info svg,
.dark .md-alert-info > div {
  color: var(--alert-info);
}

.dark .md-alert-success {
  background-color: var(--alert-success-bg);
  border-color: var(--alert-border-success);
}
.dark .md-alert-success svg,
.dark .md-alert-success > div {
  color: var(--alert-success);
}

.dark .md-alert-warning {
  background-color: var(--alert-warning-bg);
  border-color: var(--alert-border-warning);
}
.dark .md-alert-warning svg,
.dark .md-alert-warning > div {
  color: var(--alert-warning);
}

.dark .md-alert-danger {
  background-color: var(--alert-danger-bg);
  border-color: var(--alert-border-danger);
}
.dark .md-alert-danger svg,
.dark .md-alert-danger > div {
  color: var(--alert-danger);
}

.dark .md-alert-note {
  background-color: var(--alert-note-bg);
  border-color: var(--alert-border-note);
}
.dark .md-alert-note svg,
.dark .md-alert-note > div {
  color: var(--alert-note);
}

/* Alert 颜色变体 - 亮色主题 */
.light .md-alert-info {
  background-color: var(--alert-info-bg);
  border-color: var(--alert-border);
}
.light .md-alert-info svg,
.light .md-alert-info > div {
  color: var(--alert-info);
}

.light .md-alert-success {
  background-color: var(--alert-success-bg);
  border-color: var(--alert-border-success);
}
.light .md-alert-success svg,
.light .md-alert-success > div {
  color: var(--alert-success);
}

.light .md-alert-warning {
  background-color: var(--alert-warning-bg);
  border-color: var(--alert-border-warning);
}
.light .md-alert-warning svg,
.light .md-alert-warning > div {
  color: var(--alert-warning);
}

.light .md-alert-danger {
  background-color: var(--alert-danger-bg);
  border-color: var(--alert-border-danger);
}
.light .md-alert-danger svg,
.light .md-alert-danger > div {
  color: var(--alert-danger);
}

.light .md-alert-note {
  background-color: var(--alert-note-bg);
  border-color: var(--alert-border-note);
}
.light .md-alert-note svg,
.light .md-alert-note > div {
  color: var(--alert-note);
}

@media (prefers-reduced-motion: reduce) {
  * {
    transition: none !important;
    animation: none !important;
  }
}
</style>
