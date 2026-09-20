// 纯黑白墨阶：用于把所有装饰性冷色/粉紫色系收敛为中性灰（仅保留 绿/琥珀/红 作状态色）
const neutralRamp = {
  50: '#fafafa',
  100: '#f2f2f4',
  200: '#e6e6e9',
  300: '#d7d7dc',
  400: '#a3a3a8',
  500: '#737378',
  600: '#52525a',
  700: '#3a3a40',
  800: '#262629',
  900: '#151518',
  950: '#0a0a0a'
}

// OCI 云蓝色阶（Oracle Cloud Infrastructure Console 标志性云蓝）
// 注意：与 client/src/styles/theme.css 的 --accent（浅色 #295BA7 / 深色 #4593DE）同源；
// 主题色管理以 theme.css 唯一来源，此 ramp 仅为 Tailwind 静态色阶兼容层。
const ociBlueRamp = {
  50: '#edf5fc',
  100: '#d6e9f8',
  200: '#b0d4f2',
  300: '#7db6e9',
  400: '#4593de',
  500: '#295BA7',
  600: '#295BA7',
  700: '#1f4a8a',
  800: '#1a3d70',
  900: '#143055',
  950: '#0e1f38'
}

// rose 在代码里被广泛用作「危险/严重/亏损」的红色语义（非装饰），
// 因此保留为真红（与 error=#dc2626 对齐），避免被灰化后危险态静默失色。
const dangerRamp = {
  50: '#fef2f2',
  100: '#fee2e2',
  200: '#fecaca',
  300: '#fca5a5',
  400: '#f87171',
  500: '#ef4444',
  600: '#dc2626',
  700: '#b91c1c',
  800: '#991b1b',
  900: '#7f1d1d',
  950: '#450a0a'
}

