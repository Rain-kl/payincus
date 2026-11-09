<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  Eye,
  EyeOff,
  Pencil,
  Trash2,
  Plus,
  RotateCw,
  Search,
  BookOpen,
  FolderPlus,
  Pin,
  Layers,
  Check,
  X
} from 'lucide-vue-next'
import api from '@/api/admin'
import IconButton from '@/components/IconButton.vue'
import DrawerModal from '@/components/DrawerModal.vue'
import {
  PageHeader,
  Card,
  Button,
  Badge,
  EmptyState,
  Input,
  Select,
  Pagination
} from '@/components/ui'
import { useToast } from '@/stores/toast'
import { parseMarkdown } from '@/utils/markdown'

defineOptions({ name: 'AdminHelpManageView' })

const { t } = useI18n()
const toast = useToast()

// Article list
interface HelpArticle {
  id: number
  title: string
  slug: string
  content: string
  category: string
  sort_order: number
  published: number
  pinned?: number
  created_at: string
  updated_at: string
}
const articles = ref<HelpArticle[]>([])
const loading = ref<boolean>(true)
const page = ref<number>(1)
const pageSize = ref<number>(20)
const total = ref<number>(0)
const totalPages = ref<number>(0)

// Filters & Search
const searchQuery = ref<string>('')
const selectedCategory = ref<string>('all')
const selectedStatus = ref<'all' | 'published' | 'draft'>('all')

// Editor
const showEditor = ref<boolean>(false)
type EditorMode = 'create' | 'edit'
const editorMode = ref<EditorMode>('create')
const editingId = ref<number | null>(null)
interface ArticleForm {
  title: string
  slug: string
  content: string
  category: string
  sortOrder: number
  published: boolean
  pinned: boolean
}
const form = ref<ArticleForm>({
  title: '',
  slug: '',
  content: '',
  category: 'general',
  sortOrder: 0,
  published: true,
  pinned: false
})
const formLoading = ref<boolean>(false)
const formError = ref<string>('')
const previewMode = ref<boolean>(false)
const showMarkdownHelp = ref<boolean>(false)

// Tab
type TabType = 'articles' | 'categories'
const activeTab = ref<TabType>('articles')

// Categories - dynamic
interface Category {
  id: string
  name: string
  color: string
}
const categories = ref<Category[]>([
  { id: 'general', name: '常规', color: '#6b7280' },
  { id: 'getting-started', name: '快速开始', color: '#22c55e' },
  { id: 'instances', name: '实例管理', color: '#3b82f6' },
  { id: 'networking', name: '网络配置', color: '#8b5cf6' },
  { id: 'billing', name: '计费相关', color: '#f59e0b' },
  { id: 'faq', name: '常见问题', color: '#ef4444' }
])

// Category editor
const showCategoryModal = ref<boolean>(false)
type CategoryMode = 'create' | 'edit'
const categoryMode = ref<CategoryMode>('create')
const editingCategoryId = ref<string | null>(null)
const categoryForm = ref<Category>({ id: '', name: '', color: '#3b82f6' })
const categoryError = ref<string>('')

// Predefined colors for categories (palette options)
const categoryColors = [
  '#6b7280', '#ef4444', '#f59e0b', '#22c55e', '#14b8a6', 
  '#3b82f6', '#8b5cf6', '#ec4899', '#06b6d4', '#84cc16'
]

onMounted(async (): Promise<void> => {
  await Promise.all([loadCategoriesFromServer(), loadArticles()])
})

// Load categories from server
async function loadCategoriesFromServer(): Promise<void> {
  try {
    const response = await api.help.categoryConfig()
    if (response.categories && response.categories.length > 0) {
      categories.value = response.categories
    }
  } catch (err) {
    console.error('Failed to load category config:', err)
  }
}

// Save categories to server
async function saveCategoriesToServer(): Promise<void> {
  try {
    await api.help.saveCategoryConfig(categories.value)
  } catch (err: any) {
    toast.error(t('admin.helpManage.saveFailed') + ': ' + (err?.message || String(err)))
    throw err
  }
}

// Category management functions
function openAddCategory(): void {
  categoryMode.value = 'create'
  editingCategoryId.value = null
  categoryForm.value = { id: '', name: '', color: '#3b82f6' }
  categoryError.value = ''
  showCategoryModal.value = true
}

function openEditCategory(cat: Category): void {
  categoryMode.value = 'edit'
  editingCategoryId.value = cat.id
  categoryForm.value = { id: cat.id, name: cat.name, color: cat.color }
  categoryError.value = ''
  showCategoryModal.value = true
}

async function saveCategory(): Promise<void> {
  if (!categoryForm.value.id || !categoryForm.value.name) {
    categoryError.value = t('admin.helpManage.fillCategoryIdAndName')
    return
  }
  
  // Validate ID format
  if (!/^[a-z0-9-]+$/.test(categoryForm.value.id)) {
    categoryError.value = t('admin.helpManage.invalidCategoryId')
    return
  }
  
  if (categoryMode.value === 'create') {
    // Check duplicate ID
    if (categories.value.some(c => c.id === categoryForm.value.id)) {
      categoryError.value = t('admin.helpManage.duplicateCategoryId')
      return
    }
    categories.value.push({ ...categoryForm.value })
  } else {
    const index = categories.value.findIndex(c => c.id === editingCategoryId.value)
    if (index !== -1) {
      categories.value[index] = { ...categoryForm.value }
    }
  }
  
  try {
    await saveCategoriesToServer()
    showCategoryModal.value = false
    toast.success(categoryMode.value === 'create' ? t('admin.helpManage.categoryAdded') : t('admin.helpManage.categoryUpdated'))
  } catch {
    await loadCategoriesFromServer()
  }
}

