// Code generated from server/src/config/rate-limit.ts by tools/gen-ratelimit.py. DO NOT EDIT.

package config

// RateLimitRule mirrors RateLimitRule in the Node backend.
type RateLimitRule struct {
	Path        string
	Method      string
	Max         int
	TimeWindow  string
	Description string
}

// GlobalRateLimit is the default per-IP limit applied to every request.
var GlobalRateLimit = RateLimitRule{
	Path:        "*",
	Method:      "*",
	Max:         300,
	TimeWindow:  "1 minute",
	Description: "全局默认限制",
}

// RateLimitRules lists the per-route overrides in declaration order.
var RateLimitRules = []RateLimitRule{
	{Path: "/api/auth/check-2fa", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "检查2FA状态 - 防止用户名枚举"},
	{Path: "/api/auth/login", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "登录接口 - 防止暴力破解"},
	{Path: "/api/auth/register", Method: "POST", Max: 3, TimeWindow: "1 minute", Description: "注册接口 - 防止批量注册"},
	{Path: "/api/auth/refresh", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "刷新令牌 - 适中限制"},
	{Path: "/api/auth/2fa/*", Method: "*", Max: 10, TimeWindow: "1 minute", Description: "2FA 相关操作"},
	{Path: "/api/auth/forgot-password/send-code", Method: "POST", Max: 5, TimeWindow: "1 hour", Description: "发送找回密码验证码 - 防止滥用"},
	{Path: "/api/auth/forgot-password/reset", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "重置密码 - 防止暴力破解验证码"},
	{Path: "/api/instances", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "创建实例 - 防止滥用资源"},
	{Path: "/api/instances", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取实例列表"},
	{Path: "/api/instances/*/start", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "启动实例"},
	{Path: "/api/instances/*/stop", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "停止实例"},
	{Path: "/api/instances/*/restart", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "重启实例"},
	{Path: "/api/instances/*/reinstall", Method: "POST", Max: 2, TimeWindow: "5 minutes", Description: "重装实例 - 严格限制"},
	{Path: "/api/instances/*/snapshots", Method: "POST", Max: 5, TimeWindow: "5 minutes", Description: "创建快照 - 资源密集操作"},
	{Path: "/api/instances/*/snapshots", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取快照列表"},
	{Path: "/api/instances/*/backups", Method: "POST", Max: 3, TimeWindow: "10 minutes", Description: "创建备份 - 资源密集操作"},
	{Path: "/api/instances/*/backups", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取备份列表"},
	{Path: "/api/instances/*/backups/*/export", Method: "POST", Max: 5, TimeWindow: "10 minutes", Description: "创建备份导出任务 - 资源密集操作"},
	{Path: "/api/instances/*/backups/export/*/download-token", Method: "POST", Max: 10, TimeWindow: "10 minutes", Description: "获取备份下载令牌"},
	{Path: "/api/instances/*/backups/export/*/download", Method: "GET", Max: 5, TimeWindow: "10 minutes", Description: "下载备份 - 带宽密集操作"},
	{Path: "/api/instances/*/ports", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "添加端口映射"},
	{Path: "/api/instances/*/ports", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取端口映射列表"},
	{Path: "/api/instances/*/sites", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取站点列表"},
	{Path: "/api/instances/*/sites", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "添加反代站点"},
	{Path: "/api/instances/*/sites/*", Method: "DELETE", Max: 10, TimeWindow: "1 minute", Description: "删除反代站点"},
	{Path: "/api/instances/*/sites/*/refresh", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "刷新站点配置"},
	{Path: "/api/users", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取用户列表 (管理员)"},
	{Path: "/api/users/*", Method: "PATCH", Max: 10, TimeWindow: "1 minute", Description: "更新用户信息"},
	{Path: "/api/users/*/change-email/send-code", Method: "POST", Max: 5, TimeWindow: "10 minutes", Description: "发送修改邮箱验证码"},
	{Path: "/api/hosts", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取主机列表"},
	{Path: "/api/hosts", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "添加主机"},
	{Path: "/api/node-groups", Method: "*", Max: 30, TimeWindow: "1 minute", Description: "节点组管理"},
	{Path: "/api/packages", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取套餐列表"},
	{Path: "/api/packages", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "创建套餐 (管理员)"},
	{Path: "/api/packages/*/hosts-detail", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取套餐宿主机详情"},
	{Path: "/api/packages/*/release-channel", Method: "PUT", Max: 10, TimeWindow: "1 minute", Description: "设置释放通知渠道"},
	{Path: "/api/packages/*/release-quota", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "释放配额 - 敏感操作"},
	{Path: "/api/ssh-keys", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取 SSH 密钥列表"},
	{Path: "/api/ssh-keys", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "添加 SSH 密钥"},
	{Path: "/api/ssh-keys/generate", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "生成 SSH 密钥对 - CPU密集操作"},
	{Path: "/api/notifications", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取通知列表"},
	{Path: "/api/notifications/channels", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "添加通知渠道"},
	{Path: "/api/inbox", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取站内信列表"},
	{Path: "/api/inbox/unread-count", Method: "GET", Max: 120, TimeWindow: "1 minute", Description: "获取未读数量 - 轮询场景"},
	{Path: "/api/inbox/*/read", Method: "POST", Max: 60, TimeWindow: "1 minute", Description: "标记消息已读"},
	{Path: "/api/inbox/read-all", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "全部标记已读"},
	{Path: "/api/inbox/*", Method: "DELETE", Max: 30, TimeWindow: "1 minute", Description: "删除消息"},
	{Path: "/api/inbox/read", Method: "DELETE", Max: 10, TimeWindow: "1 minute", Description: "清空已读消息"},
	{Path: "/api/inbox/admin/send/*", Method: "POST", Max: 20, TimeWindow: "1 minute", Description: "管理员发送站内信给特定用户"},
	{Path: "/api/inbox/admin/broadcast", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "管理员全站广播"},
	{Path: "/api/inbox/instances/*/notify", Method: "POST", Max: 30, TimeWindow: "1 minute", Description: "宿主机所有者发送站内信给实例用户"},
	{Path: "/api/inbox/hosts/*/notify", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "宿主机所有者通知节点实例用户"},
	{Path: "/api/images", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取镜像列表"},
	{Path: "/api/images/sync", Method: "POST", Max: 2, TimeWindow: "5 minutes", Description: "同步镜像 - 资源密集操作"},
	{Path: "/api/logs", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取日志列表"},
	{Path: "/api/help", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取帮助文档"},
	{Path: "/api/oauth/*", Method: "*", Max: 10, TimeWindow: "1 minute", Description: "OAuth 认证"},
	{Path: "/api/auth/invite", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "生成邀请码 (管理员)"},
	{Path: "/api/auth/invites", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取邀请码列表"},
	{Path: "/api/auth/user/invite", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "生成邀请码 (用户)"},
	{Path: "/api/auth/user/invites", Method: "GET", Max: 30, TimeWindow: "1 minute", Description: "获取用户邀请码列表"},
	{Path: "/api/auth/user/invites/*", Method: "DELETE", Max: 20, TimeWindow: "1 minute", Description: "删除用户邀请码"},
	{Path: "/api/tickets", Method: "POST", Max: 5, TimeWindow: "1 minute", Description: "创建工单 - 防止滥发"},
	{Path: "/api/init-commands", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取初始化命令列表"},
	{Path: "/api/init-commands/available", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取可用初始化命令列表"},
	{Path: "/api/init-commands/distros", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取发行版列表"},
	{Path: "/api/init-commands", Method: "POST", Max: 10, TimeWindow: "1 minute", Description: "创建初始化命令 - 防止滥用"},
	{Path: "/api/init-commands/*", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取单个初始化命令详情"},
	{Path: "/api/init-commands/*", Method: "PUT", Max: 20, TimeWindow: "1 minute", Description: "更新初始化命令"},
	{Path: "/api/init-commands/*", Method: "DELETE", Max: 20, TimeWindow: "1 minute", Description: "删除初始化命令"},
	{Path: "/api/tickets", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取工单列表"},
	{Path: "/api/tickets/pending-count", Method: "GET", Max: 120, TimeWindow: "1 minute", Description: "获取待处理工单数量 - 轮询场景"},
	{Path: "/api/tickets/my-hosts", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取我的宿主机工单列表"},
	{Path: "/api/tickets/hosts/*", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取指定宿主机工单列表"},
	{Path: "/api/tickets/attachments/*/content", Method: "GET", Max: 240, TimeWindow: "1 minute", Description: "读取工单图片内容"},
	{Path: "/api/tickets/*", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取工单详情"},
	{Path: "/api/tickets/*/messages", Method: "GET", Max: 60, TimeWindow: "1 minute", Description: "获取工单消息列表"},
	{Path: "/api/tickets/*/messages", Method: "POST", Max: 20, TimeWindow: "1 minute", Description: "回复工单 - 防止刷屏"},
	{Path: "/api/tickets/*/status", Method: "PATCH", Max: 20, TimeWindow: "1 minute", Description: "更新工单状态"},
	{Path: "/api/tickets/*/close", Method: "POST", Max: 20, TimeWindow: "1 minute", Description: "关闭工单"},
}

// RateLimitWhitelistExactPaths skip rate limiting entirely.
var RateLimitWhitelistExactPaths = []string{
	"/api/health",
}

// RateLimitWhitelistPathPrefixes skip rate limiting for matching prefixes.
var RateLimitWhitelistPathPrefixes = []string{
	"/api/ws/instances/*/terminal",
	"/api/admin/hosts/*/incus-proxy/*",
	"/api/ws/hosts/*/incus-proxy/*",
}
