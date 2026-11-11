# ContainerCacheGuard 0.2.0-rc.14

新增check-config离线参数检查；状态、日志和doctor输出信号年龄/有效期与节点停止线/余量。指标与节点、业务信号按最早有效期收回许可，保留连续健康恢复。

统一有界信号读取，拒绝符号链接/FIFO/超长/重复字段/过深JSON；业务附加字段仍兼容。详细升级注意事项见[使用手册](RC14-USAGE.zh.md)，开发计划见[rc.14/15计划](RC14-RC15-PLAN.zh.md)。

严格默认、无Web、静态amd64/arm64和每日30天/1GB日志保留。本轮不连接或修改171/94。受限输出策略实验性不变，PG14真实配对备份及48小时验收未完成；原生回归和附件以发布TEST-RESULTS.json为准，不能据此宣称不会驱逐。

已公开[候选版](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.14)；已测构建`243cacaa07c5aa8de6c8bd13f9e04a5c03b0956d`，[原生CI](https://github.com/cuipengdba/ContainerCacheGuard/actions/runs/43054188767)。两架构各86项顶层race单测、39项CLI、3项采集器测试通过，0跳过，6附件摘要/标签核验；[完整交付边界](RC14-RC15-DELIVERY.zh.md)。
