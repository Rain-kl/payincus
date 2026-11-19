import { readFileSync } from 'node:fs'

/**
 * 速率限制配置
 * 
 * 配置说明:
 * - max: 时间窗口内最大请求数
 * - timeWindow: 时间窗口 (支持 '1 minute', '1 hour', '1 second' 等)
 * - description: 接口说明 (仅用于文档)
 * 
 * 调整建议:
 * - 登录/注册: 较严格，防止暴力破解
 * - 读取操作: 可以适当放宽
 * - 写入操作: 适中限制
 * - 敏感操作: 严格限制
 */

export interface RateLimitRule {
    /** 路由路径 (支持通配符) */
    path: string
    /** HTTP 方法 */
    method: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH' | '*'
    /** 时间窗口内最大请求数 */
    max: number
    /** 时间窗口 */
    timeWindow: string
    /** 接口说明 */
    description: string
}

// ==================== 全局默认配置 ====================

export const globalRateLimit = {
    /** 全局默认: 每分钟最大请求数 */
    max: 300,
    /** 全局默认: 时间窗口 */
    timeWindow: '1 minute',
    /** 描述 */
    description: '全局默认限制'
}

// ==================== 接口级别配置 ====================

export const rateLimitRules: RateLimitRule[] = [
    // ========== 认证相关 (严格限制) ==========
    {
        path: '/api/auth/check-2fa',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '检查2FA状态 - 防止用户名枚举'
    },
    {
        path: '/api/auth/login',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '登录接口 - 防止暴力破解'
    },
    {
        path: '/api/auth/register',
        method: 'POST',
        max: 3,
        timeWindow: '1 minute',
        description: '注册接口 - 防止批量注册'
    },
    {
        path: '/api/auth/refresh',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '刷新令牌 - 适中限制'
    },
    {
        path: '/api/auth/2fa/*',
        method: '*',
        max: 10,
        timeWindow: '1 minute',
        description: '2FA 相关操作'
    },
    {
        path: '/api/auth/forgot-password/send-code',
        method: 'POST',
        max: 5,
        timeWindow: '1 hour',
        description: '发送找回密码验证码 - 防止滥用'
    },
    {
        path: '/api/auth/forgot-password/reset',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '重置密码 - 防止暴力破解验证码'
    },

    // ========== 扩展中心 (高风险后台操作) ==========
    {
        path: '/api/instances',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '创建实例 - 防止滥用资源'
    },
    {
        path: '/api/instances',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取实例列表'
    },
    {
        path: '/api/instances/*/start',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '启动实例'
    },
    {
        path: '/api/instances/*/stop',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '停止实例'
    },
    {
        path: '/api/instances/*/restart',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '重启实例'
    },
    {
        path: '/api/instances/*/reinstall',
        method: 'POST',
        max: 2,
        timeWindow: '5 minutes',
        description: '重装实例 - 严格限制'
    },

    // ========== 快照/备份 (严格限制) ==========
    {
        path: '/api/instances/*/snapshots',
        method: 'POST',
        max: 5,
        timeWindow: '5 minutes',
        description: '创建快照 - 资源密集操作'
    },
    {
        path: '/api/instances/*/snapshots',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取快照列表'
    },
    {
        path: '/api/instances/*/backups',
        method: 'POST',
        max: 3,
        timeWindow: '10 minutes',
        description: '创建备份 - 资源密集操作'
    },
    {
        path: '/api/instances/*/backups',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取备份列表'
    },
    {
        path: '/api/instances/*/backups/*/export',
        method: 'POST',
        max: 5,
        timeWindow: '10 minutes',
        description: '创建备份导出任务 - 资源密集操作'
    },
    {
        path: '/api/instances/*/backups/export/*/download-token',
        method: 'POST',
        max: 10,
        timeWindow: '10 minutes',
        description: '获取备份下载令牌'
    },
    {
        path: '/api/instances/*/backups/export/*/download',
        method: 'GET',
        max: 5,
        timeWindow: '10 minutes',
        description: '下载备份 - 带宽密集操作'
    },

    // ========== 端口映射 ==========
    {
        path: '/api/instances/*/ports',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '添加端口映射'
    },
    {
        path: '/api/instances/*/ports',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取端口映射列表'
    },

    // ========== 反代站点 ==========
    {
        path: '/api/instances/*/sites',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取站点列表'
    },
    {
        path: '/api/instances/*/sites',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '添加反代站点'
    },
    {
        path: '/api/instances/*/sites/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '删除反代站点'
    },
    {
        path: '/api/instances/*/sites/*/refresh',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '刷新站点配置'
    },

    // ========== 用户管理 ==========
    {
        path: '/api/users',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取用户列表 (管理员)'
    },
    {
        path: '/api/users/*',
        method: 'PATCH',
        max: 10,
        timeWindow: '1 minute',
        description: '更新用户信息'
    },
    {
        path: '/api/users/*/change-email/send-code',
        method: 'POST',
        max: 5,
        timeWindow: '10 minutes',
        description: '发送修改邮箱验证码'
    },

    // ========== 主机/节点管理 (管理员) ==========
    {
        path: '/api/hosts',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取主机列表'
    },
    {
        path: '/api/hosts',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '添加主机'
    },
    {
        path: '/api/node-groups',
        method: '*',
        max: 30,
        timeWindow: '1 minute',
        description: '节点组管理'
    },

    // ========== 套餐管理 ==========
    {
        path: '/api/packages',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取套餐列表'
    },
    {
        path: '/api/packages',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '创建套餐 (管理员)'
    },
    {
        path: '/api/packages/*/hosts-detail',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取套餐宿主机详情'
    },
    {
        path: '/api/packages/*/release-channel',
        method: 'PUT',
        max: 10,
        timeWindow: '1 minute',
        description: '设置释放通知渠道'
    },
    {
        path: '/api/packages/*/release-quota',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '释放配额 - 敏感操作'
    },

    // ========== SSH 密钥 ==========
    {
        path: '/api/ssh-keys',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取 SSH 密钥列表'
    },
    {
        path: '/api/ssh-keys',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '添加 SSH 密钥'
    },
    {
        path: '/api/ssh-keys/generate',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '生成 SSH 密钥对 - CPU密集操作'
    },

    // ========== 通知 ==========
    {
        path: '/api/notifications',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取通知列表'
    },
    {
        path: '/api/notifications/channels',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '添加通知渠道'
    },

    // ========== 站内信 ==========
    {
        path: '/api/inbox',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取站内信列表'
    },
    {
        path: '/api/inbox/unread-count',
        method: 'GET',
        max: 120,
        timeWindow: '1 minute',
        description: '获取未读数量 - 轮询场景'
    },
    {
        path: '/api/inbox/*/read',
        method: 'POST',
        max: 60,
        timeWindow: '1 minute',
        description: '标记消息已读'
    },
    {
        path: '/api/inbox/read-all',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '全部标记已读'
    },
    {
        path: '/api/inbox/*',
        method: 'DELETE',
        max: 30,
        timeWindow: '1 minute',
        description: '删除消息'
    },
    {
        path: '/api/inbox/read',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '清空已读消息'
    },
    {
        path: '/api/inbox/admin/send/*',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '管理员发送站内信给特定用户'
    },
    {
        path: '/api/inbox/admin/broadcast',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '管理员全站广播'
    },
    {
        path: '/api/inbox/instances/*/notify',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '宿主机所有者发送站内信给实例用户'
    },
    {
        path: '/api/inbox/hosts/*/notify',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '宿主机所有者通知节点实例用户'
    },

    // ========== 镜像 ==========
    {
        path: '/api/images',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取镜像列表'
    },
    {
        path: '/api/images/sync',
        method: 'POST',
        max: 2,
        timeWindow: '5 minutes',
        description: '同步镜像 - 资源密集操作'
    },

    // ========== 日志 ==========
    {
        path: '/api/logs',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取日志列表'
    },

    // ========== 帮助文档 ==========
    {
        path: '/api/help',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取帮助文档'
    },

    // ========== OAuth ==========
    {
        path: '/api/oauth/*',
        method: '*',
        max: 10,
        timeWindow: '1 minute',
        description: 'OAuth 认证'
    },

    // ========== 邀请码 ==========
    {
        path: '/api/auth/invite',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '生成邀请码 (管理员)'
    },
    {
        path: '/api/auth/invites',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取邀请码列表'
    },
    {
        path: '/api/auth/user/invite',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '生成邀请码 (用户)'
    },
    {
        path: '/api/auth/user/invites',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '获取用户邀请码列表'
    },
    {
        path: '/api/auth/user/invites/*',
        method: 'DELETE',
        max: 20,
        timeWindow: '1 minute',
        description: '删除用户邀请码'
    },

    // ========== 工单系统 ==========
    {
        path: '/api/tickets',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '创建工单 - 防止滥发'
    },

    // ========== 自定义初始化命令 ==========
    {
        path: '/api/init-commands',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取初始化命令列表'
    },
    {
        path: '/api/init-commands/available',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取可用初始化命令列表'
    },
    {
        path: '/api/init-commands/distros',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取发行版列表'
    },
    {
        path: '/api/init-commands',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '创建初始化命令 - 防止滥用'
    },
    {
        path: '/api/init-commands/*',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取单个初始化命令详情'
    },
    {
        path: '/api/init-commands/*',
        method: 'PUT',
        max: 20,
        timeWindow: '1 minute',
        description: '更新初始化命令'
    },
    {
        path: '/api/init-commands/*',
        method: 'DELETE',
        max: 20,
        timeWindow: '1 minute',
        description: '删除初始化命令'
    },
    {
        path: '/api/tickets',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取工单列表'
    },
    {
        path: '/api/tickets/pending-count',
        method: 'GET',
        max: 120,
        timeWindow: '1 minute',
        description: '获取待处理工单数量 - 轮询场景'
    },
    {
        path: '/api/tickets/my-hosts',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取我的宿主机工单列表'
    },
    {
        path: '/api/tickets/hosts/*',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取指定宿主机工单列表'
    },
    {
        path: '/api/tickets/attachments/*/content',
        method: 'GET',
        max: 240,
        timeWindow: '1 minute',
        description: '读取工单图片内容'
    },
    {
        path: '/api/tickets/*',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取工单详情'
    },
    {
        path: '/api/tickets/*/messages',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '获取工单消息列表'
    },
    {
        path: '/api/tickets/*/messages',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '回复工单 - 防止刷屏'
    },
    {
        path: '/api/tickets/*/status',
        method: 'PATCH',
        max: 20,
        timeWindow: '1 minute',
        description: '更新工单状态'
    },
    {
        path: '/api/tickets/*/close',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '关闭工单'
    },

    // ========== 从路由文件内联声明迁移的规则 ==========
    // 2026-11-19 限流整改：原 89 处 `config: { rateLimit: ... }` 内联声明全部收敛至此
    // （单一事实源）。修复 app.ts 的 hook 顺序后，config 规则与内联声明同层生效，
    // 两者并存会互相覆盖，必须收敛为一个源。路由文件已同步删除内联声明。
    {
        path: '/api/admin/instances/*/suspend',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:1490'
    },
    {
        path: '/api/admin/instances/*/unsuspend',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:1562'
    },
    {
        path: '/api/admin/instances/*/extend',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:1616'
    },
    {
        path: '/api/admin/instances/*/refund',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:1816'
    },
    {
        path: '/api/admin/instances/*/delete-and-refund',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:1954'
    },
    {
        path: '/api/admin/instances/*/apply-aff',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:2222'
    },
    {
        path: '/api/admin/instances/*/update-price',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:2317'
    },
    {
        path: '/api/admin/instances/*/update-price/preview',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:2564'
    },
    {
        path: '/api/admin/instances/batch-update-price/preview',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:2630'
    },
    {
        path: '/api/admin/instances/batch-update-price',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:2651'
    },
    {
        path: '/api/admin/billing/recharge-records/*/sync',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:3101'
    },
    {
        path: '/api/admin/instances/create',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:3427'
    },
    {
        path: '/api/admin/instances/*/upgrade-plan',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-billing.ts:4294'
    },
    {
        path: '/api/admin/statistics/overview',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] admin-statistics.ts:145'
    },
    {
        path: '/api/admin/tasks/*/stop',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] admin-tasks.ts:29'
    },
    {
        path: '/api/admin/tasks/*/logs/stream',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] admin-tasks.ts:45'
    },
    {
        path: '/api/aff/me/codes',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:147'
    },
    {
        path: '/api/aff/me/codes/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:196'
    },
    {
        path: '/api/aff/leaderboard',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:273'
    },
    {
        path: '/api/aff/validate',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:283'
    },
    {
        path: '/api/aff/me/convert',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:355'
    },
    {
        path: '/api/aff/admin/withdrawals/*/approve',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:484'
    },
    {
        path: '/api/aff/admin/withdrawals/*/reject',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] aff.ts:530'
    },
    {
        path: '/api/balance/admin/*/adjustment-requests',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] balance.ts:478'
    },
    {
        path: '/api/balance/admin/adjustment-requests/*/approve',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] balance.ts:538'
    },
    {
        path: '/api/balance/admin/adjustment-requests/*/reject',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] balance.ts:593'
    },
    {
        path: '/api/balance/admin/*/adjust',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] balance.ts:632'
    },
    {
        path: '/api/balance/admin/*/gift',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] balance.ts:710'
    },
    {
        path: '/api/gift-cards/admin/generate',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:140'
    },
    {
        path: '/api/gift-cards/admin/batch',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:209'
    },
    {
        path: '/api/gift-cards/admin/list',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:293'
    },
    {
        path: '/api/gift-cards/admin/*/enable',
        method: 'PATCH',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:327'
    },
    {
        path: '/api/gift-cards/admin/*/disable',
        method: 'PATCH',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:340'
    },
    {
        path: '/api/gift-cards/admin/*',
        method: 'DELETE',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:353'
    },
    {
        path: '/api/gift-cards/admin/batch-disable',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:366'
    },
    {
        path: '/api/gift-cards/admin/batch-delete',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:389'
    },
    {
        path: '/api/gift-cards/user/redeem',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:414'
    },
    {
        path: '/api/gift-cards/user/generate',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:462'
    },
    {
        path: '/api/gift-cards/user/*/revoke',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:516'
    },
    {
        path: '/api/gift-cards/user/mine',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] gift-cards.ts:537'
    },
    {
        path: '/api/hosts/*/instances/create-for-user',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] hosts.ts:5449'
    },
    {
        path: '/api/instances/*/apply-aff',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:366'
    },
    {
        path: '/api/instances/*/renew',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:394'
    },
    {
        path: '/api/instances/batch/renew-preview',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:489'
    },
    {
        path: '/api/instances/batch/renew',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:519'
    },
    {
        path: '/api/instances/*/change-plan',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:702'
    },
    {
        path: '/api/instances/*/auto-renew',
        method: 'PATCH',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:873'
    },
    {
        path: '/api/instances/batch/auto-renew',
        method: 'PATCH',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] instance-billing.ts:932'
    },
    {
        path: '/api/instances/batch/destroy-info',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] instance-destroy.ts:854'
    },
    {
        path: '/api/instances/batch/destroy',
        method: 'POST',
        max: 3,
        timeWindow: '1 minute',
        description: '[inline] instance-destroy.ts:884'
    },
    {
        path: '/api/instances/*/destroy-info',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] instance-destroy.ts:944'
    },
    {
        path: '/api/instances/*/destroy',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] instance-destroy.ts:1082'
    },
    {
        path: '/api/instances/*/swap/enable',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] instances.ts:5624'
    },
    {
        path: '/api/instances/*/swap/disable',
        method: 'POST',
        max: 5,
        timeWindow: '1 minute',
        description: '[inline] instances.ts:5732'
    },
    {
        path: '/api/admin/orders/*/*/operation',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] orders.ts:680'
    },
    {
        path: '/api/packages/public',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:798'
    },
    {
        path: '/api/packages/public/regions',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:944'
    },
    {
        path: '/api/packages/',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:1458'
    },
    {
        path: '/api/packages/*',
        method: 'PATCH',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:1658'
    },
    {
        path: '/api/packages/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:1942'
    },
    {
        path: '/api/packages/*/release-quota',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:2180'
    },
    {
        path: '/api/packages/*/plans',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:2383'
    },
    {
        path: '/api/packages/*/plans/*',
        method: 'PUT',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:2506'
    },
    {
        path: '/api/packages/*/plans/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] packages.ts:2664'
    },
    {
        path: '/api/recharge/orders',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:985'
    },
    {
        path: '/api/recharge/orders/*/cancel',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:1234'
    },
    {
        path: '/api/recharge/orders/*/repay',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:1266'
    },
    {
        path: '/api/recharge/orders/*/verify',
        method: 'POST',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:1463'
    },
    {
        path: '/api/admin/payment-providers',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2175'
    },
    {
        path: '/api/admin/payment-providers/*',
        method: 'PUT',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2210'
    },
    {
        path: '/api/admin/payment-providers/*/status',
        method: 'PATCH',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2266'
    },
    {
        path: '/api/admin/payment-providers/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2317'
    },
    {
        path: '/api/admin/recharge/orders/*/complete',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2404'
    },
    {
        path: '/api/admin/recharge/orders/*/fail',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2466'
    },
    {
        path: '/api/recharge/callback/*',
        method: 'POST',
        max: 100,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:2997'
    },
    {
        path: '/api/recharge/callback/*',
        method: 'GET',
        max: 100,
        timeWindow: '1 minute',
        description: '[inline] recharge.ts:3014'
    },
    {
        path: '/api/ws/instances/*/spice-session',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] spice.ts:40'
    },
    {
        path: '/api/system-config/public',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] system-config.ts:125'
    },
    {
        path: '/api/user-invites/',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] user-invites.ts:165'
    },
    {
        path: '/api/vip-benefits/me',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] vip-benefits.ts:33'
    },
    {
        path: '/api/vip-benefits/*/claim',
        method: 'POST',
        max: 100,
        timeWindow: '3 minutes',
        description: '[inline] vip-benefits.ts:40'
    },
    {
        path: '/api/vip-benefits/claim-all',
        method: 'POST',
        max: 30,
        timeWindow: '3 minutes',
        description: '[inline] vip-benefits.ts:58'
    },
    {
        path: '/api/admin/vip-benefits/rewards',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] vip-benefits.ts:69'
    },
    {
        path: '/api/admin/vip-benefits/rewards',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] vip-benefits.ts:76'
    },
    {
        path: '/api/admin/vip-benefits/rewards/*',
        method: 'PUT',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] vip-benefits.ts:89'
    },
    {
        path: '/api/admin/vip-benefits/rewards/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: '[inline] vip-benefits.ts:108'
    },
    {
        path: '/api/vip-levels/me',
        method: 'GET',
        max: 60,
        timeWindow: '1 minute',
        description: '[inline] vip-levels.ts:24'
    },
    {
        path: '/api/admin/vip-levels/*',
        method: 'GET',
        max: 30,
        timeWindow: '1 minute',
        description: '[inline] vip-levels.ts:31'
    },
    {
        path: '/api/admin/vip-levels/*',
        method: 'PUT',
        max: 20,
        timeWindow: '1 minute',
        description: '[inline] vip-levels.ts:48'
    },

    // Public API（pat_ token）的九条限额，原为 public-api.ts 的
    // PUBLIC_API_RATE_LIMITS 常量展开的内联声明，一并收敛至此。
    {
        path: '/api/v1/balance/adjustment-requests',
        method: 'POST',
        max: 5,
        timeWindow: '10 minutes',
        description: 'Public API /api/v1/balance/adjustment-requests'
    },
    {
        path: '/api/v1/services/*/actions',
        method: 'POST',
        max: 10,
        timeWindow: '1 minute',
        description: 'Public API /api/v1/services/*/actions'
    },
    {
        path: '/api/v1/services/*/renew',
        method: 'POST',
        max: 5,
        timeWindow: '10 minutes',
        description: 'Public API /api/v1/services/*/renew'
    },
    {
        path: '/api/v1/services/*/tasks/*',
        method: 'GET',
        max: 120,
        timeWindow: '1 minute',
        description: 'Public API /api/v1/services/*/tasks/*'
    },
    {
        path: '/api/v1/services/*/tasks/*',
        method: 'DELETE',
        max: 10,
        timeWindow: '1 minute',
        description: 'Public API /api/v1/services/*/tasks/*'
    },
    {
        path: '/api/v1/tickets',
        method: 'POST',
        max: 10,
        timeWindow: '5 minutes',
        description: 'Public API /api/v1/tickets'
    },
    {
        path: '/api/v1/tickets/*/status',
        method: 'PATCH',
        max: 20,
        timeWindow: '1 minute',
        description: 'Public API /api/v1/tickets/*/status'
    },
    {
        path: '/api/v1/tickets/*/replies',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: 'Public API /api/v1/tickets/*/replies'
    },
    {
        path: '/api/v1/notifications',
        method: 'POST',
        max: 20,
        timeWindow: '1 minute',
        description: 'Public API /api/v1/notifications'
    },
]

