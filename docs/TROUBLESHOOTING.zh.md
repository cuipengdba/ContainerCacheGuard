# 使用中怎么看、怎么排查

先看 `status` 的 `decision.reason`，再看 `metrics` 和 `counters`。不要只盯住总内存或 advice_bytes。

```sh
./containercacheguard status --config /etc/ccg.json
```

## 明明开着，为什么没有释放

| 原因/表现 | 含义 | 下一步 |
| --- | --- | --- |
| `observation_only` | 仍是 observe | 核对范围后显式改为 enforce；观察阶段建议量为0是正确行为 |
| `warmup` | 启动学习期 | 等待配置的 warmup_seconds；常驻程序尽量提前启动 |
| `below_cache_target` | `file-shmem` 未超过软目标 | 无需为看到动作而把目标随意设成0 |
| `waiting_memory_pressure` | pressure模式尚未满足用量条件 | 如希望超过缓存预算就治理，使用budget；理解两者区别 |
| `no_eligible_roots` | 目录未出现、不受支持、被排除或不可见 | 运行doctor，核对挂载、路径和下一轮发现时间 |
| `unlimited_cgroup_requires_cache_target` | 看不到有限内存额度 | 配置明确的cache_target_bytes，不凭宿主总内存猜容器预算 |
| `dirty_or_writeback` | 目标cgroup脏页与写回高于阈值 | 查是否正常持续写入、磁盘拥塞；源/输出分配置，不能直接关闭全部保护 |
| `self_resource_budget` | 上个采样窗口自身CPU或RSS越界 | 比较索引规模和CPU峰值；预算过低会跟不上，过高需验证业务代价 |
| `io_bandwidth` / `io_psi` | 可观测IO条件超限 | 保留退让并调查磁盘；io.stat堆叠设备可能重复计量 |
| `business_pause` / `business_latency` | 外部业务信号要求退让 | 先处理业务异常；检查信号采集窗口和阈值 |
| `metrics_unavailable` | 指标或已配置业务信号不可用 | 查真实cgroup挂载、文件权限、信号时效；缺失不能当成0 |
| `memory_critical_stop_speculative_reclaim` | 达到临界比例，默认pause策略生效 | 先确定是什么内存。低复用输出可单独验证bounded，不能普遍强开 |
| `new_oom_kill` | v2事件计数较启动时增加 | 保守保持CRITICAL；调查后重启，不自动忘记事件 |
| `worker_slow_or_blocked` | 单工作线程超过预期时间未返回 | 停止派发；排查文件系统/设备阻塞，不增加更多工作线程去堆积 |

`CONTROL` 或 `CONTROL_CRITICAL` 只说明允许有预算的工作，不保证此刻一定找到能释放的页。

## advice_bytes 很大，缓存却不降

先区分四件事：建议范围累计量、建议前后观察净下降、后续重新驻留、当前cgroup普通文件近似量。累计采样可以重复覆盖同一页，不能当作当前驻留总量。建议可能遇到写回、固定页面或内核暂不移除；也可能刚释放就被业务重新读入。

看 `ineffective_advice`、`deferred_retries`、`observed_reappeared_bytes` 和 `protected_regions`。本版同一轮区域最多尝试三次建议，之后回到正常发现。持续写入与重复读取的文件不能要求立刻归零。若输入增长大于治理能力，就要在输入限速、治理成本和内存余量之间重新定标。

## 数据库容器高，sidecar 自己却很低

这很可能是监控范围不对。sidecar自动检测的是自己的cgroup。共享卷让它看见源文件，不会自动把数据库缓存记账搬到sidecar。明确填写业务容器cgroup_path，同时核对共享文件是同一设备/inode、UID和只读挂载。

如果目标cgroup被重建，程序要求重新发现和启动；不要让旧路径静默指向别的业务。受限OpenShift环境中，fsGroup读权限不一定足以获得真实mincore结果。

## 缓存下降了，内存仍然很高

看anon、shmem、slab等分类。PG共享内存、Redis数据集和应用堆不是文件缓存接口的清理对象。不要把这些内容强制清空，也不要为了让数字变小而删除业务文件。容器file常包含shmem，普通文件近似量使用file-shmem。

## 索引或访问提示溢出

`inventory_overflow` 或 `hint_overflows` 上升表示覆盖/提示受到预算限制。提示丢失不直接损坏文件；普通补扫仍在，但控制可能滞后。先检查实际文件数、目录数、工作时间与CPU，再调整max_files、max_directories、max_regions、max_hint_files或发现节奏。增加上限会增加内存成本，不能只把数字无限放大。

## 怎样确认停住了

发送SIGUSR1后，等状态变为SUSPENDED，再观察advice_bytes是否停止增加；已经在内核中的一次操作可能晚于信号完成。SIGUSR2恢复判断，SIGTERM退出。业务进程不由CCG管理，不会随它退出。停止后缓存不会自动恢复，业务再次读取时才会自然建立。

配置修改需要重启，没有热重载或隐藏的自动写配置行为。状态文件过期时status返回非零，进程管理器应单独监控CCG存活。

## 什么时候应停止扩大试验

当业务延迟、错误率、磁盘排队、重读或控制器成本超出选定预算时，先暂停新释放，保存版本、配置和同一窗口的指标。调整一个范围或一组明确相关的参数，再做对照。运行一次没有驱逐、或某个缓存瞬间归零，都不能代替长期和真实集群的验收。
