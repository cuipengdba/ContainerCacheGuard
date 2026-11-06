# 命令与配置：先知道每个参数控制什么

配置格式为 JSON，UTF-8；未知字段会报错，避免拼写错误后静默采用默认值。以 `init-config` 输出为默认值的可执行依据。参数中的“字节”不是 MB 字符串：1 MiB = 1048576 字节，1 GiB = 1073741824 字节。

## 命令

| 命令 | 作用 | 是否释放缓存 |
| --- | --- | --- |
| `help`、`--help` | 使用说明 | 否 |
| `version` | 版本、构建提交、架构 | 否 |
| `init-config --mode auto或paths` | 将完整默认 JSON 输出到标准输出 | 否 |
| `doctor --config 文件` | 检查实际目录、内核页大小、UID、cgroup 指标 | 否 |
| `run --config 文件` | 前台常驻，输出限频 JSON 日志和状态文件 | 只有 enforce 且策略允许 |
| `status --config 文件` | 读取状态，过期时返回非零 | 否 |
| `scan --path 目录` | 一次完整只读驻留扫描，独立测量当前覆盖范围 | 否，但遍历与查询有成本 |

通用选项：`--config`、`--mode auto|paths`、`--action observe|enforce`、可重复的 `--path`、`--cgroup-path`、`--state-file`、`--cache-target-bytes`、`--reclaim-bytes-per-second`、`--scan-bytes-per-second`、`--duration 30s/5m/2h`。未指定 duration 则持续运行。命令行覆盖 JSON 同名设置；`--path` 默认选择 paths 模式，与显式 auto 冲突时会报错。

`scan` 可能需要数分钟完整覆盖大目录，不应放进每秒监控循环。其输出 `Complete=false` 或非零退出码不能当成完整结果。源码中 scan 的大小写字段保留 `Files/Bytes/Resident/Errors/Complete/Seconds`；daemon 状态字段为 snake_case。

## 范围和动作

| JSON 参数 | 默认值 | 人话说明 |
| --- | --- | --- |
| `mode` | `auto` | 自动发现范围，或 `paths` 指定范围 |
| `action` | `observe` | 观察不动缓存；`enforce` 才可以回收 |
| `paths` | `[]` | paths 模式的一组绝对路径。支持目录或普通文件，禁止根目录及系统目录；不跟随符号链接 |
| `auto_roots` | `[]` | auto 模式额外登记的目录，不取代自动发现；要严格限定范围请使用 paths |
| `exclude_paths` | `[]` | 排除路径及其子树；优先于发现与包含规则 |
| `exclude_names` | 见生成配置 | 按每级目录/文件的完整名字排除。默认含 WAL、global、事务/复制/共享内存相关 PG 目录；自定义数组会替换默认数组 |
| `cgroup_path` | 自动检测 | 本容器 memory cgroup；sidecar 应显式指定业务容器的只读挂载路径。必须是真实 cgroup 文件系统 |
| `state_file` | `/tmp/containercacheguard/status.json` | 状态输出和同名 `.lock` 文件；父目录必须属于当前 UID，权限 0700。不同受控范围使用不同目录 |

自动发现磁盘类型的挂载点（ext4/xfs/btrfs/overlay/f2fs），以及 `/data`、`/backup`、`/backups`、`/pgdata`、`/var/lib/postgresql`、`/var/lib/mysql`、`/app/data`、`/opt/data` 和 `PGDATA`。过滤系统目录、被排除路径、符号链接和不支持文件系统。自动模式不保证找到任意应用所有缓存文件；容器内部特殊路径可以通过 auto_roots 补充。可执行文件、共享库、静态库、非普通文件、短于一页的文件跳过。

父目录遍历不会跨到不同设备的嵌套挂载。自动发现可独立登记受支持挂载；paths 模式需要单独写出嵌套挂载路径。同设备的父子范围合并，不同设备分别登记。

## 什么时候回收，什么时候停

