# 系统版本更新日志

<!-- 此文件由 docs-site/scripts/generate-changelog.mjs 自动生成。请不要手动编辑。 -->

该页面从仓库 Git tag 和 commit 自动生成，用于展示系统版本演进。后台 OTA 的“可更新版本”和生产部署仍以 GitHub Release tag 为准。

## 最新发布状态 / Latest Release State

- 最新发布提交 / Latest Release Commit: `427dfef7c`
- 提交日期 / Commit date: 2026-10-29
- 提交说明 / Commit subject: refactor(heartbeat): reduce default heartbeat interval from 10 seconds to 5 seconds and update related configurations
- 最新 tag / Latest tag: `v2.1.0`

## 未发布变更 / Unreleased Changes

### 其他变更 / Other Changes

- feat(server-go): port security substrate and first Wave 2 route modules `8a69808eb`
- feat(server-go): bootstrap Go backend foundation with Prisma-aligned models `b443258c2`
- feat(node): authorize startup and bind shared egress policies `1fd5c1597`

### 改进与调整 / Improvements and Adjustments

- refactor(avatar): simplify avatar styles to only include initials and remove deprecated options `2dc6be3b0`

## 历史版本 / Historical Versions

## v2.1.0

- 发布提交 / Release commit: `427dfef7c`
- 提交日期 / Commit date: 2026-10-29
- 提交说明 / Commit subject: refactor(heartbeat): reduce default heartbeat interval from 10 seconds to 5 seconds and update related configurations

### 改进与调整 / Improvements and Adjustments

- refactor(heartbeat): reduce default heartbeat interval from 10 seconds to 5 seconds and update related configurations `427dfef7c`
- refactor(storage): allocate from heartbeat-backed pool records `954525a88`
- refactor(migration): remove default image data and update related documentation `42c01e6ed`
- refactor(agent): remove standalone agent distribution and legacy control `e329dc9ad`
- feat(resources): refactor package and host creation into drawer style and unify drawer toasts `e2ecf847b`
- refactor(admin): use compact frameless action icons with bottom tooltips in entertainment view `6c25ad9db`
- feat(nav): hide hosting features and move system update to settings tab `28e7721c6`
- docs: update spec to remove legacy redirects and shims `19d288517`
- refactor(hosts): decouple deriveEffectiveHostStatus to tunnel manager to prevent test script hangs `d79186df9`
- refactor(admin): remove auto-refresh toggle and simplify data loading in admin dashboard `4619ac16c`

### 其他变更 / Other Changes

- feat(tui): enhance download progress feedback and carriage return handling `3bff2123e`
- feat(ci): enhance CI configuration with caching and concurrency management `6103f562d`
- feat(storage): custom loop dir, sparse-by-default ZFS loop pools, zfs/default defaults, report cpu model `6a145f269`
- feat(storage-encryption): implement storage encryption routes and enhance session management `e017dd065`
- feat(security): run node recovery as background tasks `a11923c1b`
- feat(security): reflect node availability and restore workloads `b74140523`
- feat(admin): integrate built-in incus web ui over reverse tunnel `2f7e9a60a`
- feat: implement root ID mapping preparation for non-privileged containers `97feef6d6`
- feat(instances): apply advanced configuration directly to nodes `559185093`
- chore(agent): remove retired smoke and installer remnants `b329b9a41`
- Merge remote-tracking branch 'origin/main' `76b78bf9e`
- feat(console): open graphical console in a separate browser window `394430277`
- style(admin): refine operation task icons `aed2db724`
- feat(operations): integrate instance tasks with latest main `86c285843`
- feat(gpu): deliver multi-card bundles and validate container runtime `83c107681`
- feat(coupon): unify promo api under /api/coupon and migrate frontend to unified engine `0676ec0ff`
- feat(ui): confine slide-over drawer to main content area with oci theme tokens and action styling `250946b06`
- feat(admin): move oauth to system settings as a standalone tab `6ef81facf`
- feat(statistics): aggregate marketing and recent instances in admin overview `2295d6ac9`
- feat(admin): design and implement operational admin dashboard `364cc2910`

### 修复与稳定性 / Fixes and Stability

- fix(storage): correct pool directory validation, add guard, parallelize CI `4ce4390ca`
- fix(websocket): handle close codes more gracefully and prevent error reporting during closure `c79f1c070`
- fix(node-security): update security mode public error function `816ea7426`
- fix(incus-ui): raise ws limits, fix close codes and back off events reconnect `4245d128f`
- fix(ci): copy incus-ui package manifest in Dockerfile deps stage and run incus-ui guard in CI `20b156000`
- fix: 操作诊断与自选 HTTP 连接 `935e6a427`
- fix: perf `2a6e1df80`
- fix(console): take over existing sessions on reconnect `224051c6e`
- fix(console): clean up SPICE operations and recover instance status `09219f6a6`
- fix(storage): schedule against actual Incus pool capacity `9491f0b03`
- fix(egress): disable marked source validation inside gateway `73d7ce356`
- fix(egress): inherit host DNS for proxy bootstrap `193feab24`
- fix(agent): preserve network capabilities during install probes `d5d4e73dd`
- fix(egress): publish gateway namespaces in the host mount view `e8e4eb6ff`
- fix(egress): apply network policies and probes through live tunnel `b99050ca3`
- fix(egress): repair proxy imports, probe reporting and Xray startup `0877bb7ab`
- fix(gpu): guide OS selection without runtime preflight `f51ddf87f`
- fix(vite): add type annotations to transform handlers and enable allowedHosts `cb455933e`
- fix(billing): fix instance renewal price preview and promo discount display `36cc04847`
- fix(aff): support unified promo code validation in /api/aff/validate `852700d4a`
- fix(theme): eliminate all transparency and rgba from global styles `a16f5525f`
- fix(admin): remove transparency from oauth scope dropdown and system styles `25a5e1f9b`
- feat(admin): add pending tickets card, adjust layout and fix chart scrollbar `f1f1c5d09`
- fix(test): resolve lint errors and guard test failures across client and server `c28a99669`
- fix(host): derive effective host status based on tunnel connection and update accordingly `113e48ee6`
- fix(admin): teleport orders detail modal to body with backdrop blur and bounded height `652006594`
- fix(billing): use 3-dots action dropdown, add modal backdrop blur, remove user avatar and hosting type `043798f23`
- fix(host): teleport public ipv4 pool creation modal to body `84136d934`
- fix(admin): sync user avatar badge and teleport action menu to body `daa69c08a`
- fix(hosts): exclude tunnel hosts from address conflict monitor `bce687476`

### 新增能力 / New Capabilities

- feat: implement service management with API readiness checks and add update application menu `f61e8958f`
- feat: add 'ops' tab to MyHostDetailView and reorder existing tabs `9658a3e3c`
- feat(security): add node protection controls and key custody `949103e2a`
- ci: allow manual integration verification `b1bea1cc9`
- feat(egress): support versioned package proxy exits `97eff8ab0`
- feat(admin): add operation task controls `43969a75d`
- feat(operations): add instance tasks and live diagnostic logs `2ab3db49e`
- feat(console): add authenticated SPICE graphical console for KVM `8d12f3997`
- feat(egress): add Xray proxy and Incus firewall policies `63d872d8a`
- feat(gpu): add shared NVIDIA GPU container support `5edd7e1e9`
- feat(promo): support promo code editing with immutability constraints `9e69ef765`
- feat(promo): add unified promo code engine `f3900bffe`
- feat(resources): convert host and package creation to drawers and remove create routes `54f045010`
- feat(i18n): add marketing label to language files and update admin navigation `ae4dbce83`
- feat(admin): refactor revenue trend to full row and add marketing and recent instances cards `a1c45d1a8`
- feat(i18n): add marketing and recent provisioning keys for admin dashboard `4428249c7`
- docs(spec): add admin dashboard revenue row and activity cards design `61ee85d2d`
- docs: add design spec for hiding hosting and moving system update tab `08a9a84d9`

## v2.0.0

- 发布提交 / Release commit: `148f219da`
- 提交日期 / Commit date: 2026-09-21
- 提交说明 / Commit subject: feat(traffic): extract reusable TrafficLineChart and refactor host traffic stats to line chart

### 改进与调整 / Improvements and Adjustments

