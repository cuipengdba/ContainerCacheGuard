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

通用选项：`--config`、`--mode auto|paths`、`--action observe|enforce`、可重复的 `--path`、`--cgroup-path`、`--state-file`、`--cache-target-bytes`、`--reclaim-bytes-per-second`、`--scan-bytes-per-second`、`--web-listen`、`--web-token-file`、`--duration 30s/5m/2h`。未指定 duration 则持续运行。命令行覆盖 JSON 同名设置；`--path` 默认选择 paths 模式，与显式 auto 冲突时会报错。

`scan` 可能需要数分钟完整覆盖大目录，不应放进每秒监控循环。其输出 `Complete=false` 或非零退出码不能当成完整结果。源码中 scan 的大小写字段保留 `Files/Bytes/Resident/Errors/Complete/Seconds`；daemon 状态字段为 snake_case。

## 范围和动作

| JSON 参数 | 默认值 | 人话说明 |
| --- | --- | --- |
| `mode` | `auto` | 自动发现范围，或 `paths` 指定范围 |
| `action` | `observe` | 观察不动缓存；`enforce` 才可以回收 |
| `paths` | `[]` | paths 模式的一组绝对路径。支持目录或普通文件，禁止根目录及系统目录；不跟随符号链接 |
| `auto_roots` | `[]` | auto 模式额外登记的目录，不取代自动发现；要严格限定范围请使用 paths |
| `exclude_paths` | `[]` | 排除路径及其子树；优先于发现与包含规则 |
| `exclude_names` | 见生成配置 | 按每级目录/文件的完整名字排除。数据库源端默认保护 WAL、global、事务/复制/共享内存相关 PG 目录；自定义数组会整体替换默认数组。备份输出卷应独立配置，是否纳入成品副本的 WAL 必须依据写入保护及校验实测决定 |
| `cgroup_path` | 自动检测 | 本容器 memory cgroup；sidecar 应显式指定业务容器的只读挂载路径。必须是真实 cgroup 文件系统 |
| `state_file` | `/tmp/containercacheguard/status.json` | 状态输出和同名 `.lock` 文件；父目录必须属于当前 UID，权限 0700。不同受控范围使用不同目录 |
| `web_listen` | 空，关闭 | 内置只读 Web 的 TCP 监听地址，例如 `127.0.0.1:9898`。端口须可用；非回环地址必须设置 token 文件 |
| `web_token_file` | 空 | HTTP Basic 的密码文件绝对路径，用户名固定 `ccg`；内容去掉首尾空白后为 16..4096 字节。启动时读取一次，改动需重启。远程访问还要 TLS 或安全隧道，Basic 本身不加密 |
| `web_history_samples` | `3600` | 页面最近历史样本上限，范围60..21600；每秒采样时默认约1小时。环形数组有界，重启后历史清零；长期图表另用持久采集 |

自动发现磁盘类型的挂载点（ext4/xfs/btrfs/overlay/f2fs），以及 `/data`、`/backup`、`/backups`、`/pgdata`、`/var/lib/postgresql`、`/var/lib/mysql`、`/app/data`、`/opt/data` 和 `PGDATA`。过滤系统目录、被排除路径、符号链接和不支持文件系统。自动模式不保证找到任意应用所有缓存文件；容器内部特殊路径可以通过 auto_roots 补充。可执行文件、共享库、静态库、非普通文件、短于一页的文件跳过。

