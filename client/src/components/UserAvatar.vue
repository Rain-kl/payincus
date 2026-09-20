<script setup lang="ts">
import { computed, ref, watch, onMounted } from 'vue'
import { useConfigStore } from '@/stores/config'
import { useAuthStore } from '@/stores/auth'
import BadgeImage from '@/components/BadgeImage.vue'

const props = withDefaults(defineProps<{
  username: string
  email?: string | null
  avatarStyle?: string
  badgeId?: string | null
  preferBadge?: boolean
  size?: number
}>(), {
  email: null,
  avatarStyle: '',
  badgeId: null,
  preferBadge: true,
  size: 32
})

const configStore = useConfigStore()
const authStore = useAuthStore()

// 确保配置已加载
onMounted(() => {
  configStore.loadPublicConfig()
})

// 风格名称映射：camelCase -> kebab-case (DiceBear API 格式)
const styleNameMap: Record<string, string> = {
  adventurer: 'adventurer',
  adventurerNeutral: 'adventurer-neutral',
  avataaars: 'avataaars',
  avataaarsNeutral: 'avataaars-neutral',
  bigEars: 'big-ears',
  bigEarsNeutral: 'big-ears-neutral',
  bigSmile: 'big-smile',
  bottts: 'bottts',
  botttsNeutral: 'bottts-neutral',
  croodles: 'croodles',
  croodlesNeutral: 'croodles-neutral',
  dylan: 'dylan',
  funEmoji: 'fun-emoji',
  glass: 'glass',
  icons: 'icons',
  identicon: 'identicon',
  initials: 'initials',
  lorelei: 'lorelei',
  loreleiNeutral: 'lorelei-neutral',
  micah: 'micah',
  miniavs: 'miniavs',
  notionists: 'notionists',
  notionistsNeutral: 'notionists-neutral',
  openPeeps: 'open-peeps',
  personas: 'personas',
  pixelArt: 'pixel-art',
  pixelArtNeutral: 'pixel-art-neutral',
  rings: 'rings',
  shapes: 'shapes',
  thumbs: 'thumbs'
}

// 预设高辨识度与优雅质感背景色盘
const AVATAR_BG_PALETTE = [
  'bg-blue-600 text-white',
  'bg-indigo-600 text-white',
  'bg-emerald-600 text-white',
  'bg-amber-600 text-white',
  'bg-rose-600 text-white',
  'bg-cyan-600 text-white',
  'bg-violet-600 text-white',
  'bg-teal-600 text-white',
  'bg-slate-700 text-white'
]

function getAvatarBgClass(name: string): string {
  if (!name) return 'bg-zinc-700 text-white'
  let hash = 0
  for (let i = 0; i < name.length; i++) {
    hash = ((hash << 5) - hash) + name.charCodeAt(i)
    hash |= 0
  }
  const index = Math.abs(hash) % AVATAR_BG_PALETTE.length
  return AVATAR_BG_PALETTE[index]
}

const avatarBgClass = computed(() => getAvatarBgClass(props.username))

const displayInitial = computed(() => {
  const name = (props.username || '').trim()
  if (!name) return '?'
  return name.charAt(0).toUpperCase()
})

const DEFAULT_AVATAR_SRC = '/images/user-icon.png'

const customImgFailed = ref(false)
const defaultImgFailed = ref(false)

watch(() => [props.avatarStyle, props.username, props.email], () => {
  customImgFailed.value = false
  defaultImgFailed.value = false
})

function onImgError() {
  if (customAvatarUrl.value && !customImgFailed.value) {
    customImgFailed.value = true
  } else {
    defaultImgFailed.value = true
  }
}

const customAvatarUrl = computed(() => {
  if (!props.username) return ''
  
  // 未设置头像风格或风格无效时，返回空字符串
  const styleKey = props.avatarStyle?.trim()
  if (!styleKey || !styleNameMap[styleKey]) {
    return ''
  }
  
  const styleName = styleNameMap[styleKey]
  const seed = encodeURIComponent(props.email || props.username)
  const apiBase = configStore.avatarApiBase
  return `${apiBase}/${styleName}/svg?seed=${seed}&size=${props.size}`
})

const currentAvatarUrl = computed(() => {
  if (customAvatarUrl.value && !customImgFailed.value) {
    return customAvatarUrl.value
  }
  return DEFAULT_AVATAR_SRC
})

const effectiveBadgeId = computed(() => {
  if (!props.preferBadge) return null
  if (props.badgeId) return props.badgeId
  if (authStore.user?.username === props.username) {
    return authStore.user.avatarBadgeId || null
  }
  return null
})

const sizeStyle = computed(() => {
  const s = props.size || 32
  return {
    width: `${s}px`,
    height: `${s}px`,
    fontSize: `${Math.max(11, Math.round(s * 0.44))}px`
  }
})
</script>

<template>
  <BadgeImage
    v-if="effectiveBadgeId"
    :badge-id="effectiveBadgeId"
    :alt="username"
    :size="size"
    variant="avatar"
  />
  <img 
    v-else-if="!defaultImgFailed"
    :src="currentAvatarUrl" 
    :alt="username"
    class="rounded-full object-cover shrink-0 select-none"
    :style="sizeStyle"
    loading="lazy"
    @error="onImgError"
  />
  <div 
    v-else
    class="rounded-full flex items-center justify-center font-semibold shrink-0 select-none leading-none shadow-sm"
    :class="avatarBgClass"
    :style="sizeStyle"
  >
    {{ displayInitial }}
  </div>
</template>