- feat(traffic): extract reusable TrafficLineChart and refactor host traffic stats to line chart `148f219da`
- refactor(admin-host): split host config fat form into sectioned cards with sticky save bar `a3b1c6fd3`
- feat(agent): guard agent code changes against stale version in make agent-hash `40b1b6ad4`
- docs(AGENTS): update contribution guidelines for Git commits `e8b94a512`
- refactor(PortalView, PublicSiteFooter): remove unused buttons and computed properties `5e1c47f55`
- style(theme): make all dropdown menus solid opaque and remove blur `222d9adc3`
- feat(instance): update network tab labels and enhance loading indicators `9bab7bbc9`
- feat(brand): update top bar logo to logo_white.svg and favicon to logo.svg `407a17d0e`
- style(theme): update oci css variables and component styles in main.css `96220f233`
- style(theme): update tailwind tokens to oci cloud blue, small radius and flat shadows `f1d593424`
- Makefile `de00f05ec`
- refactor: update database configuration and improve environment variable handling `c2842a105`
- docs: update telegram group link `54e97bdc7`

### 修复与稳定性 / Fixes and Stability

- fix(terminal): support websocket reverse tunnel for pure intranet hosts `9b09d50c7`
- fix(host): ensure card container does not clip country dropdown with overflow visible `91c5e7b08`
- fix(gift-cards): allow admin management when allowlist is not configured `97dce6672`
- fix(host): include China in country selection, remove transparency and fix card overlay on config dropdown `2424d6bb5`
- fix(view): remove tunnel mode indicator from host display `215b645eb`
- fix(host): enable tunnel mode by default in host configuration `60316f6eb`
- fix(host): skip address uniqueness for tunnel-mode loopback endpoints `1ebbb5df7`
- fix(dev): harden vite proxy error handler for SSE/WebSocket long-connection errors `bf7811394`
- fix(docs): update link reference for THEME.md and correct path `97f6253ba`
- fix(agent): overwrite stale caddy.service unit (User=caddy) on every install `5cdacd972`
- fix(agent): run Caddy service as root, drop caddy user requirement `e2082a1a0`
- fix(agent): ensure /etc/caddy owned by root:caddy so caddy.service can start `483f70636`
- fix(MyHostCreateView): update submit button disabled state to include tunnelEnabled check `d3d54d327`
- fix(agent): assign parsed heartbeat Caddy instruction `49974b74f`
- fix(admin-host): replace sticky save card with flush full-width action bar `77e798f09`
- fix(guards): sync stale guards and surface risk approval/verification in logs `d6918285a`
- fix(caddy): remove self-destructing pre-cleanup; cleanup only after success `d08d79ecb`
- fix(caddy): chown log dir after validate, not before `39b9997af`
- fix(agent): hash agent file contents, not filenames, in change guard `777b4dc25`
- fix(caddy): resolve invalid-user crash and log-permission startup failure `58002dcd1`
- fix(agent): hash only git-tracked agent files honoring .gitignore `d72554c1b`
- fix(host): agent tunnel WS proxying + install script resilience `e19d3d28b`
- fix(instance): live CPU usage in resource section like memory/disk `fd4b34868`
- fix(instance): plan selection uses FBF9F8 fill with neutral border, no accent `7faa9b734`
- fix(instance): represent plan selection with FBF9F8 primary instead of blue `eee6eeb0d`
- fix(i18n): simplify translations for official and market terms `39d262dd5`
- fix(instance): single background on plan cards; drop nested fixed bg `7a1b47e3e`
- fix(instance): plan card resource region follows card hover color `dc76d3cb5`
- fix(instance): show plan CPU in C units in package selector too `29b4096a4`
- fix(instance): unify package card hover so inner info region follows `cd91edd15`
- fix(instance): host selector stacks one per row when its width is under 500px `7f1914d77`
- fix(layout): use border-[length:0.5px] so tailwind emits the thinner search box border `c8e58f16f`
- fix(layout): topbar search box black by default, white on focus, 33px tall `b32028f04`
- fix(admin): keep user card action menu above sibling cards on small screens `9303351ba`
- fix(instance): teleport row menu to body and restore white table background `14dd3dd9d`
- fix(instance): migrate transfer modal to standard modal-overlay backdrop system `76c83fa6c`
- fix(system-update): switch update repository to Rain-kl/payincus `411e79625`
- fix(host): sanitize leftover Caddy state and drop --resume on reinstall `0f1ef2edd`
- fix(instance): remove unused themeStore imports in ChangePlanModal and RenewModal `1f37d719f`
- fix(modal): smooth backdrop-filter transition to prevent blur snap-in `0c4ce0b90`
- fix(admin): replace boxed external link icon with clean chevron in instance count `4dfaeb664`
- fix(storage): allow optional source for DIR pool and auto-fallback to Incus default `b2626e661`
- fix(storage): resolve storage pool creation timeouts and handle dm-thin-pool error `c2f29988d`
- fix(server): close pg pool on prisma disconnect and unref module timers to prevent process hanging `7d8ee6253`
- fix(layout): remove duplicate footer in SideNav `bf8ad28b0`
- fix(ui): unify card border-radius tokens and fix full-width topbar layout `cb801d518`
- style(layout): fix brand white text, hide sidebar scrollbar and remove topbar white gap `7e4c64092`
- style(theme): fix oci topbar #393632 color, flat underline tabs, 4px buttons and clean sidebar indicator `e3ec1ca59`
- fix(admin): use solid opaque background for oauth scope dropdown `77ea01ddf`
- fix(caddy): add newline to caddy hash-password pipe to prevent EOF error `a7b796e70`
- fix(container): ensure password authentication is enabled and clean up cloud-init drop-in `39ecda3a8`
- fix(host): do not overwrite host resource limits when recalculating used resources `17b95b270`
- fix(package): align paid package mode and hide instance limits in edit mode `38871bfd7`
- fix(host): ensure incusbr0 forwarding rules are configured and persisted for Docker compatibility `c6875d265`
- fix(host): wait for background apt locks and report errors in install_incus `9cf5e1d1c`
- fix(host): clean up conflicting sysctl forwarding entries and force apply required params `2a8fabcea`
- fix(host): remove stale incus trust certs by fingerprint during install/uninstall `19fa5f64a`
- fix(docker): set INSTALL_DIR in runtime stage `6782656cd`
- fix(docker): auto-generate panel mTLS client certificate and fix agent dist fallback `989afbfd4`
- fix(docker): allow agent source directory in docker build context `c081dfa58`
- fix(config): load server and vite dev host from environment variables instead of hardcoding `9806bd137`
- fix(server): allow binding host to 0.0.0.0 for external agent connectivity `e28395a1a`
- fix(config): remove default incudal telegram link and clean up legacy value `63916df7d`

### 其他变更 / Other Changes

- feat(agent): stream runtime logs on demand over tunnel to panel SSE `02a9c51f8`
- style(packages): rebalance admin package table column widths `e5f652ffe`
- feat(instance): enlarge region cards, set height to 65px, display 3 per row and remove radio circle `5e9005d56`
- feat(instance): convert region selector to horizontal rectangle radio cards `618400897`
- style(instance): adapt column widths with content and cap at max spacing bounds `1c24b99ac`
- style(theme): unify dropdown and popup menus to solid opaque token theme `99e6e1694`
- feat(instance): sticky right actions column, flat #F6F4F3 table, cpu cores formatting, and remove redundant batch bar `514fbe28e`
- feat(instance): rebuild instance list in oci style with horizontal scroll `955994089`
- feat(layout): streamline user component to square icon and move language settings into user menu `0bc5bd412`
- style(layout): adjust header height from 72px to 62px for consistency across components `82b05737e`
- style(instance): 操作下拉菜单去透明改纯色实底, 销毁移入菜单 `19cebe62f`
- style(instance): 去除端口映射操作区的嵌套卡片 `0dd82105d`
- style(instance): unify all instance modals to oci redwood style `b2bb1d65d`
- feat(instance): redesign traffic stats with unified area chart and 30/60/90d toggle `da2b26544`
- style(layout): remove outer box frame from page-header and use single divider line `1ef24db4f`
- feat(instances): streamline instance detail header to oci redwood style with actions dropdown `46f2700ba`
- feat(admin-users): streamline action buttons into compact more dropdown `bfc002248`
- feat(layout): display Cloud as brand name in title bar `342185b92`
- feat(avatar): use local Oracle Redwood user-icon as default avatar `cc9394b02`
- feat(avatar): display background and capitalized initial when avatar is unset `9c5b919d0`
- style(theme): implement oci topbar color and active nav indicator in kawaii-cloud.css `e5673d920`
- feat(agent): implement websocket reverse tunnel worker and local dialer `59ace8904`
- feat(incus): adapt IncusClient to route requests over tunnel virtual duplex streams `99a61ff30`
- feat(ui): redesign instance management cards for enhanced console UX `3b771385c`
- feat(agent): register /tunnel websocket endpoint and deliver tunnel config in heartbeat `fba8c89ee`
- feat(admin): optimize oauth scope selector with multi-select dropdown `5049bdc3b`
- feat(tunnel): implement server-side multiplexed tunnel protocol and manager `a96c70568`
- feat(admin): swap resources and operations categories in sidebar `b557da32f`
- feat(host): configure lvm2 and thin-provisioning tools automatically in node install script `8f53aecf8`
- feat(docker): bundle agent releases and eliminate systemd networkd startup stall `20e25de9c`
- perf(nav): expand sidebar menu groups by default `ca56c35bf`
- purge `41bf81daa`
- Merge pull request #9 from aklibk86-dev/main `e33c2d287`
- 增加docker部署 `876d49a3e`

