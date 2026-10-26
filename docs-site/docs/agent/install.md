# 安装节点连接器

节点连接器随 [Rain-kl/incus](https://github.com/Rain-kl/incus) 的 `incus-payincus` deb 安装，由同一软件包更新。仅支持 Debian 13、Ubuntu 24.04 的 amd64 / arm64。

在宿主机管理页面生成安装命令，在全新节点运行后通过 `incus-tui` 确认 Panel 地址和一次性 token，初始化存储、设置 HTTPS 监听并导入 Panel 公钥证书。token 隐藏显示，确认后才兑换；无交互终端时请稍后手工运行 `sudo incus-tui`。

`latest` 拉取稳定包，`canary` 拉取已验证的集成包。控制台入口默认 canary。默认初始化为 DIR，ZFS 安装和配置需显式确认。

不提供独立 Agent 二进制、自更新、旧协议回退或旧节点迁移。软件包后续更新请使用 incus-tui / apt；旧节点需按全新安装流程处理。
