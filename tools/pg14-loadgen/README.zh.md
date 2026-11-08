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