### 新增能力 / New Capabilities

- feat(caddy): implement uninstall functionality to remove Caddy and its components `6e5ffb76b`
- feat(caddy): explicit install trigger, tunnel default-on, agent online via heartbeat `fb785a6e6`
- feat(caddy): manage Caddy via agent tunnel, auto-install with zero public exposure `6b26331c2`
- feat(makefile): add check target to verify agent code changes `ace76e91d`
- feat(agent): add make agent-hash to track package hash with UTC date version `c36b1fa13`
- feat(instance): show CPU allowance in C units (1C = 1 core) on create page `8fc9ace6d`
- feat(theme): update color variables for improved theme consistency and add markdown theme import `9b7f73b44`
- feat(instance): merge network addresses and traffic chart into info tab `7bf7082b7`
- feat(layout): add 32px footer with #F5F4F2 background, terms on left and copyright on right `f2a77901b`
- feat(admin): add copyright setting field to brand configuration `1681a0330`
- feat(brand): add brandCopyright store property and i18n translations `31148ec2e`
- feat(config): add brand_copyright system configuration and public api `e294664ba`
- docs(spec): add 3-tier layout, footer and brand copyright design spec `623eed8b5`
- feat(ui): support direct and websocket tunnel modes in host management views `9d889a7d7`
- docs(specs): add design spec for oracle cloud theme `1a6279f11`
- feat(host): add tunnel_enabled, target_host and target_port fields to host schema `12ba7dc39`
- docs(specs): add design spec for agent websocket reverse tunnel `b20d77884`
- feat(admin): support batch delete, disable and enable in images view `cdbfbd994`
- feat(instance): support password auth and anti-weak-password on creation `ea0b2652f`
- feat(docker): expose PostgreSQL port and update InstanceCreateView layout `568dded2f`
- feat(nav): add create-instance entry to user sidebar and align labels to 4 chars `ba57180da`
- refactor: clean up layout and remove unnecessary styles in InstanceCreateView `ae45f4833`
- feat: add Docker image publish workflow for aklibk/payincus `ca56c59d0`

## v1.6.0

- 发布提交 / Release commit: `b140752e9`
- 提交日期 / Commit date: 2026-07-14
- 提交说明 / Commit subject: Release v1.5.8 修复内网 IP 分配竞态 + LXC 端口转发改走内核 NAT

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.5.8

- 发布提交 / Release commit: `b140752e9`
- 提交日期 / Commit date: 2026-07-14
- 提交说明 / Commit subject: Release v1.5.8 修复内网 IP 分配竞态 + LXC 端口转发改走内核 NAT

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.5.7

- 发布提交 / Release commit: `3e2a64257`
- 提交日期 / Commit date: 2026-07-14
- 提交说明 / Commit subject: Release v1.5.7 修复宿主机 ZFS 安装失败 + 预编译内核模块加 SHA-256 校验

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.5.6

- 发布提交 / Release commit: `1cf90c34a`
- 提交日期 / Commit date: 2026-07-14
- 提交说明 / Commit subject: Release v1.5.6 OTA 不再依赖 Git + 创建实例自动下发远程端口

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.5.5

- 发布提交 / Release commit: `49ebad913`
- 提交日期 / Commit date: 2026-07-14
- 提交说明 / Commit subject: Release v1.5.5 页面级错误边界：渲染异常不再导致静默白屏

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.5.4

- 发布提交 / Release commit: `d17675b30`
- 提交日期 / Commit date: 2026-07-13
- 提交说明 / Commit subject: Release v1.5.4 修复管理端节点详情白屏（Transition out-in + KeepAlive）

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.5.3

- 发布提交 / Release commit: `24226069a`
- 提交日期 / Commit date: 2026-07-13
- 提交说明 / Commit subject: Release v1.5.3 修复节点详情白屏 + 第三方二进制固定版本与 SHA-256 校验


### 修复

- **管理后台「节点 → 节点详情」整页白屏（刷新后恢复正常）**：节点详情视图被用户端与管理端共用，但页面内负责在路由参数变化时重新加载数据的守卫写死了用户端的路由名（`my-host-detail`），管理端的路由名是 `admin-my-host-detail`，因此该守卫在管理端永远提前返回、从不加载数据。而该组件外层包着 `<KeepAlive>`，被缓存复用时 `onMounted` 不会再执行——于是页面进入「既不在加载中、节点数据又为空」的状态；模板此前没有对应的兜底分支，两个条件都不成立就什么都不渲染，表现为整页白屏（没有任何报错，刷新因为是全新挂载才恢复）。

  三处修复：路由守卫改为同时识别用户端与管理端的路由名；新增 `onActivated`，组件被 `KeepAlive` 缓存复用时按需重新加载；模板补上兜底分支，数据为空时明确展示「加载失败」并提供重试 / 返回，杜绝再次渲染成空白页。

### 安全（宿主机安装脚本）

- **第三方二进制改为固定版本 + 固定 SHA-256 校验**：`wgcf` 与 `RFW` 以 root 身份安装并执行在客户宿主机上。此前 `RFW` 拉取的是上游的 `latest`——上游一旦变更，客户机器上装到的就是另一个二进制，既不可复现也无法校验；`wgcf` 虽已固定版本但没有任何哈希校验。现在两者的版本与期望 SHA-256 都固定在脚本内，下载后逐字节比对，任一不符即丢弃并重试，校验不通过一律不安装（fail closed）。

  > 说明：仅比对上游同源的 `checksums.txt` 是不够的——能替换二进制的攻击者同样能替换那份校验和。因此哈希由本项目固定，并已通过独立下载重算核对。升级第三方组件时需同步更新版本号与哈希。

> 本版不涉及数据库结构变更。基于 v1.5.2。

## v1.5.2

- 发布提交 / Release commit: `9fda0a300`
- 提交日期 / Commit date: 2026-07-13
- 提交说明 / Commit subject: Release v1.5.2 宿主机/Agent/Caddy 安装脚本系统性整改


本版是一次针对**宿主机安装脚本 / Agent 安装脚本 / Caddy 脚本**的系统性审查与整改。这三个脚本以 root 身份运行在客户的宿主机上，本次修复了其中若干**会损坏客户机器、泄露凭据、甚至把管理员锁在服务器外**的既有缺陷。所有问题均与近期的功能精简无关，是长期存在的。

### 🔴 会导致失联 / 锁死 / 数据损坏的修复

