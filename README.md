# ContainerCacheGuard

**在 Linux 容器里直接运行的文件缓存治理常驻程序。** 适用于数据库备份、大文件扫描、数据同步等容易让文件页缓存持续增长的场景。一个静态二进制，不需要安装 PostgreSQL 客户端、Python、vmtouch 或 nocache，也不要求业务由它启动。

当前已发布 [**0.2.0-rc.5 预发布版**](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.5)，提供 amd64/arm64 静态二进制；它修复了低扫描预算下释放前复查被顺序扫描耗尽令牌的问题，并限制同一文件的重复页扫描。amd64 已在 94 完成 Linux 单元测试及单文件跨容器释放复测；arm64 仍缺实机验证。新的 94 Kubernetes 实验使用 **PostgreSQL 14**，DBS/HISDBS真实数据已迁入；四容器正式接入和长时间压力验收尚未完成。基础机制测试及历史真实数据实验见 [测试报告](docs/TESTING.zh.md)。它不是“任何负载下永不驱逐”的保证：内存不足也可能来自应用堆、共享内存或资源规划，文件缓存治理只能解决其中一部分。

**rc.9 的实测边界：** 约198GiB全量备份中，源端file峰值11.92→2.07GiB、输出端5.96→3.19GiB，完整校验及独立恢复通过。但不限速校验仍使输出缓存升到5.76GiB；高强度回收下重复扫描读盘约翻倍，热点尾延迟明显变差。因此当前不建议将高吞吐实验配置直接用于全PGDATA自动执行。先观察、优先低复用产物、明确排除热点，再按业务指标验收。详见[完整实测结论](docs/RESULTS.zh.md)。

## 两种模式，按需要选择

| 模式 | 你需要提供什么 | 程序做什么 |
| --- | --- | --- |
| `auto` 全自动范围模式 | 通常不需要逐个填写路径；特殊目录可加 `auto_roots` | 发现容器可见的磁盘数据挂载、常用数据目录和 `PGDATA`，在压力和资源预算约束下处理支持的文件缓存 |
| `paths` 指定路径模式 | 填写一个或多个目录/文件，可配置排除目录 | 持续治理这些路径下的文件缓存，不扩展到其它路径；目录尚未创建时等待后续发现 |

另有独立动作开关：`observe` 只观察，`enforce` 才执行释放。它不是第三种范围模式。默认先观察，帮助你确认实际 cgroup 和目标范围。

**全自动不等于全机清缓存。** 程序不会递归扫描 `/`，也不会清空 Redis 数据、PG shared_buffers、Python 堆、tmpfs 或共享内存。它监测匿名内存、shmem 和 slab，但当前版本不主动回收这些类型。应用自己的缓存需要对应应用的接口，不能用文件释放接口替代。

## 快速开始

