# rc.14 / rc.15 最终交付与验证边界

两版均已完成计划、实现、中文帮助、原生双架构回归和公开候选发布。本轮未连接171或94，未改变运行实例、数据库、内核或压力任务；保留远端其它会话提交。

| 版本 | 主要优化 | 每种架构实际验证 | 构建提交 | 原生CI / 下载 |
| --- | --- | --- | --- | --- |
| rc.14 | 有界信号读取、到期许可、年龄/余量、check-config | 86项顶层race单测、39项CLI、3项采集器；0跳过 | `243cacaa07c5aa8de6c8bd13f9e04a5c03b0956d` | [CI](https://github.com/cuipengdba/ContainerCacheGuard/actions/runs/43054188767) / [候选附件](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.14) |
| rc.15 | 缓存时间积分/覆盖、按实例compare、IO/尾延迟/自身开销门槛 | 93项顶层race单测、43项CLI、3项采集器；0跳过 | `c8523d6d2000c0c2b110b6679a59a851afadb4c7` | [CI](https://github.com/cuipengdba/ContainerCacheGuard/actions/runs/43056936817) / [候选附件](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.15) |

两版均为Linux amd64/arm64静态ELF，无INTERP/DYNAMIC段，源码构建标记干净、无HTTP依赖，实际CLI进程不监听TCP。各6个附件的服务端SHA256/大小与本地一致，标签指向已测构建提交。原始race/CLI日志、静态程序、SHA256SUMS、LICENSE、中文说明和机器可读测试JSON保留在各发布及本地交付目录。ARM64原生页大小为4KiB，64KiB页和CentOS7现场未验收。

## rc.14验证了什么

已有有效业务信号继续工作，附加字段兼容；符号链接（含父路径）、FIFO、过长尾部、重复字段及Unicode折叠别名、过深JSON均被拒绝。许可按最早有效期收回，缺失与0余量分开展示；配置检查不采集cgroup或外部信号。既有真实临时文件内容不变、缺保护暂停/恢复和日志清理回归通过。安全检查不能取消已经阻塞的内核调用，CCG停止建议不会停止备份程序。

## rc.15验证了什么

缓存积分按小时拆分；缺样、未知和重启不补时长。真实CLI生成的合成对照报告可被compare读取，并分别展示遥测门槛满足、证据不足、超限。回归覆盖：缓存改善但IO/尾延迟恶化、窗口重叠/时长不等、来源不一致、缺保护和覆盖不足、数值溢出、零基线、异常重复或负统计。JSON/Markdown与require-thresholds非零退出通过。

这些是**原生功能和安全边界回归**；合成日志不是迁移来的PG14业务试验。时间积分为采样保持估算，p99样本最大值不是整个窗口重新计算的p99，可见cgroup IO不代表全部底层磁盘延迟。compare不能验证数据/压力/备份限速与缓存起点完全一致，也不覆盖完整全备耗时、校验恢复和K8s事件，因此telemetry_thresholds_met不表示生产验收完成。

## 仍未完成的现场项目

94真实数据的匹配observe/enforce、两库完整备份/pg_verifybackup/独立恢复、持续等压、YsP备份服务正式注册与48小时均不能标为通过。未在94替换rc.14/15，不自动启用PSI，不放开源PGDATA或清理匿名/共享内存；容量和匿名内存峰值仍需独立解决。视频和领导长期方案继续暂停。

用法：[rc.14配置/信号](RC14-USAGE.zh.md)、[rc.15统计/对比](RC15-USAGE.zh.md)、[四角色挂载与信号来源](RC13-USAGE.zh.md)。