- **`nat` 模式可能让宿主机丢失 IPv6、导致 SSH 断连**：Linux 在 `forwarding=1` 时默认停止接受 Router Advertisement（RA），只有 `accept_ra=2` 才是「开启转发仍接受 RA」。此前 IPv6 转发对所有模式启用，`accept_ra=2` 却只在 `nat_ipv6` 模式设置。绝大多数 VPS 的 IPv6 默认路由由 RA/SLAAC 下发，因此 `nat` 模式下该路由会在 RA 租期到期后（通常 30 分钟～2 小时）消失且不再刷新——宿主机失去 IPv6；若管理员通过 IPv6 SSH 登录，会在安装数小时后被锁在服务器外。现将 `accept_ra=2` 提升为所有模式共用。
- **面板证书轮换失败会让宿主机永久失联**：此前先删除旧的 `panel` 信任项再导入新证书，一旦下载到的是 CDN/WAF 错误页或损坏内容，导入失败即退出，旧信任已删、新证书未加。现在先校验确实是合法 X.509 证书，校验不通过直接中止且**不改动任何现有信任配置**。
- **ZFS 编译后的清理会删除客户自己的工具链**：此前无条件 `purge build-essential/gcc/g++/make` 并以 `linux-headers-*` 通配符删除所有内核头。现在只清理「本次为编译 ZFS 而新增」的软件包，去掉通配符。同时 `install_zfs_dkms` 改为返回真实结果——编译失败不再谎报成功、也不再据此清理工具链。
- **ZFS 内核锁定可能锁错版本**：改为锁定 `linux-image-$(uname -r)`（ZFS 模块实际对应的运行内核），避免重启后进入无 ZFS 模块的内核导致存储池不可用。
- **WireGuard / ndppd / resolv.conf 被覆盖或整目录删除**：此前安装会无条件覆盖 `/etc/wireguard/wg0.conf`（客户可能正用 wg0 作 VPN 或 SSH 管理隧道）、覆盖全局 `/etc/ndppd.conf`、直接改写 `/etc/resolv.conf`；卸载则 `rm -rf /etc/wireguard/`。现在本脚本写入的配置一律带专属标记：覆盖前若发现非本脚本所写，先备份并告警；卸载只删除带标记的文件，其余保留。
- **卸载删除范围过宽**：此前会删除当前目录下的 `*.install.sh`、`install.sh` 及固定的 `/root/install.sh`，可能误删无关安装器。现在只删除本脚本自身。
- **RFW 重装会让防火墙保持停用**：此前先停用旧服务再下载新二进制，下载失败即让客户防火墙处于停用状态。现改为先下载校验、最后才原子替换；失败时恢复旧服务。

### 🔒 安全修复

- **含 token 的渲染脚本不再以可读权限落盘**：此前包含 host / Agent 一次性 token 的脚本会被复制为 `0755` 的 `/usr/local/bin/incudal`，本地普通用户可读取仍然有效的凭据。
- **Caddy 管理密码不再进入进程参数与 shell 历史**：此前密码经 `--password` 传入命令行、并再次以 `--plaintext` 出现在 `caddy hash-password` 的进程参数中。现在密码由面板注入脚本响应（`Cache-Control: no-store`），脚本通过 stdin 传给 `hash-password`，用后立即清除。
- **Caddy TLS 私钥权限收紧**：`key.pem` 由 `0644` 改为 `root:caddy 0640`，`/etc/caddy` 目录同步收紧。
- **Agent 安装脚本 dry-run 不再打印密钥**：`agent_secret` 与一次性 install token 在 dry-run 输出中强制脱敏。
- **ZFS 预编译模块路径穿越防护**：不再无条件信任归档内的 `module_path`，校验其必须落在预期目录内。
- **下载的二进制新增 ELF 校验**：Release 资产缺失或被 CDN 拦截时可能返回 HTTP 200 + HTML，此前不校验就 `chmod +x` 执行。现在 wgcf 与 RFW 必须通过 ELF 魔数校验。

### 健壮性修复

- **安装命令会把 CDN/WAF 的人机校验页当脚本执行**：节点安装与 Caddy 脚本命令此前使用 `curl -sL` / `-sSL`（缺 `-f`）。curl 在 HTTP 4xx/5xx 时默认仍以成功退出并写出错误响应体，于是 Cloudflare 的 “Just a moment...” 挑战页 HTML 会被当作 shell 脚本执行（报 `syntax error near unexpected token '<'`）。现统一为 `curl -fsSL`。
  > 若面板域名启用了 CDN 人机校验，仍需在 CDN 侧放行安装脚本路径（`/api/hosts/install.sh*`、`/api/hosts/caddy-script/*`、`/api/agent/install.sh`），否则被挑战的机器依然拿不到脚本。
- **Agent 升级失败可回滚**：新二进制先以临时配置冒烟通过，才备份旧版本并原子替换；重启后未 active 立即回滚，避免宿主永久离线。一次性 token 也改为在全部预检与下载完成后才消费，失败可重试。
- **Caddy 配置事务化切换**：新 Caddyfile 先 `caddy validate`，通过后才原子切换；旧配置先备份，失败自动恢复；不再删除面板动态站点的 `autosave.json`；已存在且有效的证书/私钥直接复用，避免重跑后面板固定的 CA 信任失效。
- **入参校验前置**：所有带值参数检查是否缺值，端口统一校验 1–65535，用户名限制字符集——全部在任何 apt / systemctl 改动之前完成。
- **内核头文件按架构选择**（不再硬编码 amd64），关键步骤失败不再被 `|| true` 掩盖后仍报告成功。

> 本版不涉及数据库结构变更。基于 v1.5.1。

## v1.5.1

- 发布提交 / Release commit: `7f5728f9e`
- 提交日期 / Commit date: 2026-07-13
- 提交说明 / Commit subject: Release v1.5.1 修复系统设置白屏 + 退款统一走余额 + i18n编译守卫


### 修复

- 修复系统设置 / Telegram 设置页整页白屏：Telegram「机器人用户名」提示文案中有一个未转义的字面量 `@`，被 vue-i18n 当作链接消息语法（`@:key`）解析，触发编译错误（code 10 INVALID_LINKED_FORMAT），导致整页渲染崩溃。现将字面量 `@` 统一写为 `{'@'}` 转义（三语言各处）。同类问题的 Heleket 支付方式占位符（`@TRON` / `@BSC`）一并修复，避免支付渠道设置页白屏。

### 优化

- 新增 i18n 文案编译守卫：用 vue-i18n 实际的消息编译器编译全部三语言文案（本版约 2.1 万条），任何编译错误即测试失败，从源头拦截未转义 `@` / 未配对花括号等会导致运行时白屏的文案语法问题。

### 升级说明（面向自建 / 开源用户）

- 本版新增两个下线迁移，均幂等无损：
  - 删除「原路退款」的 `recharge_refund_requests` 表与 `RechargeRefundStatus` 枚举（`DROP TABLE IF EXISTS ... CASCADE` / `DROP TYPE IF EXISTS`）。
- 其余业务数据完整保留，自建用户可安全 OTA。

> 本版为 v1.5.0 的修复与收尾：根治了因文案未转义 `@` 导致的系统设置页白屏、把退款统一到余额、新增 i18n 文案编译守卫防此类问题复发，并清理了数据库中的孤儿枚举。基于 v1.5.0。

## v1.5.0

- 发布提交 / Release commit: `bb5a0ea8a`
- 提交日期 / Commit date: 2026-07-13


### 精简（移除下列模块，含前端、后端路由/worker、Prisma 模型与数据库表）

### 优化

- 全站 UI 重做为纯净的浅色主色 + 单一强调色的 Linear 风格设计，支持浅色 / 深色双主题。
- 修复长期存在的“点击侧边栏偶发白屏”问题：`index.html` 增加 `Cache-Control: no-cache`，避免浏览器 / CDN 缓存到旧入口导致的分块 404。
- 文档站删减已下线功能的冗余内容，导航去除死链。

### 修复

### 升级说明（面向自建 / 开源用户）

- 本版新增 9 个下线迁移，全部按幂等、无损原则编写：`DROP TABLE IF EXISTS ... CASCADE`、`DROP TYPE IF EXISTS`；共享枚举（`TicketObjectLinkType`、`PaymentProviderType`）采用“重命名旧类型 + 新建 + `USING` 转换”的安全收窄方式，且在收窄前先删除/转换被移除枚举值对应的数据行，不使用不受支持的 `ALTER TYPE ... DROP VALUE`。
- `plugin_gateway` 类型的支付渠道会被自动置为 `disabled` 并归类为 `manual`，不会因外键约束中止升级；升级后管理员可自行清理这些遗留渠道。
- 上述设计保证任意历史版本的部署都能顺利完成“备份 → 迁移 → 重启”的在线升级流程，仅删除被下线功能自身的数据，其余业务数据完整保留。