从 [Releases](https://github.com/cuipengdba/ContainerCacheGuard/releases) 下载对应的 `linux-amd64` 或 `linux-arm64` 文件及 SHA256 清单，校验后安装。下面在容器内执行，文件必须可被当前 UID 读取；通常使用数据文件所有者的 UID。

```sh
chmod +x containercacheguard-linux-amd64
./containercacheguard-linux-amd64 version

# 生成自动模式配置。先检查发现的目录和 cgroup。
./containercacheguard-linux-amd64 init-config --mode auto > /tmp/ccg.json
./containercacheguard-linux-amd64 doctor --config /tmp/ccg.json
./containercacheguard-linux-amd64 run --config /tmp/ccg.json --duration 2m

# 确认范围后启用：例子把普通文件缓存软预算设为 2 GiB。
./containercacheguard-linux-amd64 run --config /tmp/ccg.json \
  --action enforce --cache-target-bytes 2147483648
```

指定路径，适合先治理明确低复用的备份产物：

```sh
./containercacheguard-linux-amd64 init-config --mode paths --path /backup > /tmp/ccg-paths.json
./containercacheguard-linux-amd64 run --config /tmp/ccg-paths.json \
  --action enforce --cache-target-bytes 1073741824 \
  --reclaim-bytes-per-second 33554432
```

这里的限速是**每秒建议释放的文件范围字节数**，不是磁盘 IO 的硬限速，也不是保证每秒释放这么多内存。建议操作可能触发写回，热数据再次读取也可能产生 IO。备份自身的 `--max-rate` 等限速仍然应保留。

## 为什么不只是定时运行 vmtouch

ContainerCacheGuard 直接调用 Linux 接口，持续保留有界状态。它先看容器压力及脏页，分小范围处理，复查实际驻留变化，再观察是否很快重新驻留；发现高 IO 压力、业务信号异常或自身开销超预算时停止新释放。扫描速度、CPU 时间窗口、文件数、区域数和回收额度都可以配置。

它没有发明新的内核淘汰算法。`mincore` 只提供驻留信息，不能精确识别冷热；反馈只能降低误伤风险。与现有脚本相比，改进是**统一调度、明确预算、可解释的结果和异常退让**。[设计与边界](docs/DESIGN.zh.md) 中有完整说明及同类项目对比。

## 查看状态、暂停和停止

```sh
./containercacheguard-linux-amd64 status --config /tmp/ccg.json
# 从状态中的 pid 获取进程号，再执行：
kill -USR1 <PID>   # 暂停新的释放动作，保留观察
kill -USR2 <PID>   # 恢复策略判断
kill -TERM <PID>   # 退出；不结束业务进程
```

以可监督的前台进程运行，交给已有进程管理器或独立容器管理；不要让它意外替代原有业务入口。配置修改后重启 CCG。停止不会把已经释放的缓存自动加载回来。

## 实时 Web 页面

在 JSON 配置里加入 `"web_listen":"127.0.0.1:9898"`，或给 `run` 加 `--web-listen 127.0.0.1:9898`。默认关闭，开启后在浏览器访问 `http://127.0.0.1:9898/`。页面每 2 秒更新当前状态，绘制最近有界历史；显示普通文件缓存、总 `file`、`active_file`、`inactive_file`、匿名内存、共享内存、slab、脏页、写回、IO、OOM、策略状态和自身开销。匿名内存等指标只观察，不通过文件回收接口清理。

在 Kubernetes 中用 `kubectl port-forward` 将页面安全地转到本机；详细命令、远程访问和认证配置见 [Web 与四容器部署](docs/FOUR-CONTAINERS.zh.md)。页面历史保存在进程内，重启后从新采样开始；48 小时验收需要外部持久采集，不能只靠浏览器曲线。

## 完整文档

- [命令与全部配置参数](docs/CONFIGURATION.zh.md)：每个参数的单位、默认值和调整影响。
- [常见状态与排障](docs/TROUBLESHOOTING.zh.md)：为什么不释放、释放后不下降、跨容器监控错位，以及怎样确认暂停。
- [容器、DBS、Kubernetes 与 OpenShift 部署](docs/DEPLOYMENT.zh.md)：同容器与 sidecar、共享 inode、UID、cgroup 和旧脚本协调。
- [Web 与 DBS/HISDBS 四容器部署](docs/FOUR-CONTAINERS.zh.md)：独立备份容器、源端和输出端配置、连接与数据保留、访问页面、压力验收。
- [设计原理与参考项目](docs/DESIGN.zh.md)：回收通路、预算、状态机和支持边界。
- [问题分析与长期治理](docs/OPERATIONS.zh.md)：怎样向团队解释问题、与旧工具的区别、短中长期措施和验收方法。
- [测试报告与复现方法](docs/TESTING.zh.md)：严格区分通过、进行中和未验证。
- [最终实验数字与未达标项](docs/RESULTS.zh.md)：完整备份、校验阶段、重复读取、热点代价、实际恢复。
- [构建与发布](docs/BUILD.zh.md)：静态 amd64/arm64，标准 Go 工具链。
- [需求、任务与版本变化](docs/ROADMAP.zh.md)。

原仓库中的 PostgreSQL 专用备份原型已从当前产品树移除，旧提交保留历史。当前程序不接管备份、恢复或数据库升级。

MIT License。参考相关项目的设计思想，核心代码独立实现，不打包或复制参考项目程序。静态交付附 Go 标准库许可证。
