# 每日日志、30天保留与1GB自动清理

从0.2.0-rc.10开始，`run`启动后默认同时写控制台和每日文件日志，无需使用cron、logrotate或另装日志程序。`doctor`、`init-config`、`status`和`scan`仍保持原有命令输出格式，方便DBS解析。

## 默认怎么工作

- 一个日期一个文件：`containercacheguard-2026-11-10.log`，内容为UTF-8 JSON行。
- 最多保留包括当天在内的30个日历日。以文件名日期计算，不通过修改mtime延长保留期。
- 所有CCG受管日志文件合计最多1000000000字节，即1GB。达到容量边界时优先删日期最早的历史日志；因此不保证一定保留满30天。
- 若只剩当天文件仍将触顶，原地截断当天旧内容，再写`log_cleanup`事件和新记录。当天旧内容会丢失，文件名保持不变，不生成`.1`等分片，也不复制1GB的尾部。
- 写入前检查容量；额外预留少量清理事件空间，所以可能略早于上限清理。正常采样期间约每分钟检查一次过期文件；即使没有新的周期日志，也会进行维护。

`log_interval_seconds`仍控制完整状态日志的频率，默认10秒。还记录启动、停止和清理事件。错误会返回到控制台；日志通道仍可写时，退出事件同时记录错误原因。

**这1GB只管理本程序的每日文件日志。** Docker/Kubernetes另行收集的stdout/stderr日志由运行时或集群负责保留；本程序不会去删除它们。日志目录中的其它命名文件也不会为了满足1GB而被删除。

## 参数怎么填

| JSON参数 | 默认值 | 含义 |
| --- | --- | --- |
| `log_directory` | 空，自动推导 | 使用`state_file`同级的`logs/<状态文件名去扩展名>/`，例如`/tmp/containercacheguard/logs/status/`。也可填写专用、规范、绝对目录 |
| `log_keep_days` | `30` | 包括当天在内的保留天数，1..3650 |
| `log_max_total_bytes` | `1000000000` | 受管每日文件总字节上限，1MiB..1TiB；1GB是十进制10亿字节 |
| `log_timezone` | `Local` | 决定按哪一个时区切分日期。容器本地时区可能是UTC；需要中国日期时明确填`Asia/Shanghai`。时区数据已打包进静态程序 |
| `log_interval_seconds` | `10` | 完整状态快照输出间隔，单位秒；不改变指标采样频率 |

文件名按`log_timezone`切日；状态中的采样时间保留RFC3339时间及其原始时区偏移，方便与已有监控对齐。清理事件时间使用分文件时区。

例如把这些字段合入已有配置，不要用下面片段覆盖其它必需配置：

```json
{
  "log_directory": "/opt/ccg/state/logs/dbs",
  "log_keep_days": 30,
  "log_max_total_bytes": 1000000000,
  "log_timezone": "Asia/Shanghai",
  "log_interval_seconds": 10
}
```

更改后只重启CCG进程。目录及文件分别以0700、0600创建，必须属于运行CCG的UID；已有权限过宽的目录不会被静默接管。程序拒绝符号链接、硬链接、多写进程共用目录和同名FIFO等异常对象。日志目录自动排除在CCG治理范围外。

## 四个容器怎么放

| 实例 | 示例`log_directory` |
| --- | --- |
| dbs | `/opt/ccg/state/logs/dbs` |
| hisdbs | `/opt/ccg/state/logs/hisdbs` |
| dbsbackup | `/opt/ccg/state/logs/dbsbackup` |
| hisdbsbackup | `/opt/ccg/state/logs/hisdbsbackup` |

把状态/日志目录放到可写持久卷或现有专用宿主机挂载，并为实际UID准备权限。四个实例各用自己的目录；共享同一个宿主卷时也要分子目录。只放在容器可写层，重建容器后可能丢日志。不要放入PGDATA或某一次`pg_basebackup`的输出目录，以免混入数据库或备份内容。这里是接入示例，不表示已经替换94运行实例。

## 怎么查看与确认清理

```sh
# 容器内查看当日日志；日期要和log_timezone一致。
tail -n 20 /opt/ccg/state/logs/dbs/containercacheguard-2026-11-10.log
# 控制台日志仍可由原来的运行方式或kubectl logs查看。
```

状态JSON新增`logging`：报告目录、当前文件、保留天数、容量、受管总字节、删除文件数和当天截断次数。删除/截断次数自当前进程启动累计；重启会归零，文件仍追加到当天原文件。状态字节数是最近采样时的值，不要求与随后一次追加逐字节同步。

Prometheus提供`ccg_log_managed_bytes`、`ccg_log_files_deleted_total`、`ccg_log_current_file_truncations_total`。指标不包含日志路径标签。`log_cleanup`事件明确记录删除数量以及是否截断当天文件。

## 对业务资源的考虑与故障行为

日常追加短记录，不为每条日志调用fsync；断电时最近尚未落盘记录可能丢失。维护只检查本日志专用目录，不递归扫描PGDATA，最多读取8192个目录项。没有后台无界缓冲、日志压缩、大文件复制或逐文件详细扫描日志。

开始采样、日志维护和状态写入前先撤销旧回收许可；写入返回后也只有新鲜且保护允许的采样才能重新授权。日志写失败时CCG停止新动作并错误退出；由现有独立看护只重启CCG，不重启数据库。它不替代磁盘空间规划或集群日志保留策略。

本版本回归用稀疏文件验证真实1GB大小边界，以避免测试本身写入GB数据；这不是1GB物理写入性能测试。ENOSPC、每日切分、重启追加和危险文件拒绝也分别验证。现场长期IO成本需继续实测。
