# rc.16：先找准缓存在哪里，再有界加速

这一版解决两个具体问题：扫描了很多字节却不知道哪些大文件没覆盖；接近内存上限时仍按初始低速处理。它不能把所有内存都当文件缓存，也不能保证回收一定快于业务分配。

## 怎样理解这次现场证据

用户提供的北京时间2026-11-12 06:14—07:13结果保存在[计划](RC16-PLAN.zh.md)。164.64GiB是累计查询范围，重复查询和前后复查都可能在内；1.484MiB也是累计驻留观察，不能直接作完整文件覆盖率。760KiB建议/区域下降不能解释10.905GiB普通文件缓存；这需要核对扫描对象与记账对象。DBS历史OOM暂停、释放0时，slab回落2.25GiB不能记为工具效果。

代码确认旧auto会跳过带执行位的普通文件，paths不会仅因执行位跳过；**尚未证明现场大文件具有执行位，也未证明只扫描了小日志。** 权限、实际PGDATA、表空间、排除项、挂载和其他已删除但仍打开的文件，都可能影响可见范围。原始日志取得后应逐项核对，不能先定结论。

## 第一步：核对实际数据目录

1. 在数据库正常连接中只读执行`SHOW data_directory;`，与容器内`PGDATA`比较；后者只是线索，不保证数据库采用它。
2. 检查数据目录和父目录的读取/执行权限。以数据文件所有者UID运行最容易得到可信的mincore信息；不能把无权限当成文件不驻留。
3. 对源容器和备份容器分别检查同一个实际文件的设备、inode、大小、属主和挂载。共享同一个内核inode可观察同一页缓存；不同复制品不共享。即使页缓存相同，其cgroup计费也不能由路径或inode直接推出。
4. 表空间链接不会被跟随。需要将实际磁盘目标以真实路径挂载并单独授权，不能为了覆盖率关闭符号链接保护。默认WAL、事务状态等目录仍排除。

`coverage_audit.requested_roots`是启动时显式请求目录的检查结果，时间见`requested_roots_checked_at`，不能当成每秒重新检查；`roots`是已打开的根元数据，各自带`checked_at`，`pgdata_environment_hint`仅为启动环境线索。嵌套挂载/设备变化、overlay复制层和跨容器计费都要与真实数据库容器的指标对照。根元数据不能覆盖每个子文件的完整挂载审计。

## 第二步：扩大需要的文件范围

优先使用明确的paths范围。如果需要auto，同时已确认数据文件带执行位，可以在配置中增加以下字段（路径换成真实路径）：

```json
{
  "mode": "auto",
  "action": "observe",
  "auto_roots": ["/data/postgresql/data"],
  "executable_data_paths": ["/data/postgresql/data/base"],
  "extended_recheck_seconds": [30, 120]
}
```

这只是配置覆盖片段，其余默认值仍生效；先`check-config`，再`doctor`和observe观察。`auto_roots`仍是自动发现的补充，**不是唯一范围限制**；需要只处理指定目录时用paths。执行位例外必须位于显式paths/auto_roots内，不自动增加扫描根；不绕过目录权限、文件属主/mincore可见性、文件系统、符号链接、排除项和`.so`/`.a`文件名规则。不能对应用程序目录笼统授权。

入库后的扫描、访问提示查询及候选建议前均重新检查文件资格，执行位/属主变化会使旧版本失效；不是仅在启动时检查一次。

```sh
containercacheguard check-config --config /etc/ccg/config.json
containercacheguard doctor --config /etc/ccg/config.json --require-ready
containercacheguard run --config /etc/ccg/config.json
```

新轮询以区域为单位在已索引文件之间轮转，大文件和小文件都能获得访问机会。目录/文件/候选/提示索引仍有上限；这不是对无限目录树的完整覆盖保证。

## 如何看coverage_audit

`groups`按实际检测根和文件名类型分组。`pg_relation_name`只表示文件名类似PG关系文件，不是内容或热度识别。组数达到上限后合并到overflow。