| JSON 参数 | 默认值 | 含义与调整影响 |
| --- | --- | --- |
| `cache_target_bytes` | `0` | 普通文件缓存的软目标，使用 `max(0,file-shmem)` 近似。0 表示从内存限额乘下面比例推导；不是硬上限 |
| `cache_target_ratio` | `0.25` | 有限 memory.max 的 25% 为缓存软目标。没有可见有限限额时，budget/pressure 模式必须显式配置字节目标 |
| `trigger` | `budget` | 文件缓存超过目标就允许试探；`pressure` 还要求内存用量超过 high 比例；`always` 为显式持续治理，仍受全部保护约束 |
| `memory_high_ratio` | `0.85` | pressure 触发模式的用量比例；不修改 memory.high |
| `memory_critical_ratio` | `0.95` | 达到可见限额95%时进入临界区，按 critical_mode 决定停止或有限继续；不是 OOM 保护保证 |
| `critical_mode` | `pause` | 默认临界时停止推测性回收。`bounded` 显式允许在临界区继续原有预算内的回收，适合经验证的低复用备份输出范围；仍遵守脏页、IO、业务延迟、自身开销和新增 OOM kill 保护，不自动提速、不写 cgroup 限额。一般业务目录先保留 pause |
| `max_dirty_bytes` | `134217728`（128MiB） | dirty+writeback 超过此值暂停回收。0 表示只允许该统计为0；不代表每个目标页严格干净 |
| `max_io_bytes_per_second` | `0` | 目标 cgroup 累计 io.stat 推算的读写流量保护阈值，0禁用；配置非零而接口缺失时停止动作 |
| `max_io_psi_avg10` | `10` | IO PSI some avg10 百分比超过10退让，0禁用；PSI 缺失显示 unknown，不伪装成0，但不会单独阻止全部运行 |
| `min_node_available_bytes` | `0` | 可选宿主 `/proc/meminfo` MemAvailable 下限，0禁用。它不是 kubelet memory.available；未知或低于阈值时停止动作 |
| `business_signal` | 空 | 可选业务信号 JSON 文件；配置后缺失/过期会停止动作。文件由监控系统写入，CCG 不执行外部命令 |
| `signal_max_age_seconds` | `10` | 业务信号允许的最大年龄；未来超过1秒的时间也拒绝 |
| `max_business_p99_ms` | `0` | 大于0时要求信号包含 p99_ms，超过就退让；0不按 p99 判断 |

业务信号例子（请写入真实当前时间并持续更新）：

```json
{"time":"2026-11-06T10:00:00Z","p99_ms":12.5,"pause":false}
```

同样负载下的生产阈值必须由测量确定。把所有保护阈值调得很宽，可以得到更大的回收数字，也可能掩盖业务损失；实验中调整的数值会单独记录。

持续写入与只读扫描需要分别配置。若正常写入的 dirty+writeback 已高于 max_dirty_bytes，工具会长期退让；此时不能声称已经控制缓存。先确认正常脏页范围、磁盘余量和写入时延，再为低复用输出路径单独调整阈值。min_file_age_seconds 仍跳过近期改写文件；一个一直增长的大文件可能长期无法回收，应考虑应用轮转或合作式接口。工具不会为了压低缓存主动 fsync 整个目录。

io.stat 在 device-mapper 等堆叠存储上可能同时列出逻辑盘和物理盘。本版流量保护累计可见设备，可能重复计算；它是保守退让信号，不能当作宿主物理磁盘吞吐。测试报告应选定同一底层设备另行比较。

## 速度、保护时间和自身预算

