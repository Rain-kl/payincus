<script setup lang="ts">
import { ref, watch, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useThemeStore } from '@/stores/theme'
import { parseMarkdown } from '@/utils/markdown'
import { getLocale } from '@/locales'

const props = defineProps<{
  show: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const { t } = useI18n()
const themeStore = useThemeStore()

const content = ref('')
const loading = ref(false)
const error = ref('')

async function loadTerms() {
  loading.value = true
  error.value = ''
  
  try {
    const locale = getLocale()
    const lang = locale.startsWith('zh') ? 'zh' : 'en'
    const response = await fetch(`/tos/${lang}.md`)
    
    if (!response.ok) {
      throw new Error('Failed to load terms')
    }
    
    const markdown = await response.text()
    content.value = parseMarkdown(markdown)
  } catch (err) {
    error.value = t('auth.tos.loadFailed')
    console.error('Failed to load terms:', err)
  } finally {
    loading.value = false
  }
}

watch(() => props.show, (newVal) => {
  if (newVal && !content.value) {
    loadTerms()
  }
})

onMounted(() => {
  if (props.show) {
    loadTerms()
  }
})
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="show"
        class="modal-overlay"
        @click.self="emit('close')"
      >
        <!-- Backdrop -->
        <div 
          class="modal-backdrop"
          @click="emit('close')"
        />
        
        <!-- Modal -->
        <div
          class="modal-content max-w-3xl md:max-w-4xl lg:max-w-5xl"
        >
          <!-- Header -->
          <div 
            class="modal-header"
          >
            <h2 
              class="modal-title"
            >
              {{ $t('auth.tos.title') }}
            </h2>
            <button
              class="p-1.5 rounded-lg transition-colors"
              :class="themeStore.isDark 
                ? 'text-gray-400 hover:text-gray-200 hover:bg-gray-800' 
                : 'text-gray-500 hover:text-gray-700 hover:bg-gray-100'"
              @click="emit('close')"
            >
              <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
              </svg>
            </button>
          </div>
          
          <!-- Content -->
          <div class="modal-body">
            <!-- Loading -->
            <div v-if="loading" class="flex items-center justify-center py-12">
              <svg class="animate-spin h-8 w-8" :class="themeStore.isDark ? 'text-gray-400' : 'text-gray-500'" fill="none" viewBox="0 0 24 24">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
              </svg>
            </div>
            
            <!-- Error -->
            <div v-else-if="error" class="text-center py-12">
              <p :class="themeStore.isDark ? 'text-red-400' : 'text-red-600'" class="mb-4">{{ error }}</p>
              <button 
                class="btn-secondary"
                @click="loadTerms"
              >
                {{ $t('common.refresh') }}
              </button>
            </div>
            
            <!-- Content -->
            <div 
              v-else 
              class="markdown-body text-sm"
              :class="themeStore.isDark ? 'text-gray-300' : 'text-gray-700'"
              v-html="content"
            />
          </div>
          
          <!-- Footer -->
          <div 
            class="modal-footer"
          >
            <button
              class="btn-primary w-full"
              @click="emit('close')"
            >
              {{ $t('auth.tos.understood') }}
            </button>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.modal-enter-active,
.modal-leave-active {
  transition: opacity 0.25s cubic-bezier(0.16, 1, 0.3, 1);
}

.modal-enter-from,
.modal-leave-to {
  opacity: 0;
}

.modal-enter-active > div:last-child,
.modal-leave-active > div:last-child {
  transition: transform 0.25s cubic-bezier(0.16, 1, 0.3, 1);
}

.modal-enter-from > div:last-child,
.modal-leave-to > div:last-child {
  transform: translateX(100%);
}
</style>

<style>
/* Markdown 样式 - 服务条款弹窗 */
.markdown-body {
  line-height: 1.7;
}

.markdown-body > *:first-child {
  margin-top: 0 !important;
}

.markdown-body > *:last-child {
  margin-bottom: 0 !important;
}

/* 标题 - 暗色主题 */
.dark .markdown-body h1,
.dark .markdown-body h2,
.dark .markdown-body h3,
.dark .markdown-body h4,
.dark .markdown-body h5,
.dark .markdown-body h6 {
  color: var(--text-primary);
  margin-top: 1.5em;
  margin-bottom: 0.5em;
  font-weight: 600;
  line-height: 1.3;
}

/* 标题 - 亮色主题 */
.light .markdown-body h1,
.light .markdown-body h2,
.light .markdown-body h3,
.light .markdown-body h4,
.light .markdown-body h5,
.light .markdown-body h6 {
  color: var(--text-primary);
  margin-top: 1.5em;
  margin-bottom: 0.5em;
  font-weight: 600;
  line-height: 1.3;
}

.markdown-body h1 { font-size: 1.75em; }
.markdown-body h2 { font-size: 1.5em; padding-bottom: 0.3em; }
.markdown-body h3 { font-size: 1.25em; }
.markdown-body h4 { font-size: 1.1em; }
.markdown-body h5 { font-size: 1em; }
.markdown-body h6 { font-size: 0.9em; }

.dark .markdown-body h2 { border-bottom: 1px solid var(--border-color); }
.light .markdown-body h2 { border-bottom: 1px solid var(--border-color); }
.dark .markdown-body h6 { color: var(--text-secondary); }
.light .markdown-body h6 { color: var(--text-secondary); }

/* 段落 */
.markdown-body p {
  margin: 1em 0;
}

.dark .markdown-body p { color: var(--text-secondary); }
.light .markdown-body p { color: var(--text-secondary); }

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

.dark .markdown-body li { color: var(--text-secondary); }
.light .markdown-body li { color: var(--text-secondary); }

/* 强调 */
.markdown-body strong {
  font-weight: 600;
}

.dark .markdown-body strong { color: var(--text-primary); }
.light .markdown-body strong { color: var(--text-primary); }

.markdown-body em {
  font-style: italic;
}

/* 链接 */
.dark .markdown-body a { color: var(--accent); }
.light .markdown-body a { color: var(--accent); }

.markdown-body a {
  text-decoration: none;
}

.markdown-body a:hover {
  text-decoration: underline;
}
</style>
