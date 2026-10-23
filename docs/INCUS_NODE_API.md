# 定制 Incus 节点 API

控制端兼容 Rain-kl/incus 的 `payincus_node_v1` 扩展。设计、接口、上游 LTS 基线、部署限制及验收记录以该仓库根目录 FEATURES.md 为准。

- 普通实例管理仍使用原生 Incus API，经现有 mTLS 直连或反向隧道传输。
- 新节点在已认证隧道通告 `payincus_node_v1` 后，控制端通过同一 IncusClient 查询 `GET /1.0/payincus` 验证能力，再调用 `PUT /1.0/payincus/network` 或 `POST /1.0/payincus/proxy-probes`，原参数结构不变。
- 没有该能力的旧节点继续走控制帧。新节点原生 API 失败时不自动退回旧协议，避免一次写请求重复执行。
- 两种通道共享每节点请求并发限制；超时、调用方取消、隧道断开或被替换均会取消本地等待及原生 HTTP 请求。取消不保证节点副作用回滚，网络 revision 和现有状态核对仍是恢复依据。
- 面板套餐、订单、权限与资源编排不变。节点 API 认证授权由 Incus 执行；不要直接向租户开放节点管理员连接。

测试：`server/scripts/test-payincus-node-api.ts`、`server/scripts/test-agent-tunnel-protocol-guards.ts`；现有 package-egress、egress、agent-auth 和响应脱敏守卫仍需通过。

尚未变更旧节点安装命令和在线发布渠道；原生包必须先在隔离节点验收，不能直接让旧 Agent 自更新流程替换 incusd。

CI 的 main/canary 均执行原生节点 API 契约、隧道兼容和出口守卫；Incus 集成包的双架构构建与 RC 发布由 Rain-kl/incus 的 PayIncus Integration 流程负责。控制端不需要随本次节点集成发布升级生产环境。