| 字段 | 人话解释与限制 |
| --- | --- |
| discovery_metadata_operations / discovery_logical_bytes_cumulative | 目录发现时取得元数据的次数及普通文件逻辑大小累计值，可能重复，不是读盘量 |
| open_operations / opened_logical_bytes_cumulative | 成功打开并取得元数据的操作数及普通文件逻辑大小累计值；没有读取这些全部数据 |
| skipped_operations / skip_reasons | 为什么跳过：execute_bit_auto、permission_denied、symlink_rejected、cross_device、directory_limit、candidate_limit等 |
| skipped_known_logical_bytes_cumulative | 已知大小的被跳过普通文件逻辑字节累计值；目录大小不充当其整棵子树大小 |
| skipped_unknown_size_operations | 在取得大小前就被排除或打开失败；这部分字节未知，不能填0声称覆盖 |
| initial_query_bytes_cumulative | 普通轮询与访问提示带来的初始驻留查询范围之和 |
| window_new_query_bytes_cumulative / window_repeat_query_bytes_cumulative | 有界历史内按设备/inode/版本/偏移/长度区分的新查询与重复；历史淘汰后旧区域会重新计新，不是运行期全量唯一字节 |
| recheck_query_bytes_cumulative / immediate_after_query_bytes_cumulative | 复查与建议后立即查询的范围；不要再把它们当新覆盖 |
| observed_resident_bytes/pages_cumulative | 每次查询观察到的驻留量累计值，重复计量，不能作当前全量缓存大小 |
| policy_eligible_resident_bytes/pages_cumulative | 通过当前范围/文件稳定时间/驻留等策略检查的观察量，可能重复；不是内核确认的冷页或干净页 |
| advice_range_bytes | 向内核提交的建议范围，不是实际回收字节 |
| immediate_region_decrease_bytes | 同一区域建议前后立即观察到的正向下降累计值，仍非全容器因果收益 |
| delayed_region_increase_bytes | 延迟复查中相邻观察的正向增加，关联候选；不能等同所有业务refault |

全容器普通文件指标用`file-shmem`，与文件范围观察分开看。anon、shmem、slab始终仅诊断。缓存被其他进程再次使用、内核自然回收和并发写入都可能影响观察。

`confirmed_cold_pages`和`confirmed_clean_pages`保留为null：当前普通mincore/fadvise后端不能确认逐页热度或干净状态，不用策略合格计数冒充这两项。

## 候选与分时复查

`recent_candidate_events`保留有界环，按sequence排序。关联键为`run_id + candidate_id`，并带路径、设备/inode、范围、尝试次数、复查阶段和实际时间。创建、策略合格、建议、立即结果、延迟复查、重新驻留保护、重试、无进展退避及终态可追溯。测量失败的驻留值是null；0才表示已测量到0。

默认先沿用`recheck_seconds`，`extended_recheck_seconds`默认为空；配置`[30,120]`会保留同一候选到建议后30/120秒再查。早于已经完成时点的额外值不会倒退。线程晚运行会记录实际晚到时间，不能补造准时样本。建议重试会以最近一次建议重设时点；修改/替换文件会终止旧候选。发生无收益或显著重新驻留时，旧退避/保护优先，不为了跑完所有时点再次强刷。

`candidate_outcomes`是各状态发生次数，包含非终态，不能相加当成候选个数。事件环覆盖次数见`candidate_events_overwritten`；不是永久逐文件审计档案。候选过多和额外复查占用`max_regions`，可能降低新增候选吞吐。暂停新建议时，已发建议的只读复查仍可在原安全调度条件允许下运行；严重保护阻止整个工作线程时复查也会延迟。

## 第三步：受保护的压力加速

`pressure_boost=false`为默认。显式开启时需`protection_profile=strict`、`critical_mode=bounded`，以及来源绑定的有效节点/业务信号、正数p99/IO/refault/PSI保护和至少256MiB节点安全余量。信号格式沿用[rc.13手册](RC13-USAGE.zh.md)，缺失PSI的strict环境不会因本版自动放宽。

只有现有策略和恢复门都允许时才加速：自身或可见父级到达`memory_high_ratio`，或有效kubelet可用内存进入“停止线之上+预警带”时，将建议速率提升至已配置的`reclaim_bytes_per_second`。这时可优先于文件缓存软目标处理仍驻留的合格范围；不是要求把容器文件页归零。离开压力带后回到初始速率，恢复原渐增机制；区域冷却和无收益退避继续优先。