// ==================== 白名单配置 ====================

/**
 * 运行时规则覆盖：设置 RATE_LIMIT_CONFIG 指向一个 JSON 文件即可不改代码调整限流。
 *
 * 文件格式（全部字段可选，缺省沿用代码内置值）：
 *   { "global": { "max": 300, "timeWindow": "1 minute" },
 *     "rules": [ { "path": "/api/auth/login", "method": "POST", "max": 5,
 *                  "timeWindow": "1 minute" } ] }
 *
 * rules 按 (path, method) 合并覆盖内置规则（同键替换）；新增键直接追加。
 * 文件在模块加载时读取一次；解析失败会直接抛错终止启动，绝不静默忽略。
 */
function loadRuntimeOverrides(): void {
    const configFile = process.env.RATE_LIMIT_CONFIG
    if (!configFile) return

    let raw: string
    try {
        raw = readFileSync(configFile, 'utf8')
    } catch (err) {
        throw new Error(`RATE_LIMIT_CONFIG 指向的文件无法读取: ${configFile} (${(err as Error).message})`)
    }

    let parsed: { global?: { max?: number; timeWindow?: string }; rules?: RateLimitRule[] }
    try {
        parsed = JSON.parse(raw)
    } catch (err) {
        throw new Error(`RATE_LIMIT_CONFIG 文件不是合法 JSON: ${configFile} (${(err as Error).message})`)
    }

    if (parsed.global?.max != null) globalRateLimit.max = parsed.global.max
    if (parsed.global?.timeWindow) globalRateLimit.timeWindow = parsed.global.timeWindow

    if (Array.isArray(parsed.rules)) {
        for (const override of parsed.rules) {
            if (!override.path || !override.method || !override.max || !override.timeWindow) {
                throw new Error(`RATE_LIMIT_CONFIG 规则缺少必填字段 (path/method/max/timeWindow): ${JSON.stringify(override)}`)
            }
            const index = rateLimitRules.findIndex(
                r => r.path === override.path && r.method === override.method
            )
            if (index >= 0) {
                rateLimitRules[index] = { ...rateLimitRules[index], ...override }
            } else {
                rateLimitRules.push(override)
            }
        }
    }
}
loadRuntimeOverrides()

