# 节点通信与实例高级配置

## 行为

节点连接器默认每 10 秒发送心跳；控制端缺失指标时也按 10 秒显示默认周期。节点接受 5–3600 秒的显式配置，新 TUI 连接配置写入 10 秒。已有配置中的显式间隔保持不变，可自行调整后重启连接器。心跳用于观测，配置修改直接走现有的常驻反向隧道或直连 mTLS Incus API。

实例详情 → 高级配置 → 编辑，支持运行/停止实例的以下设置：

| 设置 | Incus 原生字段 | 生效方式 |
| --- | --- | --- |
| 读写吞吐 / IOPS | 根磁盘 `limits.read/write` | 驱动和内核支持时热更新 |
| 入站 / 出站带宽 | 主网卡 `limits.ingress/egress` | 支持流控的 NIC 和 HTB 内核支持时热更新 |
| 最大进程数 | `limits.processes` | LXC 热更新；管理员不受用户自助提升上限约束 |
| CPU 优先级 | `limits.cpu.priority` | 0–10，保留 0 |
| 自动启动、优先级、延迟、关机超时 | `boot.autostart`、`boot.autostart.priority`、`boot.autostart.delay`、`boot.host_shutdown_timeout` | 写入后用于后续生命周期，不自动重启实例 |

优先级范围 0–100，延迟和关机超时允许 0–600 秒。带宽输入整数 Mbit/Gbit 或 Mbps/Gbps，后者在保存时转为原生单位；存储输入带单位速率，例如 `100MB`。0、false 与继承值 null 分别处理。一次请求的读/写各只能选择吞吐或 IOPS 一种模式。进程限制仅用于 LXC。

管理员或节点所有者可保存高级配置；普通实例租户仍只能使用已有的受限自助功能，不能直接提升此接口权限。转移、恢复、备份上传和实例任务冲突会拒绝操作；同实例同时保存被拒绝。新接口是 `PATCH /api/instances/:id/advanced-config`，继续使用现有登录鉴权。

## 接线和一致性

- `server/src/routes/instance-advanced-config.ts`：权限、参数与任务检查、同实例并发保护、带宽仲裁锁、节点确认、数据库及审计。
- `server/src/lib/incus/instance-advanced-config.ts`：集中读取并 PATCH 原生实例 API，沿用认证后的 IncusClient，发现实际根磁盘及主网卡名称。主网卡优先设备名 eth0、接口名 eth0，否则要求唯一 NIC；有歧义时拒绝操作。profile 继承设备完整复制为实例级覆盖，保留池、网络、MAC、地址等属性，不改共享 profile。
- `incus-client.ts` / `incus-utils.ts`：配置 API 的异步操作等待传递 AbortSignal。默认 30 秒操作预算，正常无需等待下一次心跳。远端断线、拒绝或异步失败不报告成功，不提前保存覆盖值。
- null 重置会把套餐/方案默认值重新应用到节点；不只是删除数据库标记。无套餐时展示和使用面板系统默认值。更换套餐后不会自动改写此前复制到实例设备的其它 profile 属性。
- HTTP 超时不保证远端 operation 未完成。节点成功之后的数据库/审计故障也可能造成部分完成；应刷新节点实际配置并重试同一设置，不能将错误响应解释为已回滚。
- `Instance.configuredLimitsIngress/Egress` 保存管理员设置，原有 `limitsIngress/Egress` 保留实际限速语义。带宽变更使用现有 `bandwidth:instance:<id>` 锁，与超额/恢复仲裁串行；超额期间取较严格值，解除后恢复管理员值，重置后回到方案/套餐值。
- 新增数据库结构 `20261026010000_add_instance_bandwidth_overrides`，部署时须先按项目现有发布流程应用这两个可空字段并生成 Prisma Client。无旧 Agent 安装/升级兼容层，不做旧字段历史数据推断或回填。

## 验证（2026-10-26）

- `server/scripts/test-instance-advanced-config.ts`：真实 Fastify 路由，未登录/租户/节点所有者/管理员、非法参数、任务冲突、节点失败不落库；实际适配器校验非标准及继承设备、默认值、零/false、IOPS、带宽意图与超额限速。Undici MockAgent 经过真实 IncusClient 验证原生异步成功、失败及等待取消。
- `server/scripts/test-instance-advanced-config-db.ts`：隔离 PostgreSQL 验证零/false/继承存储、管理员带宽与实际限速分离、超额及恢复后配置保留。CI 纳入这两项测试及相关流量守卫。
- 相关实例、任务冲突、带宽、节点报告、隧道、分发、前端路由/颜色/i18n 守卫通过；服务端/前端类型检查、用户端与管理端 build、dist 边界检查及 make check 通过。前端局部 ESLint 无错误，仍有既有格式警告。
- Incus 侧 config/report/service/connector race 测试、golangci-lint 和独立 TUI 测试通过。官方 LTS 仍为 v7.0.2。
- 在无外网的隔离 Incus daemon 内，验证了停止 LXC 的全部配置 PATCH、非标准 profile 继承设备保留，写入约 19ms。此项不是生产延迟承诺。
- 当前嵌套容器内核缺少 HTB，cgroup 控制器未完整委派，运行中 I/O/网卡/CPU 限制的完整真机验收未完成；未连接或修改用户服务器，未发布/部署本次变更。