## v1.4.3

- 发布提交 / Release commit: `cdcdeb583`
- 提交日期 / Commit date: 2026-07-12
- 提交说明 / Commit subject: Release v1.4.3: 修复 OTA 迁移前 pg_dump 连接（role "root"）


### 修复

- 修复 OTA 迁移前数据库备份步骤连接错误：此前 `pg_dump` 未携带正确的连接信息，会回退到本地 unix socket 并以操作系统用户（root）连接，触发 `role "root" does not exist`，导致备份失败、迁移中止并自动回滚。现改为从 `DATABASE_URL` 解析出主机/端口/用户/密码/库名并通过环境变量传入 `pg_dump`，凭据不落命令行、不写入更新日志。

> 本补丁修复了 v1.4.0/v1.4.1 引入的 OTA 数据库备份缺陷，使后续在线升级能够顺利完成备份→迁移→重启流程。无 schema 变更。基于 v1.4.2。

## v1.4.2

- 发布提交 / Release commit: `630544fe1`
- 提交日期 / Commit date: 2026-07-12
- 提交说明 / Commit subject: Release v1.4.2: 版本号对齐 tag + 归档本次大版本审计台账


### 维护

- 将 `package.json` 版本号从 `1.3.7` 更新为 `1.4.2`，与发行 tag 保持一致(此前版本号未随 v1.4.0/v1.4.1 同步)。
- 归档本次大版本(v1.4.0)的审计台账与逐项评审记录到仓库(`EXECUTION_LOG.md`、`audits/`),便于团队追溯。

> 无功能/schema 变更。基于 v1.4.1(见对应发布说明)。

## v1.4.0

- 发布提交 / Release commit: `38c7bf154`
- 提交日期 / Commit date: 2026-07-12
- 提交说明 / Commit subject: Release v1.4.0 全面加固与功能补全 + OTA 数据库迁移支持


> 本版(v1.4.0)为一次大范围加固与功能补全:约 120 项缺陷/逻辑修复 + 7 个大功能,并首次为 OTA 增加数据库迁移支持。**升级前请务必阅读文末「升级注意 / 部署红线」。**

### 安全加固

- 修复开放重定向反斜杠绕过(前后端同步),防 OAuth 登录码泄漏致账号接管。
- 所有外呼(AI/SMTP/webhook/市场扫描/图床/证书查询)统一走安全出站组件:连接期 DNS 复核、禁重定向、超时、响应限长,防 SSRF/DNS rebinding。
- 日志脱敏覆盖 `pat_`/`poa_`/Basic/深层对象;`apiError` 5xx 不再返回内部异常原文。
- Caddy 管理请求启用 TLS 证书校验(CA 固定/fail-closed),不再明文可劫持。
- root OTA 单元只执行 root 拥有、服务用户不可写的固定入口与 SHA256 已验证产物,阻断服务用户→root 提权链。

### 资金与计费正确性

- 修复退款方向/漏钱:充值来源退款改「扣减」,退款按比例冲回 AFF 佣金与统计,托管「删除并退款」回扣节点主人。
- 邮箱退款按真实实付逐期、累计退款≤累计实付;延期统一 31 天基、升级剩余价值按实付封顶、三者入事务加锁。
- 订单退款限已完成状态、上限取实付、条件唯一防查重竞态;套餐价「分/元」不再放大 100 倍。
- 硬删用户/到期实例改软删/归档,不再级联抹掉财务与审计;积分↔余额比价护栏、奖池概率总和约束、礼品卡资金来源与撤销。

### 实例交付 / 网络 / 终端 / 流量

- 流量双通道加采样版本、拒迟到上报回退快照,消除重复计量;月流量重置在采集锁内重建基线再原子清零;固定 Asia/Shanghai 时区。
- 批量端口映射统一按最终数计配额/校验范围;IPv6 CIDR 子网重叠检查;资源池申领按主机容量原子校验防 OOM 超卖;宿主用量原子 increment 防并发丢失更新。
- **修复终端「Failed to connect」**:审计日志失败与连接生命周期隔离;WS 初次建连/重连加 close 监听与代次校验,消除僵尸会话。

### 规则与体验

- 礼品卡按 `expiresAt` 实时排除过期;有使用记录的兑换码禁物理删除只允许禁用/归档;流量兑换码改一次性。

### 新功能

- **AFF 佣金/折扣百分比后台可配**。
- **邮箱过期自救 + 自动续费**:过期可续费复活、到期暂停上游/续费恢复、`autoRenew` 自动扣费执行器(每次 1 月)、定时同步上游实际用量。
- **Hosting 现金提现人工审核流**(转面板余额免审即时)。
- **工单自动关闭可配置 + 可关**。

### 运维 / OTA

- **OTA 首次支持数据库迁移**:升级时在切换 release 后、重启前自动 `pg_dump` 备份并 `prisma migrate deploy`;迁移失败中止,代码自动回滚时保留 DB 备份并告警(DB 不自动回滚,如需请用备份手工恢复)。
- 明确并文档化「私有后端单进程」假设(登录锁/OAuth nonce/去重/配置缓存)。

### ⚠️ 升级注意 / 部署红线

2. **流量重置价改「元」存储须与代码原子同发**——迁移把存量「分」值 ÷100,若代码上线而迁移未跑会导致 100 倍偏差。
3. **Caddy TLS 校验**:升级前须在后端配置 `CADDY_CA_PATH` 并收集各宿主 `/etc/caddy/cert.pem`,否则反代管理会 fail-closed。
4. **root 提权链硬化(装机/OTA 单元变更)**:升级需在维护窗口重新执行装机脚本安装新 OTA 单元,并按部署清单核对属主/权限/sudoers。
5. 发布前请在维护窗口对受影响流程做真机验证。

### 验证

- 全量守卫(200+ 个 `test-*-guards`)、前后端 TypeScript、i18n 键、路由与产物边界守卫通过;用户端与管理端生产构建通过。
- 说明:以上为本地/守卫层验证,生产真机验证在部署期完成。

## v1.3.7

- 发布提交 / Release commit: `fd5e722f0`
- 提交日期 / Commit date: 2026-07-10
- 提交说明 / Commit subject: Release v1.3.7 UI consistency refinement


### 界面一致性

- 继续统一用户端与管理端的中性色视觉语言，收敛实例、套餐、消息、账务、运营和系统设置页面中过多的装饰性颜色。
- 保留失败、危险、成功、警告和处理中等关键业务状态的语义色，并补齐对应的暗色模式样式。
- 优化表格、筛选区、状态标签、操作按钮和管理端表单的间距、边框与信息层级，降低高密度页面的视觉干扰。

### 布局与可读性

- Antom 支付配置弹窗扩大到双列表单宽度，减少密钥、币种和商户字段的拥挤。
- 实例与套餐页面的网络模式、来源和类型标签改为稳定的中性标签；IPv6 地址不再截断，便于核对完整地址。
- 调整账务提醒、批量操作及多项管理表单的网格布局，并改善窄屏下的换行和内容容纳。

### 验证

- 前端 TypeScript、国际化键和路由守卫通过。
- ESLint 零错误；存量 Vue 缩进提示不阻断发布。
- 用户端与管理端生产构建、前后台产物边界守卫和后端 TypeScript 编译通过。

## v1.3.6

- 发布提交 / Release commit: `1f4008cc2`
- 提交日期 / Commit date: 2026-07-10
- 提交说明 / Commit subject: Release v1.3.6 Antom payments and UI refinement


### 界面体验

- 统一用户端与管理端的页面密度、导航、表单、列表、空状态和响应式布局，减少模板化装饰，提升长时间操作时的可读性。
- 重构首页、市场、登录、用户仪表盘及管理端核心页面的视觉层级，并补齐桌面端与 390px 移动端适配。
- 使用语义状态色区分成功、处理中、警告与失败状态，避免中性色主题削弱业务反馈。
- 引入轻量滚动揭示动效，并遵循系统的减少动态效果设置；空状态图标统一使用 Lucide 图标。

### Antom 支付

