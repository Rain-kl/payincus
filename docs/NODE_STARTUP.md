# 节点正常启动与安全模式恢复

本文为 2.0.0 节点契约；不提供旧节点兼容或历史套餐转换流程。

## 正常停机与启动

`systemctl stop incus`、TUI 更新及正常系统关机共用 Incus 停机入口。节点保存正在运行实例的 project/name/UUID/frozen 状态，停止工作负载，卸载已注册 ZFS 加密根的密钥并验证 `keystatus=unavailable`。失败必须返回实际错误，不把正常停止写为安全告警。单个根失败不会跳过其余根；未导入池没有驻留密钥，冷启动无需先导入才能继续。

正常启动不设置 `locked`，设置独立 `recoveryPending`。容器和 VM 在等待期间不能启动，通信连接器持续在线，管理台显示服务尚不可用。崩溃后仍运行的工作负载和驻留密钥由 incusd 自身先清理，再申请新的启动授权；准备流程不依赖连接器在线。

连接器只在本地状态可读、未锁定且有待恢复项时申请控制端密钥。控制端不可达、状态未知或密钥尚未托管时等待并自动重试。只有控制端也未锁定才允许返回密钥；手动锁定的存储根不会被自动加载。加载成功后按 UUID 核验实例身份，恢复停机前运行/Frozen 状态；冷启动补充原生 boot.autostart、volatile.last_state.power。部分失败保留未完成清单，后续自动重试。正常启动执行器用独立 startupRecovering 标记所有权；告警与管理员恢复流程清除此标记，避免本机自动恢复抢跑控制端的人工批准异步任务。

## 启动证明协议

`POST /api/agent/startup-keys`：

1. 复用 HMAC 身份验证、实际 body hash 和 PostgreSQL 外层 nonce 防重放。未知节点、无效签名、过期外层认证或重复外层 nonce 不会锁定其它节点，也不会返回密钥。
2. 身份确认后验证 AES-256-GCM 内层证明：Agent token、毫秒时间戳、随机 nonce。时间容忍正负 180 秒，包含边界。内层 nonce 独立写 PostgreSQL，180 秒内重放同样拒绝；不能只依赖时钟窗口。
3. 内层加密使用 SHA256(`payincus-startup-v1\0` + 高熵 Agent secret) 作为 256-bit key，12-byte 随机 IV、16-byte GCM tag。AAD 绑定协议、请求/响应方向和 Agent ID。密文采用 base64(密文 || tag)。响应也加密并绑定原请求 nonce；不得写入缓存。
4. 已确认身份的无效内层证明持久化 `recoveryBlocked` 并递增内部 recoveryGeneration。立即尝试通过已固定证书指纹的 mTLS API 下发节点锁定；后续安全报告响应会重试该控制决定。此标志不能被普通心跳报告清除。
5. 管理员批准恢复时记录当前 recoveryGeneration；恢复成功仅能清除这一代锁。如果恢复过程中发生新告警，旧审批不能解除新锁，需重新核验。节点同样持久化已确认的控制代数：旧锁定消息即使延迟到恢复成功之后才到达，也不能重新锁定节点；新一代告警仍正常触发。

实现：`server/src/lib/node-security/startup.ts`、`routes/agent.ts`、`lib/node-security/report.ts`、`routes/node-security.ts`；节点实现位于 Incus `security/lifecycle.go`、`panel/recovery.go`、`cmd/payincus-agent/recovery.go`。用户原文 ARCTEL 尚未对应到确定算法，本实现采用 AES-256-GCM。

密钥仅在加密传输及内存中使用，不写节点磁盘、启动参数、审计日志或节点 State。ZFS keylocation 保持 prompt。控制端原有 KMS/主密钥封装、节点 GUID/指纹验证及管理员显式解锁流程继续使用。

## 安全模式锁定

CLI 关键操作或控制端锁定依旧持久化 `locked`，随后异步停止实例并卸载密钥。已锁定节点重启仍锁定，本地不会请求启动密钥，控制端也拒绝返回。管理员明确批准后，现有异步恢复任务恢复原运行状态。

正常重启期间不会降级安全模式，也不会关闭 CLI 审计。TUI 使用本机 API，不执行受审计 Incus CLI，因此合法维护与关键 CLI 风控分开处理。

## 验证与边界

测试由 GPT Luna 执行：Go 生命周期、加密请求/响应、HTTP 路由及 TUI 合同；TypeScript 证明窗口、重放、错误认证及粘性锁；专用隔离 PostgreSQL 的真实 HMAC API/密钥授权；类型检查、前端守卫及双端构建。具体结果以本次测试输出和 CI 为准。

停机清理依赖进程有机会执行及 ZFS 能正常卸载。SIGKILL/OOM kill/突然断电无法执行用户态回调；再次启动将重新清理残留再申请授权。被其它进程占用的 ZFS 不会被强行视为卸载成功。本机制不撤回此前已经被复制到外部的密钥或数据。

Incus 自身数据库、控制连接配置及启动必需系统卷必须能独立读取，不能放进需要由 Incus API 才能授权解锁的托管根。该机制用于实例数据，不改变控制程序自身启动依赖。
