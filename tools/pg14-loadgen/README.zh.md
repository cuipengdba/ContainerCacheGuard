# 94 上的 PostgreSQL 14 压力模拟辅助程序

`target94_loadgen.py` 只用于已授权的 **10.110.32.94** 测试节点，不属于 ContainerCacheGuard 常驻进程，也不能在只读源节点171运行。它调用目标机自带的 PG14 `psql` 与 `pgbench`，在 `clouddb.ccg_load` 下建立两张实验表，不修改业务表。部署真实数据后，先记录恢复时点的模式、关系、行数和尺寸，再启动该程序；实验模式会使94不再与来源备份逐字节一致。

示例（数值仅演示语法，**不是已经校准的171压力档位**）：

```sh
python3 target94_loadgen.py --role dbs --rate 100 --clients 4 --target94 \
  --inserts-per-tx 3 --updates-per-tx 4 --event-bytes 256 --update-bytes 1024 \
  --round-seconds 3600 --min-free-gib 64
```

| 参数 | 含义 |
| --- | --- |
| `--role dbs\|hisdbs` | 选择94上现有PG14二进制、Unix Socket目录和端口；当前路径是94的`/opt/PUC-784`，不能直接照搬其它节点。 |
| `--rate` | pgbench全局目标事务数/秒；实际完成数因泊松调度、业务延迟而波动。 |
| `--clients` | 并发连接数，默认4，允许1–32。 |
| `--round-seconds` | 每轮运行秒数，默认3600；轮间自动记录结果并继续。 |
| `--inserts-per-tx`、`--updates-per-tx` | 每事务实验行数，默认3和4；用来逼近171只读观测的插入/更新计数。 |
| `--event-bytes`、`--update-bytes` | 实验字段长度，影响数据增长、WAL和IO；先低档，凭实测调整。 |
| `--state-rows` | 更新表的键范围，默认10万；越小热点越集中。 |
| `--min-free-gib` | PGDATA文件系统可用容量低于此值时暂停新一轮，默认64GiB；保留进程并记录`paused_low_disk`。 |
| `--report-dir` | 每轮pgbench原始输出日志目录，默认`/var/tmp/ccg-loadgen`。 |
| `--once` | 只运行一轮，用于冒烟检查。 |
| `--target94` | 必须显式提供，且程序还检查94的PUC目录，减少误在其它节点启动的机会。 |

程序每轮输出 JSON，包括目标速率、退出码、日志路径以及PG统计计数器前后值。对照171时应看实际提交/插入/更新速率、`wal_bytes`、磁盘IO、业务响应时间及Kubernetes事件。此程序只模拟一部分写入统计，**不能重现171真实SQL、事务竞争、网络调用或逐表访问分布**。只有完整数据和同时段监控都到位后，才能把它作为CCG对照的一部分。

2026-11-08空HISDBS冒烟：1TPS、60秒、1客户端、每事务1插入1更新、16字节字段；pgbench返回0，实际72事务，实验表72行。该临时空库尚未迁入171业务数据，不能作为正式压力结论。