- 新增 Antom Checkout Payment 托管收银台支付方式，支持支付会话创建、结果查询、异步通知与幂等入账。
- 管理端支付方式页面新增 Antom 配置表单、字段说明、密钥脱敏和生产就绪检查。
- Antom 回调继续复用现有订单金额校验、状态流转和重复通知保护；新增 Prisma 枚举迁移与定向 guard。
- Antom 默认保持未配置和停用状态，正式启用前仍需完成商户凭证配置及沙箱实付验证；原路退款暂按人工流程处理。

### 文档与验证

- 补充中英文 Antom 配置、回调和上线前核对说明。
- 前后端 TypeScript 类型检查、前端国际化检查、Prisma 校验、183 项测试命令、用户端与管理端生产构建、文档构建全部通过。
- 首页、市场、登录、用户仪表盘、支付方式管理及 Antom 表单已完成桌面端和移动端实屏验收。

## v1.3.5

- 发布提交 / Release commit: `7f6f6b8c1`
- 提交日期 / Commit date: 2026-07-09
- 提交说明 / Commit subject: Release v1.3.5 security hardening and UI refresh


### 安全与稳定性

- 修复重建、迁移和恢复期间 Incus 实例短暂不可见时被状态调度器误标为删除的问题，并避免操作期间误发意外停机通知。
- 强化主题资源响应的 CSP 与 sandbox，降低可执行 SVG/HTML 资源造成存储型 XSS 的风险。
- OAuth state 与登录码改用常量时间签名比较，并按各自有效期逐条清理防重放 nonce。
- OTA manifest 和 artifact 仅向可信 GitHub 主机附带 Release Token；回滚拒绝删除包含原子备份的安装目录。
- Restore 与 Backup Upload Worker 只清理确实超时的 PROCESSING 任务，避免多节点部署误杀其他节点任务。
- Telegram webhook URL 不再从客户端可控的转发头推导；普通用户不再看到通知渠道 Chat ID 预览。

### 账务与支付

- 用余额生成礼品卡改用独立 `gift_card_issue` 余额日志类型，不再计入可兑换积分的真实消费额。
- 新增历史日志重分类迁移，使用礼品卡关联、金额与事务时间窗口识别历史发行扣款。
- 已验签且金额一致的迟到支付回调不再因本地过期时间被静默丢弃，继续进入幂等入账；待支付订单增加六小时取消宽限期。

### 会话与前端可靠性

- 用户端和管理端 Token 刷新增加 15 秒超时，刷新端点挂起时不再长期阻塞请求队列。
- 管理端 `/auth/me` 瞬时失败不再触发管理员整段登出。
- 用户端与管理端跨域误入时只清当前标签页内存状态，保留另一端合法共享会话。
- Service Worker 缓存名从注册 URL 的版本参数生成，后续发版无需重复手工维护缓存字面量。

### 界面与文档

- 用户端与管理端视觉调整为更克制、清晰的产品界面，统一页面密度、表单、导航、卡片和响应式细节。
- 文档站改为任务导向的信息架构，统一浅色/暗色主题、侧栏、正文、代码块、表格和风险提示，并完成中英文移动端适配。
- 修复一键升级的原子布局识别和版本输出，安装包增加 SHA256 与归档路径校验。
- 重写中英文手工部署与 systemd 指南，补齐 GitHub Pages sitemap、robots、canonical 和多语言 alternate。

### 验证

- 前后端 TypeScript 类型检查。
- 全量 `pnpm build`、`pnpm test`、文档构建和部署文档 guard。
- 文档站 70 个路由的桌面与 390px 移动端宽度、标题和链接检查。

## v1.3.4

- 发布提交 / Release commit: `bd0a36c69`
- 提交日期 / Commit date: 2026-07-09
- 提交说明 / Commit subject: Release v1.3.4 reload loop guard


### 修复与体验

- 修复首页在 Service Worker 接管或静态资源恢复期间可能反复自动刷新的问题。
- Service Worker `controllerchange` 刷新改为按客户端版本在当前会话内只执行一次，避免同一版本重复接管造成刷新循环。
- 前端静态资源版本不一致恢复逻辑改为按当前 HTML 资源签名只自动刷新一次；如果刷新后仍失败，会停止自动刷新并保留控制台告警，避免用户卡在无限刷新中。

### 验证

- `pnpm --filter client type-check`
- `pnpm --filter server test:frontend-route-guards`

## v1.3.3

- 发布提交 / Release commit: `f32e44e02`
- 提交日期 / Commit date: 2026-07-09
- 提交说明 / Commit subject: Release v1.3.3 demo safeguards and UI polish


### 修复与体验

- 合并演示账号一键登录和只读保护：演示账号登录历史脱敏，受保护账号禁止修改密码和管理员重置密码。
- 前端保留最新生产主线功能的前提下，合入二次元风格的轻量视觉升级：字体、主题色、动效 token、实例配置卡片、镜像选择下拉和资料页标题细节。
- 镜像选择下拉改为 Teleport 到 `body`，避免被父级滚动容器裁切。
- Markdown 渲染增加基础 HTML 清理，降低帮助文档等富文本内容的 XSS 风险。
- 同步 Service Worker 静态缓存名到 `v1.3.3`，确保 OTA 后客户端获取新版本静态资源。

### 验证

- `pnpm --filter client type-check`
- `pnpm --filter server type-check`
- `pnpm --filter server test:demo-account-safety-guards`
- `pnpm --filter server test:demo-notification-redaction-guards`
- `pnpm --filter server test:demo-readonly-redaction-guards`
- `pnpm --filter server test:frontend-route-guards`
- `pnpm build`

## v1.3.2

- 发布提交 / Release commit: `62338ac31`
- 提交日期 / Commit date: 2026-07-08
- 提交说明 / Commit subject: Release v1.3.2 stale asset recovery


### 修复与稳定性

- 增加前后台统一的旧静态资源恢复逻辑：捕获 Vite preload、动态 import、router 和 Vue 错误中的旧 chunk 加载失败，清理 Incudal 静态缓存后自动刷新，减少 OTA 后点击页面无反应或白屏需要手动刷新的情况。
- 登录、注册、忘记密码等 Turnstile 验证区域固定显示验证状态，明确区分“未验证 / 已验证 / 已过期 / 验证失败”，避免用户误判当前验证码是否已经完成。
- 移除前端生产 HTML 中残留的 Figma capture 外链，降低线上 CSP/外部脚本噪音。
- 同步前端路由守卫测试，覆盖旧静态资源恢复、Figma capture 移除和 Turnstile 固定状态提示。
- 同步 Service Worker 静态缓存名到 `v1.3.2`，确保 OTA 后客户端获取新版本静态资源。

### 验证

- `pnpm --filter server test:frontend-route-guards`
- `pnpm --filter client type-check`
- `pnpm --filter client build:user`
- `pnpm --filter client build:admin`
- `pnpm build`
- `git diff --check`
- 本地 CDP 全页扫描：202 页，0 issues。
- 本地可见导航点击扫描：用户端 desktop/mobile 36/36，通过；管理端 desktop/mobile 60/60，通过。

## v1.3.1

- 发布提交 / Release commit: `edd97236f`
- 提交日期 / Commit date: 2026-07-08
- 提交说明 / Commit subject: Release v1.3.1 auth redirect fixes


### 修复

- 合并 PR #8：登录成功后使用 `router.replace()` 跳转，等待导航完成并避免登录页留在历史记录中。
- 注册成功后在同步认证状态并加载当前用户后立即跳转客户控制台，不再依赖延迟 `setTimeout()`。
- 已登录用户访问登录/注册等游客页时使用 `replace` 跳转到控制台，避免返回按钮回到已失效的认证页。
- 同步前端路由守卫测试，覆盖登录、注册和游客页重定向的新跳转语义。
- 同步 Service Worker 静态缓存名到 `v1.3.1`，确保 OTA 后客户端获取新版本静态资源。

## v1.2.12

- 发布提交 / Release commit: `df34eacf9`
- 提交日期 / Commit date: 2026-07-08
- 提交说明 / Commit subject: Release v1.2.12 UI and stability fixes


### 修复与稳定性

