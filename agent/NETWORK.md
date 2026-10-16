# Xray 出口与 Incus 防火墙

Agent v1.2.8 内置固定版本 Xray-core v26.3.27。发布构建下载官方归档，分别校验归档和可执行文件 SHA-256，然后将对应架构的核心打包进 Agent。普通源码构建没有下载核心时仍可编译，但会报告 Xray 不可用。Xray 使用 MPL-2.0，固定版本源代码与许可证见 `internal/egress/assets/README.txt`。

## 配置入口

- 管理员：宿主节点 → 配置 → 网络连接下方的「出口代理」独立卡片。
- 模式：不接管、Xray 直连、代理节点出站。现有节点默认不接管；已配置节点上的新增实例继承节点策略。
- 侧边栏「节点」下方的「代理」独立页面统一管理代理节点；出口代理卡片中的管理入口也可跳转到该页面。支持订阅 URL、Xray JSON、可转换的 sing-box outbound、逐行分享链接。先解析预览，再确认添加；重复节点按连接配置指纹去重。
- 分享链接支持 SOCKS5、HTTP/HTTPS、VLESS、VMess、Trojan、Shadowsocks。传输支持 TCP、WebSocket、gRPC、HTTPUpgrade、XHTTP。无法等价转换的协议、链式依赖、本地文件或 socket 覆盖选项明确报错，不执行导入配置的入站和全局路由。
- 选择多个代理节点后，故障转移按选择顺序执行，第一项为主节点；轮询模式均衡分配新连接，也可选择最低延迟。
- 管理员：宿主节点 → 防火墙，位于配置与镜像之间。默认允许，按需添加拒绝项；规则也可使用默认拒绝的白名单模式。代理关闭后防火墙继续有效。
- 全部操作仅允许管理员；共享的用户端组件不包含管理端 API 地址。

## 数据路径与边界

容器主动新建的外网 TCP/UDP 连接先通过 Incus ACL 和补充过滤，然后由宿主策略路由转发到 `incudal-egress` 网络命名空间，在该命名空间内 TPROXY 接管到 Xray。Xray 出站经过独立连接返回宿主机出口。

- 使用 conntrack 连接方向、DNAT 状态和专用连接标记，保留外部入站连接的回程。内核 NAT 和用户态 forkproxy 的实际行为仍需在目标 Incus 版本验收。
- 实际 Incus 网段、routed 分配地址/路由和管理员添加的内部网段直达，仍受 ACL 限制。
- 原有未接管连接保留路径；新连接受新策略控制。被接管的连接不会在核心故障时回退直连。
- 受管桥按接口匹配，覆盖后续新容器；routed 网卡同时按分配地址/前缀匹配，避免停止、宿主重启、接口名称变化造成遗漏。新建 routed 网卡还使用稳定的 `iceg` 宿主接口前缀。
- 执行实例创建前检查网络策略已生效，并预先附加 NIC ACL 或接口标识。未知或不支持的网卡类型报告失败，不显示为完整覆盖。
- Xray 只代理 TCP/UDP。接管模式阻断新的外网 ICMP/ping 和其他未代理协议；保留内网与连接相关的 ICMP、IPv6 邻居发现。
- 网桥 DNS 仍由 Incus/dnsmasq 提供内部解析，公共上游查询经命名空间中的 Xray DNS 转发入口。Xray 代理服务器域名的引导解析由命名空间的独立 resolver 完成。

## ACL 语义

通过 Incus `/1.0/network-acls` 管理专用 ACL `incudal-host-firewall`，绑定受管网桥与受支持的 NIC；不覆盖无关 ACL 引用，保存并恢复原有默认动作。NIC ACL 负责同桥实例通信；routed NIC 使用宿主机 nftables 补充 FORWARD、INPUT、OUTPUT 过滤。出站与入站使用独立链，保证一侧允许不会跳过另一侧限制。

ACL 使用原始容器连接目标和容器端口。例如公网 2222 映射到容器 22，应限制 22。排序遵循 Incus 的 drop、reject、allow、默认动作。已有连接通常继续放行。原生 ACL 保留部分 DNS、DHCP、NDP 基础服务，不能把它们当成可以任意关闭的普通规则。

## 同步与运行

Panel 保存目标版本，通过现有 HMAC 鉴权的反向 WebSocket 隧道立即调用 Agent 应用配置，并等待实际应用状态返回；失败直接显示错误。心跳只回报状态，不再交付配置或检测。隧道重连后立即补发最新目标版本。离线、旧版 Agent 在保存前明确报错；保存后断线的目标保留，重连恢复。Incus 使用直连的宿主也建立同一控制通道，不改变其 Incus 网络连接方式。仅下发被该宿主选中的代理凭据，数据库加密存储，列表与日志不输出明文。

Agent 使用本地 Incus Unix socket，定期核对网络与实例。新增实例只更新规则，不因实例数量变化重启 Xray；修改 Xray 核心配置或升级核心可能重启代理进程，已有代理连接可能重连。故障转移使用原生 RoutingService 覆盖目标，切换影响新连接。

故障转移默认连续三次失败切换，优先节点连续两次成功后回切；未知健康状态不会直接提升为主节点。轮询/最低延迟使用 Xray 原生 balancer 与 observatory，所有候选失效时 `fallbackTag` 指向 blackhole；默认 outbound 同样为 blackhole。