显式 `paths` 范围**不会仅凭执行权限位跳过文件**：实测某PG14发行镜像把普通关系数据文件设为`0700`，把执行位当二进制判据会漏掉大部分数据库文件。广范围 `auto` 模式仍跳过带执行位的文件；两种模式都排除`.so`/`.a`及系统目录，若目录包含真正可执行程序，应通过`exclude_paths`排除。设置`paths`前必须确认整个范围是业务数据或备份输出，不能拿根目录试验。

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
| `max_file_refaults_per_second` | `0` | cgroup v2 中每秒 `workingset_refault_file` 增量的退让阈值，0禁用；配置非零但指标不可用时停止动作。它反映此前被逐出文件页的再次访问，**不能单独证明由本工具造成**。先测基线和业务峰值，再设阈值；它是滞后保护，不保证业务不受影响 |
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
| `rescan_seconds` | `60` | 一轮**完整目录发现结束后**再次开始的最短间隔，也控制已索引文件完成一轮页驻留扫描后的等待间隔；百万文件目录的一轮可能持续很久，目录间隔从完成时计算，避免结束后立即重扫。调小会更快发现同一文件中新驻留的页，但增加扫描CPU；新文件优先依赖访问提示，溢出时仍需补扫 |
| `scan_bytes_per_second` | `268435456`（256MiB/s） | 驻留查询覆盖范围预算，不是磁盘读取字节；约 75% 留给顺序文件扫描、25% 留给候选页的释放前后复查。它不限制备份的实际磁盘吞吐；该项仍需配合 `pg_basebackup --max-rate` |
| `reclaim_bytes_per_second` | `67108864`（64MiB/s） | 建议释放范围速率上限，允许最多约1秒额度突发 |
| `initial_reclaim_bytes_per_second` | `4194304`（4MiB/s） | 起始速率；健康时每30秒增加这一档，发现代价或无效则降低 |
| `region_bytes` | `4194304`（4MiB） | 单次文件范围；64KiB整数倍，64KiB..64MiB，适配4K/64K页大小；不会建议释放末尾不满页的部分 |
| `discover_entries_per_second` | `500` | 发现总速率预算；inotify可用且启用时，25%预留给事件发现，75%用于普通遍历，其余情况全部用于普通遍历。低于每秒1条的分配允许积攒到1条后执行；总速率受限，小额度下两路可各有1条突发。预留额度不借给普通遍历，完整补扫可能变慢 |
| `max_files` | `150000` | 文件工作索引上限；超过后轮换旧索引并累计 `index_rotations`，不无限增内存。轮换不保证所有文件在同一时刻被采样 |
| `max_directories` | `20000` | 待遍历目录队列上限 |
| `max_regions` | `8192` | 候选、复查和保护区域记录上限；不是保存全库位图 |
| `max_operations_per_second` | `1000` | 区域处理次数预算；一次区域处理可能涉及多个系统调用，不是精确 syscall 计数 |
| `access_hints` | `true` | 有界目录级访问提示，帮助及时发现正在被扫描的大文件；false 仅轮询。提示不等于冷页判断 |
| `max_watches` | `1024` | inotify 目录 watch 上限，范围1..16384；不足时保留轮询并记录溢出 |
| `max_hint_files` | `2048` | 范围1..100000。限制已索引的访问提示文件队列，也分别限制待发现路径队列；两队列合计最多2倍该值，不是共同上限。每条还占用路径字符串及索引内存，不能按指针大小估算总成本 |
| `max_cpu_percent` | `5` | 自身进程上一个采样窗口超过单核5%时暂停工作；这是反馈保护，不是内核 CPU 硬限额 |
| `max_rss_bytes` | `134217728`（128MiB） | 自身 RSS 超过该值暂停扫描；同时设置更低的 Go 内存软目标。不是进程硬内存上限 |
| `worker_budget_ms` | `25` | 每秒工作时间预算；分最多5毫秒的小片执行。显式`observe`使用所配预算来测量大目录覆盖，仍受自身CPU/RSS保护；`enforce`被安全门拦住时最多使用5毫秒/秒做慢速观察。阻塞系统调用不能保证严格按时返回 |
| `max_reappearance_ratio` | `0.25` | 释放后再次驻留量超过观察下降量25%时保护该范围，并降低回收速率 |
| `recheck_seconds` | `5` | 计划在释放后5秒起复查；预算不足或内核阻塞时可能延迟 |

扫描和回收速率不能小于一个 region；最大16GiB/s。要用1MiB/s建议速率，先把 region_bytes 和 initial_reclaim_bytes_per_second 都调小到不超过该值。参数校验失败会明确报错，不会偷偷忽略。

## 实验配置与默认配置不同

[源端实验例子](../deploy/lab-source.example.json)及[输出端实验例子](../deploy/lab-output.example.json)记录了真实数据对照使用的性能参数，路径改为通用示意，交付时动作改为 observe。它们是高吞吐实验配置，**不是推荐直接复制到生产的默认值**：实验源/输出容器分别有12GiB/6GiB限额，IO PSI不可用而禁用了该保护；源端使用100毫秒/秒工作预算和25%的自身CPU采样退让阈值；输出端最后一轮使用200毫秒/秒、40%采样退让阈值、2GiB/秒查询覆盖及192→256MiB/秒建议额度。阈值不是实际CPU占用，实际成本以报告为准。