async function deleteCategory(cat: Category): Promise<void> {
  // Check if category is in use
  const inUse = articles.value.some(a => a.category === cat.id)
  if (inUse) {
    toast.error(t('admin.helpManage.categoryInUse'))
    return
  }
  
  if (!confirm(t('admin.helpManage.confirmDeleteCategory', { name: cat.name }))) return
  
  categories.value = categories.value.filter(c => c.id !== cat.id)
  try {
    await saveCategoriesToServer()
    toast.success(t('admin.helpManage.categoryDeleted'))
  } catch {
    await loadCategoriesFromServer()
  }
}

async function loadArticles(): Promise<void> {
  loading.value = true
  try {
    const params: Record<string, unknown> = {
      page: page.value,
      pageSize: pageSize.value
    }
    if (searchQuery.value.trim()) {
      params.search = searchQuery.value.trim()
    }
    if (selectedCategory.value !== 'all') {
      params.category = selectedCategory.value
    }
    const response = await api.help.adminList(params)
    const data = response as { articles?: HelpArticle[]; total?: number; totalPages?: number }
    articles.value = data.articles || []
    total.value = data.total || 0
    totalPages.value = data.totalPages || 1
  } catch (err: any) {
    toast.error(t('admin.helpManage.loadFailed') + ': ' + (err?.message || String(err)))
  } finally {
    loading.value = false
  }
}

async function handleRefresh(): Promise<void> {
  await Promise.all([loadCategoriesFromServer(), loadArticles()])
}

function openCreate(): void {
  editorMode.value = 'create'
  editingId.value = null
  form.value = {
    title: '',
    slug: '',
    content: '',
    category: categories.value[0]?.id || 'general',
    sortOrder: 0,
    published: true,
    pinned: false
  }
  formError.value = ''
  previewMode.value = false
  showEditor.value = true
}

async function openEdit(article: HelpArticle): Promise<void> {
  editorMode.value = 'edit'
  editingId.value = article.id
  formLoading.value = true
  formError.value = ''
  previewMode.value = false
  showEditor.value = true
  
  try {
    const response = await api.help.adminGet(article.id)
    const data = response as { article?: HelpArticle }
    const a = data.article
    if (!a) throw new Error('Article not found')
    form.value = {
      title: a.title,
      slug: a.slug,
      content: a.content,
      category: a.category || 'general',
      sortOrder: a.sort_order || 0,
      published: a.published === 1,
      pinned: a.pinned === 1
    }
  } catch (err: any) {
    toast.error(t('admin.helpManage.loadFailed') + ': ' + (err?.message || String(err)))
    showEditor.value = false
  } finally {
    formLoading.value = false
  }
}

async function saveArticle(): Promise<void> {
  if (!form.value.title || !form.value.slug || !form.value.content) {
    formError.value = t('admin.helpManage.fillRequired')
    return
  }
  
  // Validate slug format
  if (!/^[a-z0-9-]+$/.test(form.value.slug)) {
    formError.value = t('admin.helpManage.invalidSlug')
    return
  }
  
  formLoading.value = true
  formError.value = ''
  
  try {
    if (editorMode.value === 'create') {
      await api.help.create({
        title: form.value.title,
        slug: form.value.slug,
        content: form.value.content,
        category: form.value.category,
        sortOrder: form.value.sortOrder,
        published: form.value.published,
        pinned: form.value.pinned
      })
      toast.success(t('admin.helpManage.articleCreated'))
    } else {
      if (editingId.value === null) throw new Error('Invalid article ID')
      await api.help.update(editingId.value, {
        title: form.value.title,
        slug: form.value.slug,
        content: form.value.content,
        category: form.value.category,
        sortOrder: form.value.sortOrder,
        published: form.value.published,
        pinned: form.value.pinned
      })
      toast.success(t('admin.helpManage.articleUpdated'))
    }
    showEditor.value = false
    await loadArticles()
  } catch (err: any) {
    formError.value = err?.message || String(err)
  } finally {
    formLoading.value = false
  }
}

async function deleteArticle(article: HelpArticle): Promise<void> {
  if (!confirm(t('admin.helpManage.confirmDelete', { title: article.title }))) return
  
  try {
    await api.help.delete(article.id)
    toast.success(t('admin.helpManage.articleDeleted'))
    await loadArticles()
  } catch (err: any) {
    toast.error(t('admin.helpManage.deleteFailed') + ': ' + (err?.message || String(err)))
  }
}

async function togglePublished(article: HelpArticle): Promise<void> {
  try {
    await api.help.update(article.id, { published: article.published === 0 })
    toast.success(article.published === 1 ? t('admin.helpManage.articleHidden') : t('admin.helpManage.articlePublished'))
    await loadArticles()
  } catch (err: any) {
    toast.error(t('admin.helpManage.operationFailed') + ': ' + (err?.message || String(err)))
  }
}

