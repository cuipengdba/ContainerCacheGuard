# ContainerCacheGuard 0.2.0-rc.15

新增schema_version=2离线报告：普通文件缓存字节×秒、时间加权均值、CPU时间均值、业务p99采样最大值、节点/探针来源及有效覆盖。小时拆分、重启/缺样和未知值有明确处理。

新增compare按实例对照observe/enforce，先检查窗口、身份和五类覆盖，再判断缓存改善与IO、尾延迟和CCG自身开销门槛。提供JSON/Markdown、可配置阈值及require-thresholds退出检查；证据不足不伪造通过。[详细手册](RC15-USAGE.zh.md)。

完整保留[rc.14](RC14-USAGE.zh.md)信号加固、许可有效期、离线配置检查和无Web/30天1GB日志。amd64/arm64静态候选的实际结果见发布TEST-RESULTS.json。本轮不连接或修改171/94；PG14真实配对、完整备份恢复、业务48小时尚未通过。telemetry_thresholds_met仅为遥测筛查，不能称为生产验收通过。

已公开[候选版](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.15)；已测构建`c8523d6d2000c0c2b110b6679a59a851afadb4c7`，[原生CI](https://github.com/cuipengdba/ContainerCacheGuard/actions/runs/43056936817)。两架构各93项顶层race单测、43项CLI、3项采集器测试通过，0跳过，6附件摘要/标签核验；[完整交付边界](RC14-RC15-DELIVERY.zh.md)。
