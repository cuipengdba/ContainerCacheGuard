# 30 秒文件缓存回收保底

DBS、HISDBS 各自保留 PG14、原备份工具和一个 CCG。保底是 CCG 原文件回收执行器中的一条有界处理路径。常规回收、固定目录保底和自动发现共用同一个工作线程、驻留检查、扫描字节、建议速率、CPU/RSS 保护和区域复查。

Linux 的 `vmtouch -e` 核心行为是向内核发出 `POSIX_FADV_DONTNEED`。CCG 用 Go 实现这段操作并静态编进 x86/ARM64 二进制。完整的上游 vmtouch CLI 用于隔离对照，CCG 运行时无需安装它。 [vmtouch v1.3.1 源码](https://github.com/hoytech/vmtouch/blob/b8b30e81d51544d6b8f428552f10d5b398a77fcb/vmtouch.c#L531)

## 配置与接入

使用[同容器示例](../deploy/fallback/same-container.json)时，将 PGDATA、备份输出、同步暂存和归档目录换成实例的真实绝对路径。未填 `pgdata` 时读取 CCG 进程环境中的 `PGDATA`，应与 `SHOW data_directory` 核对。目录需存在且由 CCG UID 读得，根路径和中间路径不能经过符号链接。额外表空间使用实际挂载路径，并在 `auto_directories` 单独授权。共享挂载只授权已经评估的低复用数据范围。

1. 先用 `check-config --config /etc/ccg/cache-fallback.json` 检查语法，再用 `run --config /etc/ccg/cache-fallback.json` 观察建索引和发现情况。
2. 确认候选范围和保护信号后，可使用 `action=enforce` 单独试固定目录。自动发现保持 `auto_action=report`，核对来源、热读、开放写 FD 和实际挂载。
3. 自动发现执行需显式设 `auto_action=enforce`。同一目录只运行一个清理执行者。使用原 DBS 任务包装时，校验、压缩、传输和恢复阶段暂停相关文件的新建议。

| `cache_fallback` 字段 | 默认和用途 |
| --- | --- |
| `enabled` | false，显式启用。原 `action=observe` 时两类范围都不发建议 |
| `interval_seconds` | 30，范围 5–300。空闲轮首次看到父级或 kubelet 预警可提前开新轮。已运行的轮次继续用剩余额度 |
| `pgdata` / `backup_directories` | PGDATA 可从环境读取，至少配置一个备份根。PGDATA 在本轮保底中至少保护 60 秒 |
| `auto_directories` / `auto_action` | 默认空、report。执行仅处理授权根内且同一挂载的对象，候选目录之间不允许重叠。备份与自动目录合计最多 24 个 |
| `parent_headroom_bytes` | 2 GiB。触发阈值取该值和按现有 `memory_high_ratio` 换算的余量中的较大值，0 关闭此触发 |
| `node_headroom_bytes` | 1 GiB。基于可信节点信号的现有停线之上余量，0 关闭此触发 |
| `min_main_reduction_bytes` | 32 MiB。连续两轮有常规范围查询但复查净收益低于此值，可安排保底。0 关闭此触发 |
| `round_work_ms_per_scope` | 各 150 ms，范围 10–3000。按工作切片累计时间，系统调用阻塞可跨过预算边界 |
| `round_entries_per_scope` | 各 1024，范围 4–16384。覆盖有界遍历和候选检查尝试，目录游标续扫 |
| `round_advice_bytes_per_scope` | 各 128 MiB，最小一个 region，最高 4 GiB。总量仍要满足常规建议速率限制 |
| `proc_entries_per_pass` | 512，范围 8–4096。从目标 cgroup 的直接成员限量采样，耗尽后冷却并从游标续读 |
| `allow_bounded_critical` | false。实验时还需原 `critical_mode=bounded`。OOM、脏页/回写、PSI/IO、信号失效、任务和业务保护继续生效 |
| `expected_memory_high_bytes` | 缺省不比较，0 表示期望 `max`。发现漂移单列 `memory_high_drift` WARN。字段只读 |

单个范围的默认批次额度相当于每 30 秒最多建议 128 MiB，折合约 4.27 MiB/s。实际速率还受共享预算、文件候选、IO 和业务保护限制。看到缓存增长快于回收时，应以实测压力和余量判断能否达标。

## 候选与保护

- 固定目录与自动目录各保留独立的访问提示/FD 候选和走目录候选及区域槽位。近期看见的 FD、访问或改写事件、atime/mtime/ctime 和复查重新驻留都可触发避让。FD 写者看到一次后，等待后续完整采样确认消失，再结合保护时间判断。cgroup v1、PID 不可见、跨挂载命名空间、权限不足、队列溢出和短命进程均可能留下覆盖缺口。
- 跳过活动 WAL、删除中或已改名的文件、未授权挂载、硬链接、类型不符和权限不足的范围。表空间、嵌套挂载和 bind mount 需独立授权。设备、inode、文件长短、近期改写和挂载身份在建议前复核。
- Linux 页缓存可能由多个容器共用。打开记录说明路径被看到。驻留检查与路径授权不能给出单文件的 cgroup 计费归属，也不能证明每一页都冷或干净。没有足够 OOM/IO/业务信号时沿用原有暂停或恢复等待。

## 看结果

状态文件及周期日志读取 `counters.cache_fallback`。顶层有轮次、触发原因、当前许可、父级余量、已配置的 kubelet 信号、FD 采样覆盖和 `memory.high`。诊断包含最近 16 条路径及来源、发现时间及距 CCG 启动的毫秒数。未指定可信 kubelet 信号时，余量字段为空。

分别看 `fixed` 和 `auto` 中的 `discoveries_total`、`skipped_total`、`round_entries`、`round_work_us`、`round_requested_bytes`、`initial_queried_bytes_total`、`advice_bytes_total`、`immediate_decrease_bytes_total`、`delayed_net_bytes_total`、`reappeared_bytes_total`。延迟净下降按首次复查及后续差量累计。它是按区域观测的结果，可能包含自然回收。用候选 ID 和跟踪窗口复核持续性，不能把多轮建议或全部复查简单相加成独立净收益。

父级 memcg 余量是 `memory.max - memory.current`。kubelet 的可用内存和驱逐阈值按实际信号口径单列，节点或 allocatable 的可用内存与单个 Pod 的 cgroup 当前占用分别记录。验收同时看净下降持续时间、真实 kubelet 余量、SQL/API 尾延迟、额外读盘、OOM/驱逐和备份校验。 [Kubernetes 驱逐信号](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/) · [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)