// Auto-generate slug from title
function generateSlug(): void {
  if (editorMode.value === 'create' && form.value.title && !form.value.slug) {
    form.value.slug = form.value.title
      .toLowerCase()
      .replace(/[^a-z0-9\u4e00-\u9fa5]+/g, '-')
      .replace(/^-|-$/g, '')
      .slice(0, 50)
  }
}

// Computed preview HTML
const previewHtml = computed<string>(() => {
  return parseMarkdown(form.value.content || '')
})

function getCategoryLabel(catId: string): string {
  const cat = categories.value.find(c => c.id === catId)
  return cat ? cat.name : catId
}

function getCategoryColor(catId: string): string {
  const cat = categories.value.find(c => c.id === catId)
  return cat ? cat.color : '#6b7280'
}

function getCategoryArticleCount(catId: string): number {
  return articles.value.filter(a => a.category === catId).length
}

function formatDate(dateStr: string | null | undefined): string {
  if (!dateStr) return '-'
  return new Date(dateStr).toLocaleDateString('zh-CN', { 
    month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' 
  })
}

// Filtered articles by status (client side)
const displayedArticles = computed(() => {
  if (selectedStatus.value === 'published') {
    return articles.value.filter(a => a.published === 1)
  }
  if (selectedStatus.value === 'draft') {
    return articles.value.filter(a => a.published === 0)
  }
  return articles.value
})

const publishedCount = computed(() => articles.value.filter(a => a.published === 1).length)
const pinnedCount = computed(() => articles.value.filter(a => a.pinned === 1).length)

// Options for filters and select
const categoryFilterOptions = computed(() => [
  { value: 'all', label: t('common.all') },
  ...categories.value.map(c => ({ value: c.id, label: c.name }))
])

const statusFilterOptions = computed(() => [
  { value: 'all', label: t('common.all') },
  { value: 'published', label: t('admin.helpManage.published') },
  { value: 'draft', label: t('admin.helpManage.draft') }
])

const categoryFormOptions = computed(() =>
  categories.value.map(c => ({ value: c.id, label: c.name }))
)

const hasActiveFilters = computed(() => {
  return searchQuery.value.trim() !== '' || selectedCategory.value !== 'all' || selectedStatus.value !== 'all'
})

function resetFilters(): void {
  searchQuery.value = ''
  selectedCategory.value = 'all'
  selectedStatus.value = 'all'
  page.value = 1
  void loadArticles()
}
</script>

