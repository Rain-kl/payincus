# Incus 节点安装与分发

节点程序统一由 [Rain-kl/incus](https://github.com/Rain-kl/incus) 的 `incus-payincus` deb 提供：incusd 内置节点功能，`payincus-agent` 负责 HMAC 心跳、反向连接与日志转发，`incus-tui` 负责本地管理。控制端不再保存 Agent 源码、编译 Agent、代理独立二进制或下发自更新指令。

仅支持 Debian 13 / Ubuntu 24.04 的 amd64、arm64。安装器按系统和架构从 GitHub 选择包、验证 SHA256、通过 apt 安装，之后进入 TUI。`latest` 为稳定渠道，`canary` 为已通过 CI 的集成渠道；控制端安装入口默认 canary，当前正式 LTS 基线为 Incus 7.0.2。

1. 在控制台创建宿主机并生成连接安装命令。通信凭据与一次性 token 的授权、单次消费和有效期保持严格校验。
2. 在全新受支持节点运行命令。脚本仅下载并交接 Incus 官方定制安装器，不再生成独立 Agent service，也不做磁盘、网桥或防火墙配置。
3. TUI 从环境接收 Panel 地址和一次性 token，隐藏 token 并从子进程环境移除；管理员确认后才兑换。无交互终端时稍后运行 `sudo incus-tui`，手工输入地址与 token。
4. 在 TUI 初始化 Incus、配置 Panel 连接、本地 HTTPS 监听和 Panel 客户端公钥证书。默认初始化使用 DIR，ZFS 为显式操作；既有资源不自动覆盖。
5. 控制端核验节点连接；开启安全模式前按 NODE_SECURITY.md 完成证书指纹核对与必要密钥托管。

没有旧节点自动升级、旧服务切换、旧配置搬运或历史协议兼容。旧节点不再通过控制端取得独立 Agent 二进制，也不能使用旧业务 RPC；需按新节点流程自行重新安装。系统包的后续更新使用 incus-tui / apt，不能由心跳替换连接器文件。历史 GitHub release 不作为当前分发源，已有安装命令不保证继续有效。

控制端保留 `/api/agent/*` 作为新连接器的通信与凭据 API；`GET /api/agent/distribution` 返回当前包分发约定。独立 binary、manifest、自更新请求接口已删除。节点业务统一要求 `payincus_node_v1`，经原生 Incus mTLS API 调用；隧道仅传输字节、能力握手及节点日志。

验证：`make check` 检查源码/分发边界，`test:agent-install-command-guards` 保留 token 哈希、有效期和原子消费守卫；隧道测试覆盖仅原生 API、拒绝旧 RPC、超时、取消、连接替换与并发限制；`smoke:node-distribution` 核验新安装入口及旧分发路径不存在。节点端的真实 WebSocket、心跳契约和 TUI 安装交接测试由 Incus 仓库维护。未执行旧节点迁移或生产部署。

本地验证记录（2026-10-26）：前后端类型检查、双端构建/分发边界、make check、相关接口/鉴权/隧道测试、独立 PostgreSQL HMAC/ACK 测试，以及安装入口 latest/canary 成功与下载失败实测通过。原生连接器/握手/字节转发/TUI 测试在 Incus 仓库通过。ESLint 无错误，保留既有风格警告。

控制端 168 项全套守卫中 166 项通过；`test:admin-route-guards` 的现有路由扫描断言与 `test:billing-query-guards` 的价格仲裁文本断言，在清理前 `949103e` 提交上同样失败。本次未改动相关业务文件，未为消除这些失败放宽断言。发布和 CI 结果以对应提交的 Actions / Release 为准。