代理页面点击检测后，通过隧道即时请求 Agent，前端等待本次结果；没有排队、心跳交付、历史查询或结果落库。最多 10 个节点并发 4 路，每台 Agent 同时只执行一组手动检测。检测使用独立临时 Xray 配置，不切换生产出口；结束后清理配置与临时进程。页面离开、HTTP 断开、隧道断开或超时会取消请求。HTTPS 成功不代表 UDP、ICMP 或所有站点可用。隧道请求限定方法与消息大小、关联请求与宿主/连接、设置超时与并发上限，旧版 Agent 必须升级后使用。

实际应用成功才回报 appliedRevision；失败保留失败状态。失败步骤和脱敏原因同步显示在 Panel，并写入 Agent 日志；不记录原始配置或核心可能包含凭据的输出。故障转移启动时等待 RoutingService 就绪并设为 blackhole 后再放行接管流量。代理应用失败或进程退出时，选中连接保持阻断，不打开直出作为补偿。Panel 的目标版本、已生效版本和最近回报分别显示。

Xray 服务与 Agent 分开运行。保存的规则和网关由独立恢复服务在 Incus 启动前恢复；Agent 重启/自升级不直接停止核心。完全关闭出口接管与防火墙后移除 Incus 的恢复依赖。只管理专用 nft 表、路由表 7420/命名空间表 7421、优先级 17420/17421，以及 mark 的 `0xff0000` 位；首次使用检查资源冲突。

## 前置条件与验收

宿主需要 Linux、systemd、iproute2、nftables，具备 IPv4/IPv6 TPROXY 与策略路由能力，util-linux（nsenter/mount），以及支持 bridge/NIC ACL 的 Incus 版本。Agent 在修改 ACL/DNS 前通过 `nft --check` 检查 TPROXY；不支持时返回 preflight 失败。不会自动修改内核、绕过检查或启用直接出口。

本地验证命令：

```sh
pnpm --filter server test:egress
python3 agent/scripts/prepare-xray.py amd64
pnpm test:agent
pnpm --filter server type-check
pnpm --filter client type-check
pnpm build:client
make check
```

`server/scripts/smoke-egress.ts` 只接受 `127.0.0.1:55432/egress_test` 专用数据库，验证真实 HTTP/数据库交互，不连接生产。新迁移也应在独立数据库验证。

当前开发环境已通过真实 Xray HTTPS 出站、RoutingService 的代理/blackhole 切换、导入与权限、配置并发、实时隧道请求与取消和前后端构建检查。宿主侧 nft 规则通过隔离检查；开发环境内核缺少可用的 nft TPROXY 表达式，命名空间规则不能在这里完成内核验证。

部署验收仍需一台获授权的非生产 Incus 节点，覆盖：存量/新增/停止后启动、IPv4/IPv6、TCP/UDP、DNAT 和公网路由入站回程、同桥/跨桥内网、ACL 的 IP/端口限制、所有代理失效、Agent/核心重启和宿主重启恢复。在完成这些验证之前，不应将本地构建视为生产验收。

排障命令：

```sh
journalctl -u incudal-agent -n 100 --no-pager
systemctl status incudal-xray --no-pager
```

新版日志使用 `[egress] applying / applied / apply failed / realtime probe started / realtime probe finished` 标记；`stage=config` 表示核心配置校验，`stage=start` 表示服务启动，`stage=health` 表示出口选择。新诊断与检测即时回报需要升级 Panel 和 Agent v1.2.8。

配置校验显式传入 `-format json`。原子应用的暂存文件名为 `xray.json.next`，Xray 默认无法从 `.next` 推断 JSON 格式；回归覆盖零节点直连与真实暂存路径。

## systemd 挂载视图回归

Agent 的 `PrivateTmp=true` / `ProtectHome=true` 保持启用。它们会隔离挂载视图；在其中直接 `ip netns add`，其他 systemd 服务可能只看到普通占位文件，`ip netns exec` 报 `Invalid argument` / 退出 255。命名空间的创建、查询、探测、健康切换和恢复统一经过 PID 1 的挂载视图。升级时通过打开的 nsfs 句柄接续旧视图中的网络命名空间；保留原有虚拟网卡和路由。Agent 重启后若旧句柄已消失，只回收该受管名称下的空占位文件，再创建宿主可见的命名空间。

新增真实 systemd 启动回归（需要支持私有 cgroup namespace 的本地 Docker 与 Go）：

```sh
bash agent/scripts/test-systemd-egress.sh
```

该脚本在独立 Ubuntu 24.04 容器中以安装器同样的 `NoNewPrivileges`、`PrivateTmp`、`ProtectHome`、CPU 和内存限额运行测试，使用实际 systemd、nftables 就绪集合与固定版本 Xray。覆盖旧问题的 255 复现、已有命名空间接续、零节点直连、三种代理策略启动、独立 Agent 挂载视图、核心重启、私有目录遮蔽下创建 veth/按名称操作网卡、新建及残留空占位文件恢复。此启动回归不等于容器流量的 TPROXY 验收；完整数据路径仍要求测试节点内核具备 nft_tproxy。