| JSON 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `interval_ms` | `1000` | cgroup/自监控及决策采样间隔，100..60000毫秒 |
| `log_interval_seconds` | `10` | 输出一条状态 JSON 的间隔，避免每个文件刷日志 |
| `warmup_seconds` | `60` | 启动学习期，不执行释放 |
| `protect_seconds` | `30` | 首次观察到一个驻留范围后至少等待多久，不能把到期等同于证明冷页 |
| `min_file_age_seconds` | `30` | 距离文件最后修改的最短时间；正在持续改写的文件会跳过 |
| `cooldown_seconds` | `60` | 再次驻留或无效释放后的保护/冷却时间 |
| `rescan_seconds` | `60` | 一轮发现结束后再次开始的最短周期；大目录会持续分批发现，不要求一分钟扫完 |
| `scan_bytes_per_second` | `268435456`（256MiB/s） | 驻留查询覆盖范围预算，不是磁盘读取字节；包含释放前后复查 |
| `reclaim_bytes_per_second` | `67108864`（64MiB/s） | 建议释放范围速率上限，允许最多约1秒额度突发 |
| `initial_reclaim_bytes_per_second` | `4194304`（4MiB/s） | 起始速率；健康时每30秒增加这一档，发现代价或无效则降低 |
| `region_bytes` | `4194304`（4MiB） | 单次文件范围；64KiB整数倍，64KiB..64MiB，适配4K/64K页大小；不会建议释放末尾不满页的部分 |
| `discover_entries_per_second` | `500` | 新目录条目发现预算；限额后有滞后，状态不能解释成完整即时目录 |
| `max_files` | `150000` | 文件索引上限，超出记录 inventory_overflow，不无限增内存 |
| `max_directories` | `20000` | 待遍历目录队列上限 |
| `max_regions` | `8192` | 候选、复查和保护区域记录上限；不是保存全库位图 |
| `max_operations_per_second` | `1000` | 区域处理次数预算；一次区域处理可能涉及多个系统调用，不是精确 syscall 计数 |
| `access_hints` | `true` | 有界目录级访问提示，帮助及时发现正在被扫描的大文件；false 仅轮询。提示不等于冷页判断 |
| `max_watches` | `1024` | inotify 目录 watch 上限，范围1..16384；不足时保留轮询并记录溢出 |
| `max_hint_files` | `2048` | 访问提示对应的文件队列上限，范围1..100000；不随业务事件无限增长 |
| `max_cpu_percent` | `5` | 自身进程上一个采样窗口超过单核5%时暂停工作；这是反馈保护，不是内核 CPU 硬限额 |
| `max_rss_bytes` | `134217728`（128MiB） | 自身 RSS 超过该值暂停扫描；同时设置更低的 Go 内存软目标。不是进程硬内存上限 |
| `worker_budget_ms` | `25` | 每秒工作时间预算；分最多5毫秒的小片执行。不允许释放时最多使用5毫秒/秒做慢速观察。阻塞系统调用不能保证严格按时返回 |
| `max_reappearance_ratio` | `0.25` | 释放后再次驻留量超过观察下降量25%时保护该范围，并降低回收速率 |
| `recheck_seconds` | `5` | 计划在释放后5秒起复查；预算不足或内核阻塞时可能延迟 |

扫描和回收速率不能小于一个 region；最大16GiB/s。要用1MiB/s建议速率，先把 region_bytes 和 initial_reclaim_bytes_per_second 都调小到不超过该值。参数校验失败会明确报错，不会偷偷忽略。

## 看懂结果

- `advice_bytes`：内核接受建议的文件范围累计字节，不等于已释放字节。
- `observed_net_reduction_bytes`：建议前后观察到的驻留净下降，线上可能混入自然回收/并发访问，不作严格因果归因。
- `observed_reappeared_bytes`：之后复查的驻留回升，可能是业务读取或预读，不是精确应用缺页数。
- `sampled_resident_bytes_cumulative`：累计采样结果，重复采样会重复计数，**绝不能当成当前总缓存**。
- `indexed_files`、`tracked_regions`、`inventory_overflow`：告诉你索引覆盖和预算是否不足。文件内容不会被守护进程读取来判断冷热。
- `degraded`：例如 PSI 缺失、没有 kubelet Summary、其它缓存只诊断。未知不等于健康。
- `worker_busy_seconds`：单工作队列执行延迟；超过5秒会停止派发新的建议，不启动替代工作线程制造堆积。

状态目录包含进程号、路径及资源指标，默认只允许相同 UID 访问。每个状态文件有进程锁；它不能检测两个不同容器使用不同状态文件却管理同一个 inode 的情况，部署时必须保证同一范围只有一个治理者。