<template>
  <div class="kawaii-page nimbus-view space-y-6 animate-fade-in">
    <!-- Apple Page Header -->
    <PageHeader
      :title="t('admin.helpManage.title')"
      :description="t('admin.helpManage.description')"
    >
      <template #actions>
        <Button
          variant="secondary"
          size="sm"
          :loading="loading"
          @click="handleRefresh"
        >
          <template #icon>
            <RotateCw class="w-4 h-4" :class="{ 'animate-spin': loading }" />
          </template>
          {{ t('common.refresh') }}
        </Button>
        <Button
          v-if="activeTab === 'articles'"
          variant="primary"
          size="sm"
          @click="openCreate"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2.5]" />
          </template>
          {{ t('admin.helpManage.create') }}
        </Button>
        <Button
          v-if="activeTab === 'categories'"
          variant="primary"
          size="sm"
          @click="openAddCategory"
        >
          <template #icon>
            <Plus class="w-4 h-4 stroke-[2.5]" />
          </template>
          {{ t('admin.helpManage.addCategory') }}
        </Button>
      </template>
    </PageHeader>

    <!-- Overview Statistics Cards -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.helpManage.articlesTab') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <BookOpen class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ total }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.helpManage.published') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--success)_12%,var(--bg-surface))] text-[var(--success)] border border-[color-mix(in_srgb,var(--success)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Eye class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--success)]">
          {{ publishedCount }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.helpManage.categoriesTab') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] border border-[color-mix(in_srgb,var(--accent)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Layers class="w-4 h-4" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--text-primary)]">
          {{ categories.length }}
        </div>
      </Card>

      <Card hoverable padding="sm" class="group">
        <div class="flex items-center justify-between">
          <span class="text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)]">
            {{ t('admin.helpManage.pinned') }}
          </span>
          <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-[color-mix(in_srgb,var(--warning)_12%,var(--bg-surface))] text-[var(--warning)] border border-[color-mix(in_srgb,var(--warning)_24%,var(--bg-surface))] group-hover:scale-105 transition-transform duration-300">
            <Pin class="w-4 h-4 rotate-45" />
          </div>
        </div>
        <div class="mt-3 font-mono text-2xl lg:text-3xl font-semibold tracking-tight tabular-nums text-[var(--warning)]">
          {{ pinnedCount }}
        </div>
      </Card>
    </div>

    <!-- Apple Segmented Control for Tabs & Filter Summary -->
    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
      <div class="flex items-center gap-1.5 p-1 rounded-2xl bg-[var(--bg-surface-soft)] border border-[var(--border-color)] overflow-x-auto max-w-full w-fit">
        <button
          type="button"
          class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none"
          :class="activeTab === 'articles'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
          @click="activeTab = 'articles'"
        >
          <BookOpen class="w-4 h-4" />
          <span>{{ t('admin.helpManage.articlesTab') }}</span>
          <Badge size="sm" :variant="activeTab === 'articles' ? 'info' : 'default'">{{ total }}</Badge>
        </button>
        <button
          type="button"
          class="flex items-center gap-2 px-4 py-2 rounded-xl text-sm font-medium transition-all duration-200 whitespace-nowrap active:scale-[0.98] select-none"
          :class="activeTab === 'categories'
            ? 'bg-[var(--bg-surface)] text-[var(--text-primary)] shadow-sm font-semibold'
            : 'text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface)]/40'"
          @click="activeTab = 'categories'"
        >
          <FolderPlus class="w-4 h-4" />
          <span>{{ t('admin.helpManage.categoriesTab') }}</span>
          <Badge size="sm" :variant="activeTab === 'categories' ? 'info' : 'default'">{{ categories.length }}</Badge>
        </button>
      </div>

      <div v-if="activeTab === 'articles'" class="text-xs text-[var(--text-secondary)] flex items-center gap-2">
        <span>共 {{ total }} 篇文档</span>
        <Button
          v-if="hasActiveFilters"
          variant="ghost"
          size="sm"
          class="text-[var(--text-secondary)]"
          @click="resetFilters"
        >
          重置筛选
        </Button>
      </div>
    </div>

    <!-- Categories Tab -->
    <div v-if="activeTab === 'categories'" class="space-y-4">
      <div v-if="categories.length > 0" class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Card
          v-for="cat in categories"
          :key="cat.id"
          hoverable
          padding="sm"
          class="group"
        >
          <div class="flex items-center justify-between gap-3">
            <div class="flex min-w-0 items-center gap-3">
              <div class="w-10 h-10 rounded-2xl flex items-center justify-center shrink-0 border border-[var(--border-color)] bg-[var(--bg-surface-soft)]">
                <span
                  class="w-4 h-4 rounded-full ring-2 ring-[var(--bg-surface)] shadow-sm"
                  :style="{ backgroundColor: cat.color }"
                />
              </div>
              <div class="min-w-0">
                <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ cat.name }}</div>
                <div class="flex items-center gap-2 mt-0.5">
                  <span class="truncate font-mono text-xs text-[var(--text-tertiary)]">{{ cat.id }}</span>
                  <span class="text-[var(--border-color)]">•</span>
                  <span class="text-xs text-[var(--text-secondary)]">{{ getCategoryArticleCount(cat.id) }} 篇</span>
                </div>
              </div>
            </div>
            <div class="flex shrink-0 items-center gap-1.5">
              <Button variant="ghost" size="sm" @click="openEditCategory(cat)">
                {{ t('common.edit') }}
              </Button>
              <Button variant="ghost" size="sm" class="text-[var(--error)] hover:text-[var(--error)]" @click="deleteCategory(cat)">
                {{ t('common.delete') }}
              </Button>
            </div>
          </div>
        </Card>
      </div>

      <Card v-else padding="none">
        <EmptyState
          :title="t('admin.helpManage.noCategories')"
          description="创建分类以便更清晰地归类和整理帮助文章"
        >
          <template #icon>
            <FolderPlus class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template #action>
            <Button variant="primary" size="sm" @click="openAddCategory">
              <template #icon><Plus class="w-4 h-4 stroke-[2.5]" /></template>
              {{ t('admin.helpManage.addCategory') }}
            </Button>
          </template>
        </EmptyState>
      </Card>
    </div>

    <!-- Articles Tab -->
    <div v-else-if="activeTab === 'articles'" class="space-y-4">
      <!-- Search & Filters Toolbar -->
      <Card padding="sm">
        <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-12 gap-3">
          <div class="lg:col-span-6">
            <Input
              v-model="searchQuery"
              size="sm"
              placeholder="搜索文档标题或 URL..."
              clearable
              @enter="page = 1; loadArticles()"
              @clear="page = 1; loadArticles()"
            >
              <template #prefix>
                <Search class="w-4 h-4 text-[var(--text-tertiary)]" />
              </template>
            </Input>
          </div>
          <div class="lg:col-span-3">
            <Select
              v-model="selectedCategory"
              size="sm"
              :options="categoryFilterOptions"
              @change="page = 1; loadArticles()"
            />
          </div>
          <div class="lg:col-span-3">
            <Select
              v-model="selectedStatus"
              size="sm"
              :options="statusFilterOptions"
            />
          </div>
        </div>
      </Card>

      <!-- Loading State -->
      <Card v-if="loading && articles.length === 0" padding="lg" class="text-center">
        <div class="flex flex-col items-center justify-center py-12">
          <RotateCw class="w-8 h-8 animate-spin text-[var(--accent)]" />
          <p class="text-sm text-[var(--text-secondary)] mt-4 font-medium">{{ t('common.loading') }}</p>
        </div>
      </Card>

      <!-- Empty State -->
      <Card v-else-if="articles.length === 0" padding="none">
        <EmptyState
          :title="t('admin.helpManage.noArticles')"
          :description="hasActiveFilters ? '未找到符合条件的文档，请尝试重置筛选' : '创建第一篇文档，为用户提供清晰明了的指引'"
        >
          <template #icon>
            <BookOpen class="w-7 h-7 stroke-[1.5] text-[var(--text-tertiary)]" />
          </template>
          <template #action>
            <Button v-if="hasActiveFilters" variant="secondary" size="sm" @click="resetFilters">
              重置筛选
            </Button>
            <Button v-else variant="primary" size="sm" @click="openCreate">
              <template #icon><Plus class="w-4 h-4 stroke-[2.5]" /></template>
              {{ t('admin.helpManage.createFirst') }}
            </Button>
          </template>
        </EmptyState>
      </Card>

      <!-- Main Articles Table & Mobile Cards -->
      <template v-else>
        <div class="card overflow-hidden">
          <!-- Mobile Card List -->
          <div class="space-y-3 p-4 lg:hidden">
            <div
              v-for="article in displayedArticles"
              :key="article.id"
              class="rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-4 shadow-[0_2px_8px_rgba(0,0,0,0.02)] transition-all duration-200"
            >
              <div class="flex items-start justify-between gap-3">
                <div class="min-w-0">
                  <div class="flex items-center gap-2">
                    <span v-if="article.pinned" class="inline-flex items-center justify-center w-5 h-5 rounded-md bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] shrink-0" :title="t('admin.helpManage.pinned')">
                      <Pin class="w-3 h-3 rotate-45" />
                    </span>
                    <div class="truncate text-sm font-semibold text-[var(--text-primary)]">{{ article.title }}</div>
                  </div>
                  <div class="mt-1 break-all font-mono text-xs text-[var(--text-tertiary)]">/help/{{ article.slug }}</div>
                </div>
                <Badge :variant="article.published ? 'success' : 'default'" size="sm" class="shrink-0">
                  {{ article.published ? t('admin.helpManage.published') : t('admin.helpManage.draft') }}
                </Badge>
              </div>

              <div class="mt-3 flex flex-wrap items-center gap-3 text-xs text-[var(--text-secondary)]">
                <div class="flex min-w-0 items-center gap-1.5">
                  <span
                    class="h-2 w-2 flex-shrink-0 rounded-full"
                    :style="{ backgroundColor: getCategoryColor(article.category) }"
                  ></span>
                  <span class="truncate font-medium">{{ getCategoryLabel(article.category) }}</span>
                </div>
                <span class="text-[var(--border-color)]">•</span>
                <span class="font-mono tabular-nums text-[var(--text-tertiary)]">{{ formatDate(article.updated_at) }}</span>
              </div>

              <div class="mt-4 flex flex-wrap items-center gap-2 border-t border-[var(--border-color)] pt-3">
                <Button variant="ghost" size="sm" @click="openEdit(article)">
                  <template #icon><Pencil class="w-3.5 h-3.5" /></template>
                  {{ t('common.edit') }}
                </Button>
                <Button variant="ghost" size="sm" @click="togglePublished(article)">
                  <template #icon>
                    <EyeOff v-if="article.published" class="w-3.5 h-3.5" />
                    <Eye v-else class="w-3.5 h-3.5" />
                  </template>
                  {{ article.published ? t('admin.helpManage.hide') : t('admin.helpManage.publish') }}
                </Button>
                <Button variant="ghost" size="sm" class="text-[var(--error)] hover:text-[var(--error)]" @click="deleteArticle(article)">
                  <template #icon><Trash2 class="w-3.5 h-3.5" /></template>
                  {{ t('common.delete') }}
                </Button>
              </div>
            </div>
          </div>

          <!-- Desktop Fixed-Width Table -->
          <div class="hidden overflow-hidden lg:block">
            <table class="w-full table-fixed">
              <thead class="bg-[var(--bg-surface-soft)] border-b border-[var(--border-color)]">
                <tr>
                  <th class="w-[34%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] px-5 py-3.5 select-none">{{ t('admin.helpManage.articleTitleCol') }}</th>
                  <th class="w-[18%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] px-4 py-3.5 select-none">{{ t('admin.helpManage.categoryCol') }}</th>
                  <th class="w-[12%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] px-4 py-3.5 select-none">{{ t('admin.helpManage.statusCol') }}</th>
                  <th class="w-[18%] text-left text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] px-4 py-3.5 select-none">{{ t('admin.helpManage.updatedAtCol') }}</th>
                  <th class="w-[18%] text-right text-xs font-semibold uppercase tracking-wider text-[var(--text-secondary)] px-5 py-3.5 select-none">{{ t('admin.helpManage.actionsCol') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-[var(--border-color)]">
                <tr
                  v-for="article in displayedArticles"
                  :key="article.id"
                  class="hover:bg-[var(--bg-surface-soft)]/50 transition-colors duration-150"
                >
                  <td class="px-5 py-3.5 truncate">
                    <div class="flex items-center gap-2 min-w-0">
                      <span v-if="article.pinned" class="inline-flex items-center justify-center w-4 h-4 rounded bg-[color-mix(in_srgb,var(--accent)_12%,var(--bg-surface))] text-[var(--accent)] shrink-0" :title="t('admin.helpManage.pinned')">
                        <Pin class="w-2.5 h-2.5 rotate-45" />
                      </span>
                      <span class="truncate font-semibold text-sm text-[var(--text-primary)]">{{ article.title }}</span>
                    </div>
                    <div class="truncate text-xs font-mono text-[var(--text-tertiary)] mt-0.5">/help/{{ article.slug }}</div>
                  </td>
                  <td class="px-4 py-3.5">
                    <div class="flex min-w-0 items-center gap-2">
                      <span
                        class="w-2.5 h-2.5 rounded-full flex-shrink-0"
                        :style="{ backgroundColor: getCategoryColor(article.category) }"
                      ></span>
                      <span class="truncate text-sm font-medium text-[var(--text-secondary)]">{{ getCategoryLabel(article.category) }}</span>
                    </div>
                  </td>
                  <td class="px-4 py-3.5">
                    <Badge :variant="article.published ? 'success' : 'default'" size="sm">
                      {{ article.published ? t('admin.helpManage.published') : t('admin.helpManage.draft') }}
                    </Badge>
                  </td>
                  <td class="truncate px-4 py-3.5 text-xs font-mono tabular-nums text-[var(--text-secondary)]">{{ formatDate(article.updated_at) }}</td>
                  <td class="px-5 py-3.5 text-right">
                    <div class="flex items-center justify-end gap-1">
                      <IconButton :label="t('common.edit')" @click="openEdit(article)"><Pencil class="w-4 h-4" /></IconButton>
                      <IconButton :label="article.published ? t('admin.helpManage.hide') : t('admin.helpManage.publish')" @click="togglePublished(article)"><EyeOff v-if="article.published" class="w-4 h-4" /><Eye v-else class="w-4 h-4" /></IconButton>
                      <IconButton :label="t('common.delete')" variant="danger" @click="deleteArticle(article)"><Trash2 class="w-4 h-4" /></IconButton>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <!-- Apple Pagination -->
        <div v-if="total > pageSize" class="pt-2">
          <Pagination
            v-model:page="page"
            v-model:page-size="pageSize"
            :total="total"
            @change="loadArticles"
          />
        </div>
      </template>
    </div>

    <!-- Article Editor Modal -->
    <DrawerModal
      :show="showEditor"
      max-width="max-w-5xl"
      raw
      @close="showEditor = false"
    >
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface)]">
        <div class="min-w-0 flex-1">
          <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">
            {{ editorMode === 'create' ? t('admin.helpManage.createArticle') : t('admin.helpManage.editArticle') }}
          </h3>
          <p class="text-xs text-[var(--text-secondary)] mt-0.5 truncate">
            {{ editorMode === 'create' ? '支持 Markdown 语法与自定义 Alert 提示框' : ('ID: ' + editingId) }}
          </p>
        </div>
        <div class="flex flex-shrink-0 items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            type="button"
            @click="previewMode = !previewMode"
          >
            <template #icon>
              <Eye v-if="!previewMode" class="w-3.5 h-3.5" />
              <Pencil v-else class="w-3.5 h-3.5" />
            </template>
            {{ previewMode ? t('common.edit') : t('admin.helpManage.preview') }}
          </Button>
          <button
            type="button"
            class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all"
            @click="showEditor = false"
          >
            <X class="w-4 h-4" />
          </button>
        </div>
      </div>

      <!-- Content Area -->
      <div class="flex flex-1 overflow-hidden min-h-[500px]">
        <!-- Form Mode -->
        <div v-if="!previewMode" class="flex-1 space-y-5 overflow-y-auto p-6">
          <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <Input
                v-model="form.title"
                :label="t('admin.helpManage.articleTitle') + ' *'"
                :placeholder="t('admin.helpManage.titlePlaceholder')"
                @blur="generateSlug"
              />
            </div>
            <div>
              <Input
                v-model="form.slug"
                :label="t('admin.helpManage.urlSlug') + ' *'"
                :placeholder="t('admin.helpManage.urlSlugPlaceholder')"
                class="font-mono"
              />
            </div>
          </div>

          <div class="grid grid-cols-1 sm:grid-cols-3 gap-4">
            <div>
              <Select
                v-model="form.category"
                :label="t('admin.helpManage.category')"
                :options="categoryFormOptions"
              />
            </div>
            <div>
              <Input
                v-model.number="form.sortOrder"
                type="number"
                :label="t('admin.helpManage.sortOrder')"
                min="0"
                class="font-mono"
              />
            </div>
            <div class="flex flex-col justify-end pb-1.5">
              <div class="flex items-center gap-6">
                <!-- Published Toggle -->
                <label class="flex items-center gap-2.5 cursor-pointer select-none">
                  <div class="relative">
                    <input
                      v-model="form.published"
                      type="checkbox"
                      class="sr-only peer"
                    />
                    <div
                      class="w-10 h-6 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] peer-checked:bg-[var(--accent)] peer-checked:border-[var(--accent)] transition-colors duration-200"
                    ></div>
                    <div
                      class="absolute left-1 top-1 w-4 h-4 bg-white rounded-full shadow-sm transition-transform duration-200 peer-checked:translate-x-4"
                    ></div>
                  </div>
                  <span class="text-xs font-medium text-[var(--text-secondary)]">
                    {{ form.published ? t('admin.helpManage.published') : t('admin.helpManage.draft') }}
                  </span>
                </label>

                <!-- Pinned Toggle -->
                <label class="flex items-center gap-2.5 cursor-pointer select-none">
                  <div class="relative">
                    <input
                      v-model="form.pinned"
                      type="checkbox"
                      class="sr-only peer"
                    />
                    <div
                      class="w-10 h-6 rounded-full bg-[var(--bg-surface-soft)] border border-[var(--border-color)] peer-checked:bg-[var(--accent)] peer-checked:border-[var(--accent)] transition-colors duration-200"
                    ></div>
                    <div
                      class="absolute left-1 top-1 w-4 h-4 bg-white rounded-full shadow-sm transition-transform duration-200 peer-checked:translate-x-4"
                    ></div>
                  </div>
                  <span class="text-xs font-medium text-[var(--text-secondary)]">
                    {{ form.pinned ? t('admin.helpManage.pinned') : t('admin.helpManage.notPinned') }}
                  </span>
                </label>
              </div>
            </div>
          </div>

          <!-- Markdown Content -->
          <div>
            <div class="mb-2 flex items-center justify-between">
              <label class="text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
                {{ t('admin.helpManage.contentMarkdown') }} *
              </label>
              <Button
                variant="ghost"
                size="sm"
                type="button"
                @click="showMarkdownHelp = !showMarkdownHelp"
              >
                {{ showMarkdownHelp ? t('admin.helpManage.hideMarkdownHelp') : t('admin.helpManage.showMarkdownHelp') }}
              </Button>
            </div>

            <!-- Markdown Syntax Help -->
            <div
              v-if="showMarkdownHelp"
              class="mb-3 space-y-3 rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] p-4 text-xs text-[var(--text-secondary)] transition-all"
            >
              <div class="grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-1.5">
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]"># 标题</code> {{ t('admin.helpManage.markdownHeading1') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">## 标题</code> {{ t('admin.helpManage.markdownHeading2') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">**粗体**</code> {{ t('admin.helpManage.markdownBold') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">*斜体*</code> {{ t('admin.helpManage.markdownItalic') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">[链接](url)</code> {{ t('admin.helpManage.markdownLink') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">![图片](url)</code> {{ t('admin.helpManage.markdownImage') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">- 列表</code> {{ t('admin.helpManage.markdownUnorderedList') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">1. 列表</code> {{ t('admin.helpManage.markdownOrderedList') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">`行内代码`</code> {{ t('admin.helpManage.markdownInlineCode') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">```代码块```</code> {{ t('admin.helpManage.markdownCodeBlock') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">&gt; 引用</code> {{ t('admin.helpManage.markdownQuote') }}</div>
                <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">---</code> {{ t('admin.helpManage.markdownHr') }}</div>
              </div>
              <div class="border-t border-[var(--border-color)] pt-2.5">
                <div class="mb-1.5 font-semibold text-[var(--text-primary)]">{{ t('admin.helpManage.customAlerts') }}</div>
                <div class="grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-1.5">
                  <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">?{info}[text]</code> {{ t('admin.helpManage.alertInfo') }}</div>
                  <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">?{success}[text]</code> {{ t('admin.helpManage.alertSuccess') }}</div>
                  <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">?{warning}[text]</code> {{ t('admin.helpManage.alertWarning') }}</div>
                  <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">?{danger}[text]</code> {{ t('admin.helpManage.alertDanger') }}</div>
                  <div><code class="rounded bg-[var(--bg-surface)] px-1.5 py-0.5 font-mono text-[var(--text-primary)] border border-[var(--border-color)]">?{note}[text]</code> {{ t('admin.helpManage.alertNote') }}</div>
                </div>
              </div>
            </div>

            <textarea
              v-model="form.content"
              class="w-full rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface-soft)] text-[var(--text-primary)] placeholder-[var(--text-tertiary)] p-4 font-mono text-sm leading-relaxed focus:outline-none focus:ring-2 focus:ring-[var(--accent)] focus:border-transparent transition-all duration-200 resize-y"
              style="min-height: 280px; max-height: calc(100vh - 420px);"
              placeholder="# 标题&#10;&#10;正文内容...&#10;&#10;```bash&#10;# 运行示例&#10;sudo systemctl status incus&#10;```&#10;&#10;?{info}[提示信息]&#10;?{success}[成功信息]"
            ></textarea>
          </div>

          <div
            v-if="formError"
            class="p-3.5 rounded-xl bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_24%,var(--bg-surface))] text-xs text-[var(--error)] font-medium"
          >
            {{ formError }}
          </div>
        </div>

        <!-- Preview Mode -->
        <div v-else class="flex-1 overflow-y-auto p-6 bg-[var(--bg-surface-soft)]/30">
          <div class="max-w-3xl mx-auto rounded-2xl border border-[var(--border-color)] bg-[var(--bg-surface)] p-6 md:p-8 shadow-sm">
            <h1 class="text-2xl font-bold tracking-tight text-[var(--text-primary)] mb-4">
              {{ form.title || t('admin.helpManage.noTitle') }}
            </h1>
            <div class="markdown-body" v-html="previewHtml"></div>
          </div>
        </div>
      </div>

      <!-- Footer -->
      <div class="flex items-center justify-end gap-3 border-t border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface-soft)]/50">
        <Button variant="secondary" size="sm" type="button" @click="showEditor = false">
          {{ t('common.cancel') }}
        </Button>
        <Button variant="primary" size="sm" :loading="formLoading" type="button" @click="saveArticle">
          {{ formLoading ? t('common.loading') : t('common.save') }}
        </Button>
      </div>
    </DrawerModal>

    <!-- Category Modal -->
    <DrawerModal
      :show="showCategoryModal"
      max-width="max-w-md"
      raw
      @close="showCategoryModal = false"
    >
      <div class="flex items-center justify-between border-b border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface)]">
        <h3 class="text-lg font-semibold tracking-tight text-[var(--text-primary)] truncate">
          {{ categoryMode === 'create' ? t('admin.helpManage.addCategory') : t('common.edit') }}
        </h3>
        <button
          type="button"
          class="inline-flex items-center justify-center w-8 h-8 rounded-full text-[var(--text-secondary)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-surface-soft)] active:scale-95 transition-all"
          @click="showCategoryModal = false"
        >
          <X class="w-4 h-4" />
        </button>
      </div>

      <div class="p-6 space-y-4">
        <div>
          <Input
            v-model="categoryForm.id"
            :label="t('admin.helpManage.categoryId') + ' *'"
            placeholder="getting-started"
            :disabled="categoryMode === 'edit'"
            :hint="t('admin.helpManage.categoryIdHint')"
            class="font-mono"
          />
        </div>

        <div>
          <Input
            v-model="categoryForm.name"
            :label="t('admin.helpManage.categoryName') + ' *'"
            placeholder="Getting Started"
          />
        </div>

        <div>
          <label class="mb-2 block text-xs font-semibold tracking-wide text-[var(--text-secondary)] select-none">
            {{ t('admin.helpManage.categoryColor') }}
          </label>
          <div class="flex flex-wrap gap-2.5">
            <button
              v-for="color in categoryColors"
              :key="color"
              type="button"
              class="h-9 w-9 rounded-xl border border-black/10 dark:border-white/10 transition-all duration-200 active:scale-90 flex items-center justify-center"
              :class="categoryForm.color === color ? 'ring-2 ring-[var(--accent)] ring-offset-2 ring-offset-[var(--bg-surface)] scale-105 shadow-sm' : 'hover:scale-105'"
              :style="{ backgroundColor: color }"
              @click="categoryForm.color = color"
            >
              <Check v-if="categoryForm.color === color" class="w-4 h-4 text-white drop-shadow" />
            </button>
          </div>
        </div>

        <div
          v-if="categoryError"
          class="p-3 rounded-xl bg-[color-mix(in_srgb,var(--error)_12%,var(--bg-surface))] border border-[color-mix(in_srgb,var(--error)_24%,var(--bg-surface))] text-xs text-[var(--error)] font-medium"
        >
          {{ categoryError }}
        </div>
      </div>

      <div class="flex items-center justify-end gap-3 border-t border-[var(--border-color)] px-6 py-4 bg-[var(--bg-surface-soft)]/50">
        <Button variant="secondary" size="sm" type="button" @click="showCategoryModal = false">
          {{ t('common.cancel') }}
        </Button>
        <Button variant="primary" size="sm" type="button" @click="saveCategory">
          {{ t('common.save') }}
        </Button>
      </div>
    </DrawerModal>
  </div>
