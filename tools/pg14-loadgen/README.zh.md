# 94 上的 PostgreSQL 14 压力模拟辅助程序

`target94_loadgen.py` 只用于已授权的 **10.110.32.94** 测试节点，不属于 ContainerCacheGuard 常驻进程，也不能在只读源节点171运行。它调用目标机自带的 PG14 `psql` 与 `pgbench`，在单独的`ccg_loadtest`数据库中建立实验模式和两张表，不改动已迁移的`clouddb`业务模式、表或行。物理PGDATA仍会因新增测试库而变化；先记录恢复时点的模式、关系、行数和尺寸，再启动压测。

94上当前DBS标定档位（171的统计总量近似；实际TPS和WAL仍以每轮输出为准）：

```sh
python3 target94_loadgen.py --role dbs --rate 110 --clients 4 --target94 \
  --inserts-per-tx 3 --updates-per-tx 4 --event-bytes 2048 --update-bytes 4096 \
  --payload-mode random --round-seconds 300 --min-free-gib 64 \
  --min-mem-available-gib 5 --max-test-db-gib 12
```

| 参数 | 含义 |
| --- | --- |
| `--role dbs\|hisdbs` | 选择94上现有PG14二进制、Unix Socket目录和端口；当前路径是94的`/opt/PUC-784`，不能直接照搬其它节点。 |
| `--rate` | pgbench全局目标事务数/秒，允许0.1–1000，可写`0.5`模拟低频事务；实际完成数因泊松调度、业务延迟而波动。 |
| `--clients` | 并发连接数，默认4，允许1–32。 |
| `--round-seconds` | 每轮运行秒数，默认3600；轮间自动记录结果并继续。 |
| `--inserts-per-tx`、`--updates-per-tx` | 每事务实验行数，默认3和4；用来逼近171只读观测的插入/更新计数。 |
| `--event-bytes`、`--update-bytes` | 实验字段长度，影响数据增长、WAL和IO；先低档，凭实测调整。 |
| `--state-rows` | 更新表的键范围，默认10万；越小热点越集中。 |
| `--payload-mode compressible\|random` | 默认可压缩重复文本；`random`用仅在测试库启用的`pgcrypto`生成行内随机文本，以便把WAL量调到接近源端。随机模式每行字段最多6000字节。 |
| `--min-free-gib` | PGDATA文件系统可用容量低于此值时暂停新一轮，默认64GiB；保留进程并记录`paused_low_disk`。 |
| `--min-mem-available-gib` | 宿主`MemAvailable`低于此值时停止当前pgbench轮次并暂停，默认5GiB。暂停时段不算达标压力时间。 |
| `--max-test-db-gib` | 测试数据库超过该体积时，**仅在两轮pgbench之间**清空可丢弃的`ccg_loadtest.ccg_load.events`并重置其序列，默认12GiB；不操作`clouddb`。若增长来自其它表而清空无效，就暂停并报告`paused_test_db_growth`，不无限扩张。轮转会添加低频TRUNCATE，不能声称完全重现原业务。 |
| `--report-dir` | 每轮pgbench原始输出日志目录，默认`/var/tmp/ccg-loadgen`。 |
| `--once` | 只运行一轮，用于冒烟检查。 |
| `--target94` | 必须显式提供，且程序还检查94的PUC目录，减少误在其它节点启动的机会。 |

程序每轮输出 JSON，包括目标速率、退出码、日志路径以及PG统计计数器前后值。对照171时应把`ccg_loadtest`与`clouddb`的事务增量分别列出，再看合计负载、`wal_bytes`、磁盘IO、业务响应时间及Kubernetes事件。此程序只模拟一部分写入统计，**不能重现171真实SQL、事务竞争、网络调用或逐表访问分布**。只有完整数据和同时段监控都到位后，才能把它作为CCG对照的一部分。

2026-11-08修改前曾在空HISDBS的`clouddb.ccg_load`完成1TPS、60秒冒烟：pgbench返回0、实际72事务。该空库后来作为切换前回退点保留，冒烟结果不是当前独立测试库版本的正式压力结论；新版本须在真实数据上重新验证。

## 94 的 2026-11-09 分档结果与自停说明

在获批把94的DBS/HISDBS `shared_buffers` 分别调为6144/4096MB后、**无备份**时，20TPS单库180秒及60TPS+HIS0.5TPS双库300秒均完成，实际DBS20.13/59.37TPS，两库轻量SQL探针窗口失败0，节点`MemAvailable`最低约9.40/9.21GiB。110TPS+HIS0.5TPS目标300秒时，节点可用内存降至7GiB以下，DBS约220秒、HIS约190秒主动停止；随后一次持续110TPS重试也因内存保护被停止。两次均不能写成连续等压通过。分钟指标显示同时出现DBS匿名内存约2.56GiB尖峰，而普通文件缓存下降；这类内存不能由文件页释放工具处理。

随后60TPS/HIS0.5TPS持续尝试在12:42因可用内存降至约6.04GiB由止损守护停止。进程快照和PG日志按容器PID对应：单个`pucbdm`查询后端匿名RSS约2.36GiB，查询包含1,431个SELECT分支；这次无备份，CCG仍为观察模式。12:50左右改为20TPS/HIS0.5TPS低档持续基线，7GiB自动暂停新轮次，5GiB止损并监测SQL失败/PG重启。低档持续运行的结果尚待观察，不能说已模拟171的同等压力，更不能说CCG解决了匿名内存峰值。

13:12同一查询ID再次在94产生约2.36GiB后端匿名RSS，5GiB止损停下了20TPS低档；第一轮300秒曾完整运行，仍不能称长稳。13:15左右改成7GiB自暂停和4GiB硬止损，SQL探针失败或PG重启同样停压。自暂停后程序仍可在内存恢复时开始下一轮，但实际TPS与暂停占空比必须报告，不能用systemd的`active`代替持续有效负载。

13:39–15:38与DBS独立备份Pod全备/校验并发的窗口里，DBS测试库实际平均仅3.482提交/s，原业务库17.371提交/s；HIS测试库0.134/s、原业务库0.611/s。虽然DBS全备、原生校验及同卷隔离恢复成功，且两库各475次轻量SQL探针失败0，这个压力仍远低于171 DBS约105TPS中位数。必须把自暂停后的**窗口平均TPS**与配置的`--rate 20`分开列示，不能把成功备份误读为等压安全验收。

94此后以60TPS+0.5TPS做较长观察时使用`--min-mem-available-gib 7`，另有目标94专用守护在SQL探针失败、PostgreSQL内部重启或`MemAvailable<6.5GiB`时停止两套模拟程序。**systemd单元active只代表生成器进程存在**；每轮还要查`target_tps`、实际事务数、`paused_low_memory`和真实运行秒数。暂停和被守护停止的时间必须从“达到171同压力”的统计中剔除。这个60TPS档低于171的DBS约105TPS中位数，不可称同强度复现。