- 修复实例改配预览的套餐价格单位，避免分/元混用导致用户看到错误差价。
- 同步 Service Worker 静态缓存名到 `v1.2.12`，确保 `/sw.js?v=1.2.12` 激活后清理旧版本静态资源缓存。

## v1.2.11

- 发布提交 / Release commit: `17d1d2265`
- 提交日期 / Commit date: 2026-07-01
- 提交说明 / Commit subject: Release v1.2.11 service worker cache version


### 修复

- 同步 Service Worker 静态缓存名到 `v1.2.11`，确保 `/sw.js?v=1.2.11` 激活后会清理旧版本资源缓存，延续 OTA 后避免旧 chunk 白屏的缓存隔离策略。

### 验证

- `pnpm --filter client build:user`
- `pnpm --filter client build:admin`
- `pnpm --filter server type-check`
- `pnpm test`
- `pnpm build`
- `pnpm --dir docs-site --ignore-workspace build`
- `git diff --check`

## v1.2.10

- 发布提交 / Release commit: `f1452aeff`
- 提交日期 / Commit date: 2026-07-01
- 提交说明 / Commit subject: Release v1.2.10 help and OAuth fallback fixes


### 修复

- 帮助中心在没有后台发布文章时提供内置“快速开始使用 Incudal”文章，避免线上帮助页空数据或文章缺失时显示通用加载失败。
- 帮助文章详情页把 `ARTICLE_NOT_FOUND` 和 404 显示为明确的文章不存在状态，减少误判为系统错误。
- OAuth 授权页对无效、停用或回调地址不匹配的客户端显示中文不可用提示，不再直接暴露原始英文错误。

### 验证

- `pnpm --filter server exec tsx scripts/test-content-route-guards.ts`
- `pnpm --filter server exec tsx scripts/test-frontend-route-guards.ts`
- `pnpm --filter client build:user`
- `pnpm --filter client build:admin`
- `pnpm --filter server type-check`
- `pnpm test`
- `pnpm build`
- `pnpm --dir docs-site --ignore-workspace build`
- `git diff --check`

## v1.2.9

- 发布提交 / Release commit: `014c5af45`
- 提交日期 / Commit date: 2026-07-01
- 提交说明 / Commit subject: Release v1.2.9 auth Turnstile cache hotfix


### 修复

- 注册、登录和忘记密码页提交时会从 Turnstile 组件和隐藏响应字段重新读取 token，并在缺少验证时聚焦到可见验证区，避免验证码组件已完成但页面仍提示未验证。
- 忘记密码页补入认证页白名单，避免残留会话检查把它当成普通用户页处理。
- 前后台入口注册 Service Worker 时使用版本化 `/sw.js?v=...` 并禁用更新缓存，避免边缘缓存返回旧 `sw.js` 导致旧 JS/CSS chunk 持续生效。

### 验证

- `pnpm --filter server exec tsx scripts/test-frontend-route-guards.ts`
- `pnpm --filter client build:user`
- `pnpm --filter client build:admin`
- `pnpm --filter server type-check`
- `pnpm test`
- `git diff --check`

## v1.2.8

- 发布提交 / Release commit: `7d166c288`
- 提交日期 / Commit date: 2026-07-01
- 提交说明 / Commit subject: Release v1.2.8 auth and OTA cache hotfix


### 修复

- 注册页发送验证码按钮不再因为 Turnstile token 尚未生成而硬禁用；点击后继续走现有校验并给出人机验证提示，避免页面看起来“不能用”。
- Service Worker 静态资源改为网络优先并升级缓存名；新 worker 接管后自动刷新一次，降低 OTA 后旧 JS/CSS chunk 导致白屏、需要手动刷新才恢复的风险。

### 验证

- `pnpm --filter server type-check`
- `pnpm --filter client build:user`
- `pnpm --filter client build:admin`
- `pnpm test`
- `pnpm build`
- `git diff --check`

## v1.2.7

- 发布提交 / Release commit: `948db6ad1`
- 提交日期 / Commit date: 2026-07-01
- 提交说明 / Commit subject: Release v1.2.7 kawaii UI refresh


### 改进

- 发布 Product Design / kawaii-cloud 主题升级：公共首页、套餐市场、认证页、用户端、资源方页面和管理端共享浅色云朵主题、玻璃质感卡片和更一致的导航布局。
- 引入 Uiverse 风格的轻量动效，但保持为本地 CSS 实现：卡片 aurora 边框、按钮 glint 反馈、骨架屏流光和 loading 高亮都遵守 `prefers-reduced-motion`。
- 增加主题图片资产，用于首页和市场页的可爱 IDC 视觉表达，不新增运行时依赖。

### 验证

- `pnpm --filter client build:user`
- `pnpm --filter client build:admin`
- `pnpm test`
- `git diff --check`
- `UI_SCAN_RUN_ID=2026-07-01T02-27-00-uiverse-motion-final node .ui-scan/run-cdp-ui-scan.mjs`
- `DATABASE_URL=... JWT_SECRET=... COOKIE_SECRET=... ENCRYPTION_KEY=... RUN_RECHARGE_CALLBACK_SMOKE=false RUN_AGENT_HEARTBEAT_SMOKE=false RUN_AGENT_RELEASE_SMOKE=false pnpm smoke:split:nginx`

## v1.1.9

- 发布提交 / Release commit: `d0bb415af`
- 提交日期 / Commit date: 2026-06-29


### 修复

- `v1.0.0` 正式版公告重写为可发布版本，补齐 0.9.x 到 1.0.0 的生产稳定性、Agent 加固、支付回调策略、资源售罄判断和升级建议。

### 验证

- `pnpm --filter server type-check`

## v1.1.1

- 发布提交 / Release commit: `c3099ec50`
- 提交日期 / Commit date: 2026-06-29


### 修复

### 验证

## v1.0.9

- 发布提交 / Release commit: `7ef8a144e`
- 提交日期 / Commit date: 2026-06-29

### v1.0.9

- Fix registration retry after Turnstile-protected failures such as an incorrect invite code: the page now resets only the Cloudflare challenge token and keeps the filled registration form intact.

## v1.0.8

- 发布提交 / Release commit: `f41632471`
- 提交日期 / Commit date: 2026-06-29

### v1.0.8

- Fix create-instance Turnstile submission: the page now uses the visible verification widget token, falls back to Cloudflare's hidden response field, blocks locally when verification is missing, and localizes raw Turnstile backend errors.

## v1.0.7

- 发布提交 / Release commit: `a6c0838ce`
- 提交日期 / Commit date: 2026-06-29
- 提交说明 / Commit subject: Release v1.0.7 instance verification

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v1.0.3

- 发布提交 / Release commit: `a6b15797e`
- 提交日期 / Commit date: 2026-06-28
- 提交说明 / Commit subject: Release v1.0.3 package delivery hotfix

Fix hosted package delivery guards and Incus certificate path resilience.

Packages can now be published or unpublished directly from the package list. The action only changes package visibility for new orders; existing instances and renewals keep their normal lifecycle.

Harden package host storage-pool persistence. When a package is active, bound hosts must resolve to an `instance_data` storage pool. Empty storage-pool selections now fall back to the host's preferred system disk pool, prioritizing `default`, and activation is blocked if no usable pool exists.

Fix stale Incus client certificate paths after OTA. If a host still references an old release directory and that certificate file no longer exists, Incus operations fall back to the stable panel certificate directory under `/opt/incudal/server/certs` or the configured panel certificate environment variables.

Restore the Germany `DEBGP` production package configuration by binding it back to the `default` system disk pool and publishing it again after confirming `DE-01` can list Incus storage pools.

## v1.0.2

- 发布提交 / Release commit: `02a9bec30`
- 提交日期 / Commit date: 2026-06-28
- 提交说明 / Commit subject: Release v1.0.2 welfare check-in

Add daily points check-in to the Welfare center.

Users can now open the Check-in tab, claim one random points reward per Beijing calendar day, and see current points, reward range, streak, monthly count and recent records. The default reward range is 1-500 points.

Add admin controls for daily check-in. Operators can enable or disable check-in, set the random points range, optionally require users to own an instance before claiming, and review paginated check-in logs with username, date, points, streak, IP and timestamp.

Harden check-in accounting with a dedicated `daily_checkins` table, a unique `(user_id, date_key)` constraint, points ledger entries, and a per-user advisory lock so concurrent requests cannot double-claim the daily reward.

