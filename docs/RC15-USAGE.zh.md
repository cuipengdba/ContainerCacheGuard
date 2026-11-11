# rc.15：用数据比较改善与代价

本版增加离线统计和compare，不新增常驻扫描或网络服务；rc.14的安全读取、信号有效期和配置检查全部保留。真实PG14压力、完整备份恢复和48小时仍须另外验证。

## 先生成两份有明确窗口的报告

```sh
containercacheguard report --manifest /reports/baseline-sources.json \
  --from 2026-11-11T01:00:00+08:00 --until 2026-11-11T02:00:00+08:00 \
  --format json --output /reports/baseline.json
containercacheguard report --manifest /reports/candidate-sources.json \
  --from 2026-11-11T03:00:00+08:00 --until 2026-11-11T04:00:00+08:00 \
  --format json --output /reports/candidate.json
containercacheguard compare --baseline /reports/baseline.json \
  --candidate /reports/candidate.json --format markdown > /reports/comparison.md
```

源文件/输出文件须不同，**用shell重定向时切勿覆盖任何输入**。report仍支持SVG和Markdown；compare只输出标准输出，不访问数据库、cgroup或集群，不自动调度。两份报告须由rc.15产生schema_version=2，并明确from/until，单窗最长31天。旧日志可以重新统计，但缺少身份、就绪信号或连续指标就不能补出证据。只改JSON字段或手填正常数字不构成试验。

每份报告最多16实例、4096小时分组，输入JSON单份16MiB，拒绝符号链接、重复字段、未知字段、负值、非有限数、重复小时、计数不符和覆盖超窗。候选窗口必须在基线结束后，时长相同；时区可不同但按绝对时刻解释。不重叠也不代表自然满足等压，仍须手动控制试验。

## 新增的小时统计是什么意思

| 字段 | 单位与含义 |
| --- | --- |
| ordinary_file_byte_seconds | 字节×秒；普通文件页max(file-shmem,0)，相邻有效日志间保持前值的时间积分估算 |
| ordinary_file_covered_seconds | 上述积分实际衔接的秒数；均值=积分/覆盖秒，没数据不是0 |
| self_cpu_percent_seconds / self_cpu_covered_seconds | 单核CPU百分比×秒及有效覆盖秒，除得估算时间均值 |
| io_bytes_delta / io_covered_seconds | 同进程/运行/身份的有效io.stat累计字节差和对应秒数；是cgroup可见读写合计，不是磁盘延迟 |
| business_p99_peak_ms | 绑定来源且ready的业务p99样本中的最大值，**不是把多个p99合成整个试验的p99** |
| business_covered_seconds / node_covered_seconds | 两端样本都有效、就绪和可解释的业务/节点区间秒数；节点区间还要求同一UID |
| business_source_ids / node_ids | 有效样本出现的探针来源和节点UID；缺失不会编造 |

文件、CPU时间积分按本地小时切分；跨小时IO字节整体归到区间结束的小时，秒数按小时拆分，必须**先合计字节和秒再求速率**，不要用单小时IO字节/秒推断精确小时速率。超过max-gap、重启/run_id变化、cgroup变化、乱序、无效指标均不补算。只有一端有效不计覆盖。时间积分和覆盖均是端点衔接估计，不能证明间隔内持续健康，不能代表连续真实峰值或严格因果收益。试验必须固定相同日志频率与max-gap；不得通过放大max-gap人为提高覆盖。当前compare不自动核验两份报告的日志频率/max-gap完全一致，操作者须查看原报告元数据并另行审计。report不带from/until仍可供人阅读，但compare会要求明确窗口。

## compare先查证据，再查门槛

每个instance_id单独比较，不跨库平均掩盖问题。要求：基线全部observe、候选全部enforce；两侧实例一致、各自单一cgroup、同保护profile、同节点UID和同一业务探针来源；窗口内不能有缺保护输入、SUSPENDED/CRITICAL/STOPPED/未知状态或节点MemoryPressure。候选必须有有效允许区间和建议字节，基线建议字节须0。五项文件/CPU/IO/业务/节点覆盖都要达到门槛。旧匿名日志、没有业务探针、全程只观察或回收一直被暂停，都不能当成功。

结果有三种：

- `insufficient_evidence`：不满足可比较条件，逐实例说明缺项；不能仅看缓存下降。
- `threshold_exceeded`：可比较的遥测中至少一项超限，或有文件操作错误；缓存变低也不能掩盖尾延迟/IO恶化。
- `telemetry_thresholds_met`：**仅这些遥测门槛满足**；不等于完整业务验收，不证明CCG导致了改善，也不保证不驱逐。

总体结论取最保守结果：任一实例证据不足则总体不足，否则有超限则总体超限。正常输出JSON或Markdown；加`--require-thresholds`时，证据不足或超限在输出结果后返回非零，便于现有DBS/试验脚本检查。参数/文件损坏直接报错。不能把命令退出0（未加require）当成试验通过。

## 参数和示例门槛

以下默认是离线筛查起点，**不是已获批准的现场验收值**。必须按业务SLO和硬件确定，并在两轮开始前写定：

| 参数 | 默认 | 含义 |
| --- | ---: | --- |
| --baseline / --candidate | 必填 | 两份报告JSON普通文件 |
| --format | json | json或markdown |
| --min-coverage | 0.9 | 五类指标各自至少覆盖声明窗口90%，范围(0,1] |
| --min-file-improvement-percent | 20 | 文件页采样峰值与时间均值都至少下降20%，范围0..100 |
| --max-io-increase-percent | 10 | 可见cgroup IO均值增幅上限，范围0..10000 |
| --max-p99-increase-percent | 5 | 业务p99样本最大值相对增幅上限，范围0..10000 |
| --max-business-p99-ms | 100 | 同时遵守绝对业务阈值，必须正数；100ms只是示例 |
| --max-cpu-average-percent | 3 | 单个CCG的单核CPU时间均值上限，范围(0,100] |
| --max-rss-bytes | 134217728 | 单个CCG RSS采样峰值上限，正整数字节 |
| --require-thresholds | false | 遥测门槛未满足时返回非零 |

基线缓存为0无法证明改善，返回证据不足；IO/p99基线为0且候选变正时，相对增幅未定义，该项按超限处理，绝不打印Infinity/NaN或当0增长。正负变化都保留原始指标。报告中的建议量和区域复查下降不作为收益判据，仍需看真实文件页占用和代价。

## 四容器试验顺序

继续给DBS、HISDBS及两备份容器配置各自唯一instance_id、状态和日志目录，挂载方法见[rc.13四角色手册](RC13-USAGE.zh.md)。优先单库低复用备份输出；匹配数据快照/对象数量、业务请求分布、备份max-rate、存储和缓存起点，排除vmtouch/nocache并发。基线与候选使用同一节点/业务探针/profile，仅动作不同；先确认rc.14的doctor和signals。源PGDATA不因输出成功而放开。

compare不接管pg_basebackup，不采集完整备份时长、pg_verifybackup结果、隔离恢复、原始业务请求、K8s事件和未捕获的瞬时内存峰值。必须独立验证这些项目并保存中止/空窗，再开展联合负载和48小时。统计阈值满足不能解决匿名内存容量问题；视频和领导长期方案仍暂停。
