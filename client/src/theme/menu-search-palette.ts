/**
 * 顶栏菜单搜索框专属色板（集中管理，组件不得散落 hex）。
 *
 * 该色板服务于 MenuSearchBox 的「黑底搜索栏 + 下拉面板」视觉——
 * 是顶栏场景下的固定暖棕墨阶，明暗两套。与全局主题 token 解耦，
 * 属于守卫白名单区域（见 server/scripts/test-frontend-color-tokens.ts）。
 *
 * 用法：组件模板 / :class 中引用对象字段，禁止直接写十六进制。
 */
export const menuSearchPalette = {
  light: {
    barBg: 'bg-white',
    barBorder: 'border-white',
    barFg: 'text-[#161513]',
    barRing: 'ring-[#6e6861]/30',
    iconIdle: 'text-[#8a857e]',
    inputFg: 'text-[#161513]',
    inputCaret: 'caret-black',
    inputPlaceholder: 'placeholder-[#161513]/50',
    kbdBg: 'bg-[#f0eeec]',
    kbdFg: 'text-[#161513]',
    kbdBorder: 'border-[#d8d5d2]',
    panelBg: 'bg-white',
    panelBorder: 'border-[#d8d5d2]',
    resultActiveBg: 'bg-[#262320]',
    resultActiveFg: 'text-white',
    resultIdleFg: 'text-[#8f8a83]',
    resultIdleHover: 'hover:bg-[#1f1c1a]',
    chipBg: 'bg-[#f0eeec]',
    chipFg: 'text-[#161513]',
    metaFg: 'text-[#736e67]',
    emptyFg: 'text-[#736e67]',
    hintFg: 'text-[#5e5a54]',
  },
  dark: {
    barBg: 'bg-black/60',
    barBorder: 'border-[#383531]',
    barFg: 'text-white',
    barRing: '',
    iconIdle: 'text-[#8a857e]',
    inputFg: 'text-white',
    inputCaret: 'caret-white',
    inputPlaceholder: 'placeholder-white/50',
    kbdBg: 'bg-[#1c1a18]',
    kbdFg: 'text-[#9c968e]',
    kbdBorder: 'border-[#33302c]',
    panelBg: 'bg-[#141210]',
    panelBorder: 'border-[#33302d]',
    resultActiveBg: 'bg-[#262320]',
    resultActiveFg: 'text-white',
    resultIdleFg: 'text-[#cfcac2]',
    resultIdleHover: 'hover:bg-[#1b1917]',
    chipBg: 'bg-[#1c1a18]',
    chipFg: 'text-[#9c968e]',
    metaFg: 'text-[#736e67]',
    emptyFg: 'text-[#736e67]',
    hintFg: 'text-[#5e5a54]',
  },
}