Keep the existing system redeem code and resource pool flows intact. The daily check-in reward itself now grants points instead of random resource-pool credits.

Update the customer instance cards with clearer LXC/KVM, IPv4 NAT/dedicated network and quota markers so the instance list matches the newer network delivery model.

## v1.0.1

- 发布提交 / Release commit: `0e5d04951`
- 提交日期 / Commit date: 2026-06-28
- 提交说明 / Commit subject: Release v1.0.1 network delivery

Add dedicated IPv4 and dedicated IPv4 + dedicated IPv6 delivery modes for hosted instances.

Plans can now use `public_ipv4` and `public_ipv4_ipv6`. New plan creation only exposes IPv4 NAT, IPv4 NAT + dedicated IPv6, dedicated IPv6, dedicated IPv4, and dedicated IPv4 + dedicated IPv6. Legacy `nat_ipv6_nat` and `ipv6_nat` values remain for existing data compatibility only and are not new plan targets.

Add host public IPv4 IPAM. Host owners and admins can create public IPv4 pools, add addresses, disable or re-enable free addresses, and delete unused addresses from the host detail page. Dedicated IPv4 capacity is checked during host selection, and instance creation reserves addresses with row locking to avoid concurrent double allocation.

Update Incus network payload generation so dedicated IPv4 instances use a routed `eth0` NIC on the host interface. Dual-stack dedicated IPv4 + IPv6 instances also receive the existing routed IPv6 NIC. NAT IPv4 modes continue to use the `incusbr0` bridge and remain the only modes that expose NAT port mappings.

Harden provisioning rollback for dedicated IPv4. User purchases, admin-created instances, host-owner created instances, async provisioning failures, and stuck `creating` timeout cleanup now release reserved public IPv4 addresses and compensate failed purchases consistently.

Update the host install script and hosting documentation so new nodes no longer present IPv6 NAT as a target capability. IPv6 delivery is documented as routed dedicated IPv6, while dedicated IPv4 requires a configured public IPv4 pool.

## v1.0.0

- 发布提交 / Release commit: `b2857f492`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Update handoff for DEBGP closure

### PayIncus v1.0.0 正式版更新公告

### 正式版定位

v1.0.0 面向真实商业运营场景发布，目标是让面板具备稳定上线、可升级、可回滚、可审计、可排障的基础能力。

从 0.9.0 到 1.0.0，主要完成了这些能力收口：

- 在线升级链路加固，减少重复执行、重复 worker 和异常升级带来的不确定状态。
- 支付回调策略明确支持“无固定回调 IP，但强制验签、金额校验和幂等”的生产配置。
- 节点 Agent 加固，避免高频心跳、全量状态采集和日志刷屏对宿主机造成压力。

### 生产稳定性

- 修复 OTA worker 重复启动和重复处理任务的问题。
- 跳过重复任务时会正确关闭数据库连接，避免升级进程残留。
- 生产包内的 split auth smoke 修复，确保发布产物和线上验证脚本一致。
- Turnstile 开启时，split auth smoke 能识别人机验证保护状态，不再把正常防护误判为失败。
- 版本日志过滤 handoff 和日志同步类提交，减少发布记录噪音。

### 节点 Agent 加固

- Agent 默认心跳间隔调整为 60 秒，并设置 30 秒最低间隔，避免被错误配置成高频请求。
- Incus 实例状态采集只处理运行中的实例，减少对宿主机 CPU 的压力。
- 单次状态上报加入 500 实例上限和更低并发，避免节点规模较大时把宿主机打满。
- 心跳日志做节流，避免长期运行时日志无限写入导致磁盘压力。
- 生成的 `incudal-agent.service` 增加 CPU、内存、任务数量和 journal 写入限额。
- Agent 发布版本提升到 `v1.0.0`，面板可以向已有节点分发加固后的 Agent 二进制。

已有节点只升级 Agent 二进制还不够。要让 systemd 的 CPU、内存、任务和日志限额生效，需要在每台节点重新运行 Agent 安装脚本，刷新 systemd unit。

### 支付回调策略

- 新增明确的生产策略：当支付商没有固定回调出口 IP 时，可以设置 `PAYMENT_CALLBACK_IP_WHITELIST_REQUIRED=false`。
- 该策略不等于跳过安全校验。回调仍必须通过签名验证、交易状态验证、金额匹配和 `payment_callbacks` 幂等记录。
- `PAYMENT_CALLBACK_SKIP_IP_WHITELIST=true` 仍属于跳过 IP 检查的高风险配置，文档已区分这两种模式。
- 生产 readiness 不再把“明确接受无固定回调 IP 策略”的配置误报为未解决风险。

### 资源与售罄判断

- 公开套餐售罄判断补齐磁盘容量，避免前台显示可购买、下单后创建失败。
- 生产 readiness 文档补充 CPU、内存和磁盘容量要求。
- 节点/套餐资源判断更接近真实交付链路，减少购买成功后资源不足的异常。

- 证据详情现在使用实体背景覆盖，不再透出底层风险列表。
- 证据快照、表格和 JSON 内容在浅色/深色模式下都保持可读。
- 后台资源风险页更适合运营人员直接查看实例风险、证据和处置记录。

### 部署与文档

- 运行路径、系统用户、systemd 服务名和发布产物名保持兼容，仍沿用 `/opt/incudal`、`incudal` 用户、`incudal-backend` 服务和现有 release artifact 命名，避免破坏已有线上环境。
- 中文和英文部署文档补充支付回调 IP 策略、Agent systemd 限额和生产检查说明。

### 升级建议

- 建议所有 0.9.x 用户升级到 v1.0.0。
- 升级后检查后台版本页、生产健康检查、支付回调配置和节点 Agent 状态。
- 每台宿主机建议重新运行 Agent 安装脚本，让 systemd 限额配置生效。
- 如果支付通道没有固定回调 IP，请使用明确策略 `PAYMENT_CALLBACK_IP_WHITELIST_REQUIRED=false`，不要使用跳过安全检查的临时绕过配置。

### 总结

## v0.9.9

- 发布提交 / Release commit: `085ff7896`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Release v0.9.9

Keep `/opt/incudal`, the `incudal` system user, the `incudal-backend` service name and release artifact names unchanged for runtime and OTA compatibility.

## v0.9.6

- 发布提交 / Release commit: `dd0816dc8`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Release v0.9.6

Fix public package sold-out detection to include disk capacity, keeping market availability aligned with backend instance creation checks.

## v0.9.4

- 发布提交 / Release commit: `cdb1386ca`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Close OTA worker database on skipped duplicate

### 其他变更 / Other Changes

- Close OTA worker database on skipped duplicate `cdb1386ca`

## v0.9.3

- 发布提交 / Release commit: `2b5aa5dac`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Guard OTA workers against duplicate starts

### 其他变更 / Other Changes

- Guard OTA workers against duplicate starts `2b5aa5dac`

## v0.9.2

- 发布提交 / Release commit: `804fb5270`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Handle Turnstile in split auth smoke

### 其他变更 / Other Changes

- Handle Turnstile in split auth smoke `804fb5270`

## v0.9.1

- 发布提交 / Release commit: `e46a425a7`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Fix split auth smoke in production artifacts

### 修复与稳定性 / Fixes and Stability

- Fix split auth smoke in production artifacts `e46a425a7`

## v0.9.0

- 发布提交 / Release commit: `6a619cfe2`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Release v0.9.0

## v0.8.9

- 发布提交 / Release commit: `857e892d0`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Update version log for v0.8.9

- 该 tag 与相邻 tag 指向同一提交，未产生额外 Git commit。

## v0.8.8

- 发布提交 / Release commit: `5c03ca250`
- 提交日期 / Commit date: 2026-06-27
- 提交说明 / Commit subject: Update version log for v0.8.8

### 其他变更 / Other Changes

- Establish PayIncus project baseline `cddea0dc8`

## 生成方式

在仓库根目录执行：

```bash
pnpm docs:build
```

或者只刷新版本日志：

```bash
pnpm docs:changelog
```

如果 CI 或 GitHub Pages 使用浅克隆，可能拿不到完整 tag。需要在构建前拉取 tags，或改用 GitHub Release API 作为数据源。