/** @type {import('tailwindcss').Config} */
export default {
  content: [
    './index.html',
    './src/**/*.{vue,js,ts,jsx,tsx}'
  ],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // OCI 中性石墨灰阶 - 保留作为文本/边框基底（700=#393632 顶栏色，900=#161513 深石墨底）
        gray: {
          50: '#fafafa',
          100: '#f5f5f4',
          200: '#e5e5e5',
          300: '#d5d4d2',
          400: '#a3a19d',
          500: '#73716d',
          600: '#52504c',
          700: '#393632',
          800: '#262422',
          900: '#161513',
          950: '#0d0c0b'
        },
        // 主强调色 - 已收敛为中性墨阶（纯黑白，保留 sakura 名以兼容既有 class）
        sakura: {
          50: '#fafafa',
          100: '#f2f2f4',
          200: '#e6e6e9',
          300: '#d7d7dc',
          400: '#a3a3a8',
          500: '#737378',
          600: '#52525a',
          700: '#3a3a40',
          800: '#262629',
          900: '#151518'
        },
        // 次强调色 - 同样收敛为中性墨阶
        sky2: {
          50: '#fafafa',
          100: '#f2f2f4',
          200: '#e6e6e9',
          300: '#d7d7dc',
          400: '#a3a3a8',
          500: '#737378',
          600: '#52525a',
          700: '#3a3a40',
          800: '#262629',
          900: '#151518'
        },
        // 点缀色 - 已收敛为中性 slate（去掉紫色装饰感）
        lavender: {
          50: '#f8fafc',
          100: '#f1f5f9',
          200: '#e2e8f0',
          300: '#cbd5e1',
          400: '#94a3b8',
          500: '#64748b',
          600: '#475569',
          700: '#334155',
          800: '#1e293b',
          900: '#0f172a'
        },
        // 暖阳金 - 点缀/VIP色
        sunny: {
          50: '#fff9eb',
          100: '#fff0c2',
          200: '#ffe08a',
          300: '#ffcd4d',
          400: '#ffb81f',
          500: '#f79f05',
          600: '#d67f02',
          700: '#a95f05',
          800: '#894b0d',
          900: '#723e10'
        },
        // 薄荷绿 - 成功/在线
        mint: {
          50: '#eafff6',
          100: '#cbffe8',
          200: '#99ffd4',
          300: '#5cf5ba',
          400: '#2be29e',
          500: '#12c584',
          600: '#08a06c',
          700: '#0a7e58',
          800: '#0c6448',
          900: '#0d523c'
        },
        // 强调色别名 - 墨色（主题感知，浅色黑 / 深色白）
        // primary/secondary 别名用于救活散落的 accent-primary/accent-secondary 死类
        accent: {
          DEFAULT: 'var(--accent)',
          hover: 'var(--accent)',
          light: 'var(--accent)',
          primary: 'var(--accent)',
          secondary: 'var(--accent)'
        },
        // primary 色阶 - 代码里散落 bg-primary-*/text-primary-*/ring-primary-* 作强调用；
        // 主题色管理：值与 theme.css 的 --accent 同步（浅色 #295BA7 / 深色 #4593DE）
        primary: ociBlueRamp,
        // 主题 token 映射（单一来源 theme.css，运行时经 var() 跟随明暗主题切换）
        'bg-primary': 'var(--bg-primary)',
        'bg-surface': 'var(--bg-surface)',
        'bg-surface-soft': 'var(--bg-surface-soft)',
        'border-color': 'var(--border-color)',
        'nav-active': 'var(--nav-active)',
        'topbar-bg': 'var(--topbar-bg)',
        'topbar-text': 'var(--topbar-text)',
        'success-soft': 'var(--success-soft)',
        // 状态色。保留 Tailwind 自带 blue/sky/teal 等语义色，
        // 避免处理中、信息、选中和图表状态被全局灰化后失去区分。
        success: '#16a34a',
        warning: '#d97706',
        error: '#dc2626',
        // rose 保留真红：代码里当「危险/严重/亏损」语义用（详见 dangerRamp 注释）
        rose: dangerRamp
      },
      fontFamily: {
        // Nimbus：Inter(自托管 variable) 为 UI 主字，系统栈 + 中文回退
        sans: [
          '"Inter Variable"',
          'Inter',
          '-apple-system',
          'BlinkMacSystemFont',
          '"SF Pro Text"',
          '"Helvetica Neue"',
          '"PingFang SC"',
          '"Noto Sans SC"',
          '"Microsoft YaHei"',
          'sans-serif'
        ],
        display: [
          '"Inter Variable"',
          'Inter',
          '-apple-system',
          'BlinkMacSystemFont',
          '"SF Pro Display"',
          '"Helvetica Neue"',
          '"PingFang SC"',
          '"Noto Sans SC"',
          'sans-serif'
        ],
        // Nimbus：JetBrains Mono(自托管 variable) 做数据/ID/哈希/终端的等宽声部
        mono: [
          '"JetBrains Mono Variable"',
          '"JetBrains Mono"',
          'ui-monospace',
          '"SF Mono"',
          'Monaco',
          'Consolas',
          'monospace'
        ]
      },
      fontSize: {
        '2xs': ['0.6875rem', { lineHeight: '1rem' }],
      },
      borderRadius: {
        'none': '0px',
        DEFAULT: '4px',
        'sm': '2px',
        'md': '4px',
        'lg': '6px',
        'xl': '8px',
        '2xl': '10px',
        '3xl': '12px',
        // 语义化卡片与组件类型别名 (抽离集中维护，直接与 CSS 变量对齐)
        'card': 'var(--radius-card, 6px)',
        'card-sm': 'var(--radius-card-sm, 4px)',
        'card-lg': 'var(--radius-card-lg, 8px)',
        'modal': 'var(--radius-modal, 8px)',
        'panel': 'var(--radius-panel, 6px)',
        'btn': 'var(--radius-btn, 4px)',
        'input': 'var(--radius-input, 4px)',
        'table': 'var(--radius-table, 4px)',
        'chip': 'var(--radius-chip, 4px)'
      },
      boxShadow: {
        'sm': 'none',
        'DEFAULT': 'none',
        'border': '0 0 0 1px var(--border-color)',
        // OCI 规范：全面消除所有彩色发光与 pop 浮层阴影，保持平整
        'glow-sakura': 'none',
        'glow-sky': 'none',
        'glow-lavender': 'none',
        'glow-sunny': 'none',
        'glow-mint': 'none',
        'pop': '0 4px 12px rgba(0, 0, 0, 0.06)'
      },
      animation: {
        'fade-in': 'fadeIn 0.15s ease-out',
        'slide-up': 'slideUp 0.2s ease-out',
        // 装饰性动效已收敛：pop-in/bounce-in 退化为普通淡入，其余花哨动效禁用
        'pop-in': 'fadeIn 0.2s ease-out',
        'bounce-in': 'fadeIn 0.2s ease-out',
        'wiggle': 'none',
        'float': 'none',
        'float-delay': 'none',
        'sparkle': 'none',
        'heart-beat': 'none',
        'glow-pulse': 'none',
        'shimmer': 'none',
        'petal-fall': 'none'
      },
      keyframes: {
        fadeIn: {
          '0%': { opacity: '0' },
          '100%': { opacity: '1' }
        },
        slideUp: {
          '0%': { opacity: '0', transform: 'translateY(4px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' }
        },
        popIn: {
          '0%': { opacity: '0', transform: 'scale(0.85) translateY(6px)' },
          '100%': { opacity: '1', transform: 'scale(1) translateY(0)' }
        },
        bounceIn: {
          '0%': { opacity: '0', transform: 'scale(0.5)' },
          '60%': { opacity: '1', transform: 'scale(1.05)' },
          '100%': { opacity: '1', transform: 'scale(1)' }
        },
        wiggle: {
          '0%, 100%': { transform: 'rotate(0deg)' },
          '25%': { transform: 'rotate(-6deg)' },
          '75%': { transform: 'rotate(6deg)' }
        },
        float: {
          '0%, 100%': { transform: 'translateY(0) rotate(0deg)' },
          '50%': { transform: 'translateY(-8px) rotate(3deg)' }
        },
        sparkle: {
          '0%, 100%': { opacity: '0.3', transform: 'scale(0.8) rotate(0deg)' },
          '50%': { opacity: '1', transform: 'scale(1.15) rotate(90deg)' }
        },
        heartBeat: {
          '0%, 100%': { transform: 'scale(1)' },
          '14%': { transform: 'scale(1.2)' },
          '28%': { transform: 'scale(1)' },
          '42%': { transform: 'scale(1.15)' },
          '70%': { transform: 'scale(1)' }
        },
        glowPulse: {
          '0%, 100%': { boxShadow: '0 0 0 0 rgb(255 79 156 / 0.4)' },
          '50%': { boxShadow: '0 0 0 6px rgb(255 79 156 / 0)' }
        },
        shimmer: {
          '0%': { backgroundPosition: '-200% 0' },
          '100%': { backgroundPosition: '200% 0' }
        },
        petalFall: {
          '0%': { transform: 'translateY(-10%) rotate(0deg)', opacity: '0' },
          '10%': { opacity: '0.8' },
          '100%': { transform: 'translateY(110vh) rotate(360deg)', opacity: '0' }
        }
      }
    }
  },
  plugins: [
    require('@tailwindcss/forms')({ strategy: 'class' })
  ]
}
