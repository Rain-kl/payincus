import { ref, watch, type Ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

export interface UseTabQueryOptions<T extends string> {
  /** 合法 tab 取值，用于校验 URL 上的值 */
  keys: readonly T[]
  /** 默认 tab：URL 上无该参数或参数非法时回退到它，且等于它时不写入 URL */
  fallback: T
  /** URL query 参数名，默认 tab */
  queryKey?: string
  /** tab 因外部导航（浏览器前进/后退、手改地址栏）变化时触发，便于按需懒加载该 tab 的数据 */
  onExternalChange?: (tab: T) => void
}

/**
 * 把页面内的 tab 选中态同步到 URL query（默认 `?tab=`），
 * 使刷新、前进后退、分享链接都能回到同一个 tab，而不是回到第一个 tab。
 */
export function useTabQuery<T extends string>(options: UseTabQueryOptions<T>): Ref<T> {
  const route = useRoute()
  const router = useRouter()
  const queryKey = options.queryKey ?? 'tab'
  const keys: readonly string[] = options.keys

  const normalize = (value: unknown): T =>
    typeof value === 'string' && keys.includes(value) ? (value as T) : options.fallback

  const tab = ref<T>(normalize(route.query[queryKey])) as Ref<T>

  // tab 变化 → 写回 URL；等于默认值时不写入，保持地址干净
  watch(tab, (value) => {
    if (normalize(route.query[queryKey]) === value) return
    const query = { ...route.query }
    if (value === options.fallback) delete query[queryKey]
    else query[queryKey] = value
    void router.replace({ path: route.path, query })
  })

  // URL 变化（前进/后退、手改地址栏）→ 回填 tab；
  // 本组件自身写入引起的回填会被相等判断拦截，不会重复触发外部回调
  watch(
    () => route.query[queryKey],
    (value) => {
      const next = normalize(value)
      if (next === tab.value) return
      tab.value = next
      options.onExternalChange?.(next)
    }
  )

  return tab
}