</template>

<style>
/* ============ Markdown 基础样式 ============ */
.markdown-body {
  color: var(--text-primary);
  line-height: 1.7;
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
  margin-top: 1.5em;
  margin-bottom: 0.5em;
  font-weight: 600;
  line-height: 1.3;
}

.markdown-body h1 { font-size: 1.75em; }
.markdown-body h2 { 
  font-size: 1.5em; 
  border-bottom: 1px solid var(--border-color); 
  padding-bottom: 0.3em; 
}
.markdown-body h3 { font-size: 1.25em; }
.markdown-body h4 { font-size: 1.1em; }

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
  border-radius: 8px;
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
  background: var(--bg-tertiary);
  padding: 0.2em 0.4em;
  border-radius: 4px;
  font-size: 0.9em;
  font-family: 'SF Mono', 'Fira Code', 'Consolas', monospace;
}

/* 代码块 */
.markdown-body pre {
  background: var(--bg-tertiary);
  padding: 1em;
  border-radius: 8px;
  overflow-x: auto;
  margin: 1em 0;
  border: 1px solid var(--border-color);
}

.markdown-body pre code {
  background: none;
  padding: 0;
  font-size: 0.875em;
  line-height: 1.6;
}

/* 引用块 */
.markdown-body blockquote {
  border-left: 4px solid var(--accent);
  padding-left: 1em;
  margin: 1em 0;
  color: var(--text-secondary);
}

.markdown-body blockquote p {
  margin: 0.5em 0;
}

/* 表格 */
.markdown-body table {
  width: 100%;
  border-collapse: collapse;
  margin: 1em 0;
  font-size: 0.9em;
}

.markdown-body th,
.markdown-body td {
  border: 1px solid var(--border-color);
  padding: 0.6em 1em;
  text-align: left;
}

.markdown-body th {
  background: var(--bg-tertiary);
  font-weight: 600;
}

.markdown-body tr:hover td {
  background: var(--bg-secondary);
}

/* 水平线 */
.markdown-body hr {
  border: none;
  border-top: 1px solid var(--border-color);
  margin: 2em 0;
}

/* 强调 */
.markdown-body strong {
  font-weight: 600;
  color: var(--text-primary);
}

/* ============ 自定义 Alert 样式 ============ */
.md-alert {
  display: flex;
  align-items: flex-start;
  gap: 0.75rem;
  padding: 1rem;
  margin: 1rem 0;
  border-radius: 0.5rem;
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
</style>