使用时替换路径、确认UID及cgroup，按业务需要启用IO/节点/业务保护，再显式选择enforce。sidecar还需设置目标业务cgroup；默认自动检测只会找到自身。不要把脚本生成例子中省略的字段解释成已禁用，它们仍继承程序默认值。

当前真实规模验证使用cgroup v2；v1分支读取内存用量及缓存统计，但没有据此完成v1性能/故障验收。新增OOM kill的保护依赖v2 memory.events，不能把v1中未采集的OOM计数当作“零OOM证据”。

v2读取可见祖先的有限额与对应`memory.current`，记录占用比例最高的父组，并将本容器、父组两种比例都用于高水位/临界退让。`limit_bytes`仍表示可见范围内最严限额，`current_bytes`只表示本容器，不能直接相除后当父组占用比例；应看`pressured_ancestor_current_bytes / pressured_ancestor_limit_bytes`。若cgroup命名空间隐藏了祖先，或者Kubelet另有节点驱逐阈值，这些本地数字仍不保证安全，需同时看节点可用内存及实际驱逐事件。

## 看懂结果

- `advice_bytes`：内核接受建议的文件范围累计字节，不等于已释放字节。
- `observed_net_reduction_bytes`：建议前后观察到的驻留净下降，线上可能混入自然回收/并发访问，不作严格因果归因。
- `observed_reappeared_bytes`：之后复查的驻留回升，可能是业务读取或预读，不是精确应用缺页数。
- `file_refaults`、`file_refaults_per_second`：分别是所监控 cgroup 的累计文件页 refault 和相邻采样差分速率；可能由业务、备份或内核回收触发，不能单凭它归因于 CCG。`file_refaults_available`表示内核字段存在，`file_refault_rate_valid`表示本次差分可用。首次样本、采集失败后的下一样本、字段缺失或计数回退时速率未知；启用refault阈值后会暂停新释放，直到有效基线恢复。
- `sampled_resident_bytes_cumulative`：累计采样结果，重复采样会重复计数，**绝不能当成当前总缓存**。
- `indexed_files`、`index_rotations`、`completed_inventory_sweeps`：分别是当前工作索引大小、索引轮换次数和完整目录遍历次数。索引轮换次数上升表明文件数超过工作索引；完整遍历次数为0时不能声称覆盖整棵目录。`inventory_overflow` 仍记录目录队列或区域等其它预算溢出。文件内容不会被守护进程读取来判断冷热。
- `pending_hint_paths`：已收到事件但等待发现额度的路径数；`hint_discovery_retries`是从队列重新尝试发现的累计次数，不保证成功；`hint_queue_dropped`累计队列已满或到期丢弃数。队列去重，等待有效期至少60秒，仍以普通补扫兜底。
- `diagnostics`：只解释当前可观测构成，不改变策略。`valid=false`时不可据此判断健康；`other_charged_bytes`是当前计费减普通文件页后的近似剩余，含匿名/共享/内核等，不能理解为另一类可直接清空的缓存；`file_target_excess_bytes`是超过文件软目标的估算值，不是承诺释放量。
- `sample_interval_ms`：采样周期，网页用它识别长空窗；采集错误及空窗曲线断线，不以零值填补。
- `degraded`：例如 PSI 缺失、没有 kubelet Summary、其它缓存只诊断。未知不等于健康。
- `worker_busy_seconds`：单工作队列执行延迟；超过5秒会停止派发新的建议，不启动替代工作线程制造堆积。

状态目录包含进程号、路径及资源指标，默认只允许相同 UID 访问。每个状态文件有进程锁；它不能检测两个不同容器使用不同状态文件却管理同一个 inode 的情况，部署时必须保证同一范围只有一个治理者。

Web 页面和 `/api/v1/status`、`/api/v1/history` 均为只读，不提供开关回收、修改配置或执行系统命令的接口。状态包含路径及 cgroup 标识；默认只绑定回环地址。服务端限制并发请求数，设有读取/写入超时；`web_history_samples` 限制历史内存。页面的“普通文件缓存”按 `max(0,file-shmem)` 估算，`active_file`/`inactive_file` 是内核 LRU 分类，不等于逐文件的精确冷热判定。