export const rateLimitWhitelist = {
    /** 跳过速率限制的路径前缀 */
    pathPrefixes: [
        '/api/ws/instances/*/terminal',  // WebSocket 终端
        '/api/admin/hosts/*/incus-proxy/*', // 管理员 Incus UI 反向隧道代理
        '/api/ws/hosts/*/incus-proxy/*'     // 管理员 Incus UI WebSocket 隧道桥接
    ],
    /** 跳过速率限制的完整路径 */
    exactPaths: [
        '/api/health'  // 健康检查
    ]
}

// ==================== 辅助函数 ====================

/**
 * 根据请求路径和方法查找匹配的速率限制规则
 */
export function findRateLimitRule(path: string, method: string): RateLimitRule | null {
    for (const rule of rateLimitRules) {
        // 检查方法匹配
        if (rule.method !== '*' && rule.method !== method) {
            continue
        }

        // 检查路径匹配 (支持 * 通配符)
        const pattern = rule.path
            .replace(/\*/g, '[^/]+')  // * 匹配单个路径段
            .replace(/\//g, '\\/')    // 转义斜杠

        const regex = new RegExp(`^${pattern}$`)
        if (regex.test(path)) {
            return rule
        }
    }
    return null
}

/**
 * 检查路径是否在白名单中
 */
export function isWhitelisted(path: string): boolean {
    // 检查完整路径
    if (rateLimitWhitelist.exactPaths.includes(path)) {
        return true
    }

    // 检查前缀匹配
    for (const prefix of rateLimitWhitelist.pathPrefixes) {
        const pattern = prefix
            .replace(/\*/g, '[^/]+')
            .replace(/\//g, '\\/')

        const regex = new RegExp(`^${pattern}`)
        if (regex.test(path)) {
            return true
        }
    }

    return false
}

/**
 * 打印速率限制配置摘要
 */
export function printRateLimitSummary(): void {
    console.log('\n📊 速率限制配置:')
    console.log(`   全局默认: ${globalRateLimit.max} 次/${globalRateLimit.timeWindow}`)
    console.log(`   自定义规则: ${rateLimitRules.length} 条`)
    console.log(`   白名单路径: ${rateLimitWhitelist.exactPaths.length + rateLimitWhitelist.pathPrefixes.length} 条\n`)
}
