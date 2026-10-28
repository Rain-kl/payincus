# 套餐出口代理（v2）

同一台 Incus 宿主机可同时运行沿用节点出口、原生直连和不同固定代理出口的容器。控制面保存策略版本，实例创建时保存版本快照；Agent 按宿主 NIC 分类并转发到该版本独立的 network namespace / Xray 进程。

## 配置方式

1. 管理端「资源 → 代理」导入代理节点，然后创建「出口策略」，选择一个代理及允许使用它的宿主机。
2. 编辑套餐，在「套餐出口」选择沿用节点出口、直连或固定代理策略版本。一个版本必须授权给套餐绑定的全部宿主机。
3. 新实例创建、重装和迁移时，服务端先保存实例 NIC 绑定，实时请求 Agent 的 `network.apply.v2`，核对绑定 generation 及对应出口的就绪状态，再创建/启动 NIC。绑定失败会进入原有运维任务日志。
4. 实例「网络」显示实例快照及上次节点确认。该记录带时间，不代表实时健康检测；节点重连或应用配置时刷新确认记录。

策略版本不可修改。更换代理或授权宿主机请发布新版本，再修改套餐。已有实例、重装和克隆继续使用原快照；迁移目标也必须被原版本授权。已有实例默认 `legacy`，升级不会自动切换它们。

## 首版边界

- 套餐独立出口只支持**非特权、IPv4 NAT 容器**，不支持 VM、routed/OVN、双栈或公网 IP 套餐。宿主 bridge 规则阻断这些容器的 IPv6；无需信任容器内配置。
- 每个策略版本固定一个代理，每台宿主机最多 16 个活动策略版本；同版本的容器共享一个网关。该出口的 UDP 能力取决于代理协议及上游；TCP-only SOCKS 无法承载普通 UDP。
- IPv4 DNS（TCP/UDP，目的端口 53）送入所属网关，通过该代理向 `1.1.1.1:53` 发起 TCP DNS，包括 TXT 等非 A/AAAA 查询。直连实例查询宿主 bridge DNS 时改用宿主真实 IPv4 上游，避免继承旧节点的代理 DNS。
- 宿主管理的内部网段保留直连。节点原有防火墙继续约束所有实例，公网 DNAT 的回程保持原路径。
- 宿主必须在线且 Agent 支持 `network.apply.v2`。离线购买在交易事务内拒绝，不会提交扣费和实例记录。

## 隔离和故障行为

- 绑定使用确定性的宿主 NIC 名 `ipg` + 11 位摘要，同时校验 NIC、bridge、分配的源 IPv4；没有绑定的 `ipg*` NIC 丢弃。Incus NIC 开启 MAC/IPv4 防伪造。
- 每组独立 namespace、Xray unit、路由表和 readiness mark；一组失效不会清空其它组 readiness。每组准备/探测单独超时，并限制并发。
- 代理组未就绪、进程退出或网关路由消失时阻断出口；路由表另有 unreachable 默认路由，forward 链同时校验出接口，避免回落到宿主机原生出口。
- 内核启动时独立的 `incudal-package-guard.service` 先阻断全部套餐 NIC；新版恢复器重建规则后再解除该启动屏障。该屏障不依赖 Agent 解析 v2，即使误降级，套餐 NIC 也保持阻断。
- `applied.json`、`groups/registry.json`、配置和 Agent 状态位于 `/var/lib/incudal-agent/network`。配置含代理凭据，禁止复制到工单或普通日志。

## 节点排查（只读）

策略版本 ID 为 123 时，其资源名为 `incudal-eg-v123` / `incudal-egress-v123.service`。实例使用的策略版本由数据库实例快照确定，不是策略展示的最高版本号。

```sh
systemctl status incudal-network.service incudal-package-guard.service
systemctl status incudal-egress-v123.service
journalctl -u incudal-agent -u incudal-egress-v123.service --since '-10 min'
ip netns list
ip rule show
nft list table bridge incudal_package_ports
nft list table inet incudal_package_egress
```

路由表为 `8000 + slot`，规则优先级为 `17000 + slot`，早于旧节点出口的 17420；slot 为 1–63 的持久分配值，不能将版本 ID 直接用作 slot。预留网段 `169.254.244.0/24`、`ipgh*` / `ipgn0` 及对应 namespace/表名不能被其它网络服务占用，发现冲突会拒绝应用。

不要直接删除表、namespace、启动屏障或状态目录来“恢复网络”，这会破坏故障阻断。正常删除实例会移除对应绑定，并在最后一个引用消失后回收网关。停用套餐不会迁移已有实例。

## 升级、验证及回退

先应用数据库迁移和服务端，再升级目标宿主机 Agent，确认 capability，最后在少量新套餐上启用。禁止把套餐出口直接批量套用到存量实例。应用版本默认值保证旧数据继续沿用节点策略。

回退时先停止新建套餐出口实例，处理已有实例的替换/删除；不要对仍有 v2 实例的宿主机直接降级 Agent。误降级后的启动阻断需要恢复支持 v2 的 Agent 才会解除。服务端新表/字段为增量迁移；存在引用时禁止删除策略或代理。

本地检查：

```sh
pnpm --filter server test:package-egress
# 只允许一次性的本机 *_test PostgreSQL；先 migrate deploy。
DATABASE_URL='postgresql://postgres@127.0.0.1:55482/payincus_egress_test' \
  pnpm --filter server test:package-egress:db
cd agent && go test ./...
```

`TestPackageEgressDatapath` 必须在无外部网卡的一次性完整内核 VM 中运行，绝不能在应用宿主或普通容器中开启。需要 root、`/run/incudal-datapath-test-authorized` 哨兵、`INCUDAL_PACKAGE_DATAPATH_TEST=1` 和指向项目固定版本 Xray 的 `PACKAGE_EGRESS_XRAY`。覆盖双出口、旧出口/直连共存、TCP/UDP DNS、公共端口回程、防伪造、单组停机及路由丢失、启动阻断。

内核/模拟上游测试不等于生产 Incus 实例验收。上线前还需在测试宿主机用真实 Incus 跑创建、重装、克隆、迁移、重启宿主，以及实际协议上游的 UDP 能力。

## 本次实现的本地验证

- Agent 版本 `v1.3.0`，通过 `make check`、Go 全套测试及固定 Xray 二进制的配置校验。
- 一次性 Debian 13 完整内核 VM（没有外部 NIC）通过真实 nftables、namespace 和 Xray 的转发/故障测试，包括普通 UDP、DNS、端口回程、IPv6/源 IP 拦截、单组停止、路由消失及启动屏障。重复应用没有累积 `ip rule`。
- 一次性 PostgreSQL 通过迁移和 API/数据库测试：版本并发、权限隔离、快照、回滚、克隆/迁移授权、绑定 generation、同宿主机并发应用串行、创建失败和重复创建的绑定保留规则。
- 前后端类型检查、服务端构建、用户/管理端构建、前端路由/主题色/i18n/dist 守卫及桌面/移动浏览器冒烟通过。
- 174 项服务端检查中 171 项通过；另 3 项在修改前的 `86c2858` 同样失败，本次未改动其范围：`test:admin-route-guards`（旧路由使用守卫别名）、`test:billing-query-guards`（既有价格仲裁静态断言）、`test:split-deploy-config`（既有 TasksView 网络调用白名单）。不是生产验收记录。
