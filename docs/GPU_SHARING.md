# NVIDIA 共享 GPU 容器运营说明

此功能把每张 NVIDIA 物理显卡作为独立共享池，也支持一个容器同时共享多张物理显卡。适用于 Incus 管理的非特权 LXC 系统容器；不使用 NVIDIA 商用 vGPU，也不向 KVM 虚拟机提供共享显卡。

共享池中的容器竞争显存、计算资源和带宽。最大实例数是分配名额，不是硬显存配额或算力百分比；卡的总显存不能当作每位用户的保证容量。关机、暂停和到期封停仍保留名额，续费后可以继续使用原卡。

## 1. 宿主机准备

1. 安装适配该宿主机和显卡的 NVIDIA 驱动；`nvidia-smi` 必须能列出所有显卡。
2. 安装 Incus 所需的 NVIDIA 容器集成工具，确认 `nvidia-container-cli` 存在且 `nvidia-container-cli info` 正常。无需 NVIDIA vGPU Manager 或 vGPU 许可证。
3. 显卡计算模式须为 `Default`。该功能不支持独占计算模式、MIG 切片或 vGPU profile。
4. 安装/升级项目 Host Agent 到 **v1.1.8** 或之后包含 `nvidia-gpu` 能力的版本，使用默认 60 秒心跳。GPU 数据超过 180 秒会停止新售卖；请保持心跳间隔不超过 60 秒。
5. 宿主机支持容器，使用非特权 LXC 镜像。不要把 GPU 加入全局 `default` profile，或通过 `raw.lxc`、通配设备规则向所有容器暴露显卡。项目只给购买 GPU 的实例挂载所选 PCI 设备。
6. 使用提供 **glibc** 的已验证 Linux GPU 容器镜像（建议先验证 Ubuntu 24.04 / Debian）。Alpine 默认采用 musl，不能直接运行注入的 NVIDIA 工具；即使文件存在，也可能报 `cannot execute: required file not found` 或缺失 `gnu_get_libc_version` / `dlvsym`。应换用经过验证的 glibc 镜像，不能仅凭工具文件存在判断可用。`nvidia.runtime` 负责驱动/工具注入；应用所需的 CUDA、PyTorch 等用户态运行环境可预装在镜像内，不能在容器内安装另一套 NVIDIA 内核驱动。

在正式开售前，使用临时非特权容器验证运行时。例如用实际别名和 PCI 地址替换以下占位内容：

```sh
incus launch <已验证的容器镜像> gpu-check
incus stop gpu-check
incus config set gpu-check nvidia.runtime=true
incus config set gpu-check nvidia.driver.capabilities=compute,utility
incus config device add gpu-check gpu0 gpu gputype=physical pci=<第一张卡PCI地址>
incus start gpu-check
incus exec gpu-check -- nvidia-smi
```

再运行实际 CUDA 应用，确认可见设备与宿主 GPU UUID 对应。用第二张卡重复验证。测试结束后删除自己创建的临时容器。

## 2. 商家配置与开售

1. 部署后端并执行 `pnpm --filter server exec prisma migrate deploy`，升级 Host Agent。迁移新增 GPU 表及可空/默认关闭字段，不改变旧实例和旧套餐的资源分配。
2. 在 `/admin/resources/hosts/{id}` 的 **GPU** 页签查看每张卡的 UUID、PCI 地址、显存、GPU/显存控制器利用率、温度、功耗、驱动和最后上报时间。空宿主显示未发现 GPU；缺失指标显示“暂无数据”，不显示虚假的 0。
3. 分别开启每张卡的共享池并设置最大共享实例数。例如第一张 A10 设置 10，第二张设置 5。名额按实例计算，同一用户的两台实例占两个名额。
4. 新增/编辑 LXC 套餐，勾选共享 GPU 并选择允许的卡。可以选择同一宿主的单卡，也可以选择绑定宿主范围内的多张卡。
5. 新增/编辑方案，可继承套餐设置、关闭 GPU，或单独选择显卡。要把两组用户完全分开，用两个套餐或两个明确指定不同卡的方案；同一宿主机上选择两张卡表示每个新容器同时获得两张 GPU，分别占用两张卡的一个共享名额；任意一张卡不可用或满额，整组都不售卖。跨宿主机选择表示调度到其中一个宿主，并使用该宿主选中的所有显卡，不会跨宿主挂载设备。
6. 按普通付费方案设置价格、账期和发布状态。用户选择方案及订单摘要的硬盘后显示 GPU 型号、每张卡的物理总显存和 `×N` 数量；不同宿主提供不同 GPU 组合时明确显示为可选组合，不能把不同宿主的卡数相加。实例详情显示实际分配的整组显卡。

## 3. 分配与生命周期

- 购买、管理员开通、商家赠送在数据库事务中预占名额；并发购买不能重复出售最后一个名额。实际设备由宿主 GPU UUID 关联的 PCI 地址选择，不按 `nvidia-smi` 的易变序号选择。
- 名额满、设备消失、驱动/运行时不可用或上报过期时不分配 GPU，也不会回退成收费的 CPU 实例。
- 交付前在容器内实际执行 `nvidia-smi --query-gpu=uuid --format=csv,noheader`，检查退出码、GPU UUID 输出及可见数量；NVIDIA 工具/加载器、NVML 或多卡可见性校验失败，实例不标记为创建成功。
- 创建失败沿用现有退款和资源补偿流程。名额在确认 Incus 实例已清除后释放；清理失败时保留名额，避免残留容器和新用户超额共用。
- 关机、封停、到期和续费保留原卡及名额。删除确认完成后释放名额。
- 重启、重建及备份恢复使用保存的显卡绑定；启动时核对设备并重新限制到指定 PCI，清除其他 GPU 设备和默认 profile 继承。
- 关闭共享池只停止新开通，不摘除已有用户的 GPU；最大实例数不能降到当前分配数以下。套餐/方案编辑不改变已有实例的显卡。
- 方案变更只允许保留原卡的配置；需要变更 GPU 设备或 GPU/CPU 类型时应新购实例。GPU 实例目前明确拒绝跨宿主迁移和克隆，避免错误挂载、漏计名额或移动到不具备同等显卡的宿主。
- 拔卡或驱动故障后，原设备保留记录并标记不可用。UUID 相同的设备恢复后沿用原池配置；新卡不会因复用了 PCI 地址而自动获得旧用户的绑定。

迁移 `20261016000200_multi_gpu_containers` 为原有单卡实例回填一条显卡分配记录，保留其原卡和名额，不因套餐选择了多张卡而自动增加旧实例的显卡。多卡配置作用于之后创建的容器；方案变更必须保留实际 GPU 组合。已有多卡实例后，不要回滚到只认识单卡绑定的应用版本；该版本无法正确恢复全部设备或计算全部名额。

## 4. 验证

```sh
pnpm --filter server test:gpu-sharing
# 下面两个测试仅允许本地 PostgreSQL；自行配置本地 DATABASE_URL。
pnpm --filter server test:gpu-sharing:db
pnpm --filter server smoke:gpu-purchase
pnpm test
pnpm build
cd agent && go test ./...
```

GPU 单元测试检查严格输入、继承、PCI 精确挂载和运行时配置。数据库集成测试覆盖并发名额、双池独立、停止/删除占位、事务回滚、恢复绑定、权限和数据过期。购买 smoke 使用实际商家/购买 HTTP 路由和 PostgreSQL，但通过模拟 Incus 验证交付与失败退款；它不能替代真实 NVIDIA 硬件上的 CUDA 验收。
