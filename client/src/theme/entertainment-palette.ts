/**
 * 娱乐中心专属色板（集中管理，组件不得散落 hex）。
 *
 * 该色板服务于幸运大转盘、奖品类型视觉标识以及签到礼盒粒子效果。
 * 集中收敛至 theme/ 目录下，符合项目统一主题规矩。
 */

export const confettiPalette = [
  '#ff6b6b',
  '#4ecdc4',
  '#45b7d1',
  '#96ceb4',
  '#feca57',
  '#ff9ff3',
  '#54a0ff',
  '#5f27cd'
]

export const prizeColorPalette: Record<string, string> = {
  nothing: '#9CA3AF',
  points: '#F59E0B',
  balance: '#10B981',
  badge: '#0EA5E9',
  instance: '#8B5CF6',
  cpu: '#3B82F6',     // 蓝色 - CPU
  memory: '#EC4899',  // 粉色 - 内存
  disk: '#F97316',    // 橙色 - 硬盘
  traffic: '#06B6D4'  // 青色 - 流量
}

export const defaultPrizeColor = '#6B7280'

export const wheelGradientPalette: Record<string, [string, string]> = {
  nothing: ['#9CA3AF', '#6B7280'],      // 灰色系
  points: ['#FBBF24', '#F59E0B'],       // 金色系
  balance: ['#34D399', '#10B981'],      // 绿色系
  badge: ['#38BDF8', '#0EA5E9'],        // 天蓝色系
  instance: ['#A78BFA', '#8B5CF6'],     // 紫色系
  cpu: ['#60A5FA', '#3B82F6'],          // 蓝色系 - CPU
  memory: ['#F472B6', '#EC4899'],       // 粉色系 - 内存
  disk: ['#FB923C', '#F97316'],         // 橙色系 - 硬盘
  traffic: ['#22D3EE', '#06B6D4']       // 青色系 - 流量
}

export const defaultWheelGradient: [string, string] = ['#9CA3AF', '#6B7280']