**节点已经MemoryPressure或触及停止线、新OOM、脏页/写回过高、业务暂停/超时、IO/PSI/refault过高、指标未知、人工暂停或自身超预算，仍会阻止新建议。** 压力加速不清除本次运行已观察的OOM或重新设基线。本版没有新增跨进程持久OOM锁，重启仍沿用旧版启动基线语义；必须保留旧日志，不能把重启后允许当成历史OOM已解决的证据。CPU、容器内存和IO配额均由外部现有系统管理，本版不改它们。本轮原生压力机制回归基于cgroup v2；v1及缺少必需指标的内核不在本版压力增强已实测范围。

| 新参数 | 默认/有效范围 | 用途 |
| --- | --- | --- |
| executable_data_paths | []，最多64项 | 仅放开明确数据路径的auto执行位过滤 |
| pressure_boost | false | 显式压力加速；不构成独立回收许可 |
| pressure_boost_node_headroom_bytes | 1073741824，0..1PiB | 相对于真实节点停止线的额外预警带；0不扩大节点预警带，自身/父级高水位仍有效 |
| max_audit_groups | 64，2..256 | 根/类型分组、根诊断记录上限；超限合并/计数 |
| max_scan_history | 16384，1..65536 | 窗口查询去重历史；evictions表明完整唯一覆盖未知 |
| max_candidate_events | 32，1..256 | 最近候选事件环上限 |
| extended_recheck_seconds | []，最多8项 | 严格递增1..3600秒，均相对最近建议时间 |

原有`scan_bytes_per_second`、`reclaim_bytes_per_second`、操作数、CPU/RSS、保护时长和工作预算继续有效。不要同时盲目拉满所有预算；如果根本没有合格候选，拉高速率不会产生收益。小事件环、合理日志间隔和有界历史可限制观测成本；更大统计会增加JSON编码和日志IO。

## 核对短暂停顿

每次采样决策变化会立即写`event=guard_transition`，包含实际时间、前后决策、OOM累计、恢复停止/恢复次数、各保护指标与信号有效性，不受周期状态日志一秒节流。它仍只能捕获采样到的变化；默认1秒采样不能证明毫秒级连续健康。若需100ms短窗口核查，应先评估日志/采样开销，再临时调整`interval_ms`，不要把日志周期改成分钟后据此判定“全程允许”。

## 测试如何算通过

原生CI的隔离实验创建约1.125GiB已知低复用普通文件（包含0700文件），先确认至少1GiB驻留；observe不发建议，再enforce记录至少1GiB范围下降及多个复查时点。并行真实PG14 SQL和本地测试API探针记录耗时/错误；记录可见cgroup IO、CPU/RSS、OOM累计和内容摘要。CCG为切换observe/enforce配置分别启动，数据库不重启、缓存不归零，两阶段前后核对同一可见cgroup身份和OOM原始累计值；这不是各数据库独立容器的完整OOM历史审计。测试API只存在CI脚本，CCG不提供Web服务。文件在测持续性之前不被哈希重读，避免把校验自身造成的回温当成业务行为。

热点mmap、父级压力、脏页/业务/IO否决、历史OOM、目录/提示/统计溢出另有回归；部分策略输入由明确测试fixture提供，不是生产故障注入。mincore只是驻留快照，低读盘和文件未修改不能证明没有内存访问；映射热点的测试不表示工具能提前识别所有热页。详见[mincore文档](https://www.man7.org/linux/man-pages/man2/mincore.2.html)。fadvise是建议，脏页可能触发写回且不能保证立即释放，见[内核接口手册](https://www.man7.org/linux/man-pages/man2/posix_fadvise.2.html)。页缓存计费和内存限制的层级规则见[内核cgroup v2文档](https://docs.kernel.org/admin-guide/cgroup-v2.html)。

隔离实验通过仍需要DBS/HISDBS真实等压、备份完成、SQL/API尾延迟、恢复校验、K8s事件和长时稳定性验收。不得用重启归零、全局drop_caches、slab自然回落或单次建议量代替收益，不能承诺零驱逐。
