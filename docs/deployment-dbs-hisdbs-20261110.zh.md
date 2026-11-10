# DBS / HISDBS 首次双组件接入实测（2026-11-10）

本次在既有 Kubernetes/containerd 业务容器内接入 **ContainerResourceGuard v0.1.0-rc.2** 和 **ContainerCacheGuard 0.2.0-rc.9**。使用当日最新已发布候选版本，没有把 main 的未发布开发功能当成发布版。两容器已逐个重建，SQL 和业务 `/test` 恢复正常；CRG 的 CPU、内存、IO 写入后回读均成功。本页是部署短测记录，不是长期性能、全量数据校验或备份恢复验收。

用户最初提出统一百分比，后续最终改为：**DBS 以 13 GiB 内存反算统一比例，HISDBS 以 15 GiB 内存反算统一比例**。不再使用早先讨论的 30%、20% 或 15%。本次按二进制 GiB 解释运维中的 GB，1 GiB=1073741824 字节。没有采用此前备选的数据库内存调小方案；两库 shared_buffers 保持原来的 8960MB，maintenance_work_mem、autovacuum_work_mem、work_mem 和连接数均保持原值。

## 百分比与实际内核值

宿主机在线逻辑 CPU=28，MemTotal=66414546944 字节（61.85337 GiB）。计算公式：`percent = 目标GiB × 1073741824 / MemTotal × 100`。比例是各容器独立上限，不是资源预留，也不是两个容器共享一个总池。

| 参数 | DBS | HISDBS |
|---|---:|---:|
| CRG percent | 21.01744927021752 | 24.25090300409714 |
| 内存硬上限 | 13958643712 字节，13 GiB | 16106127360 字节，15 GiB |
| CPU 配额 | 5.88488 核 | 6.79025 核 |
| cpu.max 回读 | `588488 100000` | `679025 100000` |
| IO读 BPS | 808217396 | 932558534 |
| IO写 BPS | 950962158 | 1097264029 |
| IO读 IOPS | 7041 | 8124 |
| IO写 IOPS | 6890 | 7951 |
| 实际受控设备 | 253:0（数据挂载所在DM层） | 253:0（同一宿主设备） |

两个容器内存上限合计28 GiB，占宿主MemTotal约45.26835%；不是合计45.27%的资源保证。CPU/IO也分别使用同一percent。IO四个维度同时约束，具体吞吐由工作负载和最先触及的维度决定。

RC2 不自动测速。本次在宿主机独立目录 `/opt/container-guards171/calibration` 使用128MiB临时普通文件、direct同步IO、队列深度1、每阶段3秒完成标定；不测速裸盘，不打开数据库业务文件，不清全局缓存。该次现场基准：读3845459010 B/s、写4524631635 B/s、读33501 IOPS、写32787 IOPS。它是当时特定方法的观测值，不能称为整盘物理极限。设备身份profile仅保留在节点，换盘/换节点需重新核验。

## 持久部署与启动关系

每个容器使用独立宿主目录 `/opt/container-guards171/dbs` 或 `/opt/container-guards171/hisdbs`，只读挂载到容器 `/opt/guards`：

```text
/opt/guards/bin/crg
/opt/guards/bin/containercacheguard
/opt/guards/config/crg.json
/opt/guards/config/ccg.json
/opt/guards/config/io-profile.json
/opt/guards/scripts/supervisor.sh
/tmp/container-guards/crg/       本容器独立锁、原值日志与状态
/tmp/container-guards/ccg/       本容器独立CCG状态
/tmp/container-guards/log/       辅助组件日志，随容器生命周期
```

入口为 `/tini -- /bin/bash /opt/guards/scripts/supervisor.sh /root/start.sh`。原 `/root/start.sh` 保留，业务独立启动，CRG与CCG分别重试；辅助组件失败不能决定原业务退出码。业务结束后停止辅助循环并保留业务退出码。安装前完成 `bash -n`，并在宿主隔离入口测试辅助文件缺失时业务退出码7仍返回7。该短测不等价于全部信号/故障注入验收。

Deployment 使用单副本 **Recreate**，固定在原数据节点，避免滚动更新时新旧实例同时使用PGDATA。新增工具挂载没有改变原有 `/opt/postgresql` 数据挂载：DBS沿用 `/opt/PUC-280/dbspod/postgresql`，HISDBS沿用 `/opt/PUC-280/hisdbspod/postgresql`。没有删除、格式化或替换数据目录，没有创建任何备份容器，也没有初始化新库。Kubernetes原128Gi外层内存上限保留；实际子容器13/15Gi由CRG管理，不能只看Deployment声明值判断最终限制。

首次旧Pod没有preStop，逐个执行 `pg_ctl -D /opt/postgresql/pgdata -m fast -w -t 90 stop` 并等到 server stopped，再更新相应Deployment。新Pod加入60秒fast-stop的preStop，终止宽限120秒。原有业务探针不改成辅助组件探针。

完整配置、启动脚本和节点监控脚本见本页旁的 `deployment-20261110/`。该目录不含口令、SSH私钥、数据库连接凭据或原始Deployment环境变量。

## CacheGuard 参数与意义

| 参数 | 本次取值 | 说明 |
|---|---|---|
| mode / action | auto / enforce | 自动发现支持的数据目录；策略允许才释放文件页 |
| auto_roots | PGDATA和basebackup | 补充特殊目录；仍保留自动挂载发现 |
| cache_target_bytes | 2147483648 | 普通文件缓存软目标2GiB，不是总内存硬上限 |
| scan_bytes_per_second | 67108864 | 扫描预算64MiB/s |
| initial_reclaim / reclaim | 4194304 / 16777216 | 初始4MiB/s、最高16MiB/s的建议范围预算，不是实际释放量 |
| max_cpu_percent / worker_budget_ms | 3 / 10 | 限制CCG自身工作开销 |
| interval_ms / log_interval_seconds | 1000 / 10 | 状态每秒采样、日志限频 |
| max_dirty_bytes | 134217728 | dirty+writeback超过128MiB退让 |
| critical_mode / recovery_healthy_seconds | pause / 10 | 临界区暂停，连续健康10秒后恢复 |
| web_listen | 127.0.0.1:19891 / :19892 | 只读页面、metrics、healthz、readyz，未开放公网端口 |

完整默认WAL/global/事务目录排除规则保留，另外排除工具、程序、业务脚本、core目录和PG统计临时目录。auto不递归扫描根目录，不清tmpfs、shared_buffers、匿名内存或slab；auto发现范围、文件权限和执行位等仍可能限制覆盖。原DBS内置缓存清理保持，故本次不能把全部缓存下降单独归因于CCG。

## 本次检查和证据边界

- 发布包SHA256核对通过；二进制版本读取与预期一致。
- 两个容器CRG状态均为managed；CPU、memory.max、io.max实际回读与上表一致。
- 两个容器CCG实际为auto/enforce；healthz、readyz、metrics均HTTP200，`ccg_monitoring_ready 1`。
- DBS曾进入CONTROL，并发出了文件范围建议，局部复查观察到下降；HISDBS当时低于软目标而IDLE，不将IDLE描述成主动回收成功。
- 安装前后分别核对每个库的system identifier、数据库名称/OID、关系类型数量、namespace数量，8项比较均相同；shared_buffers也仍为8960MB。使用原持久目录，无新库初始化。没有执行全表行数/校验和扫描，不能把目录与目录项核对写成全量数据一致性证明。
- 新容器当时oom/oom_kill均0。监控如实记录安装窗口的短暂SQL/API不可用，不抹掉停机样本；恢复后的检查正常。长期负载与OOM风险仍需按小时报告继续观察。
- 本环境IO/内存PSI不可用，CCG明确降级报告；不补造为0。采样时完整inventory sweep尚未完成，不声称已治理所有文件。
- 机器可读脱敏快照见 `deployment-20261110/test-summary.json`。本次未做额外压测、完整备份/恢复和长期验收。

## 持续监控、图标与报告

节点systemd服务 `container-guards-monitor.service` 每60秒采样；开机自启、失败重启，CPU预算10%单核、内存256MiB。按日JSONL保留14天，仅轮转自己的监控文件。SQL为5秒statement_timeout的统计/健康查询，无业务表扫描。主机网络为hostNetwork共享网络，不能重复归属到每个容器；磁盘图分别展示253:0逻辑层与8:0物理层，不相加。

本地定时任务每小时通过**只能执行只读报告命令、无shell/PTY/端口转发**的专用SSH key取最近一小时样本，生成资源图表和中文汇报。私钥仅在操作端，不上传仓库。节点采样不依赖操作端在线，小时汇报由操作端任务调度。

图表包含：⚙CPU核数/节流、🧠总内存、📁普通文件缓存、📦匿名内存、🔗共享内存、💽磁盘读写、🌐主机网络、🏠宿主可用内存、🩺SQL探针。SQL探针耗时包含crictl/客户端启动成本，不能当成业务SQL p99。原始记录还含磁盘容量、refault、OOM和Guard计数；建议范围字节不是实际回收字节。缺失或超过180秒的采样标未知/异常，累计量按容器ID分段，安装窗口与稳定期分开分析。

## 复现、停用与回退

1. 先备份Deployment与配置，只读检查PGDATA路径、库标识、资源用量与控制文件权限；核对发布SHA256和架构。
2. 在目标节点独立目录用RC2显式标定并保存profile，分发到每个角色工具目录；使用本页附件的对应crg/ccg配置和supervisor。
3. 将宿主工具目录只读挂载为 `/opt/guards`；保留所有数据卷、业务环境、镜像和探针。先逐个正常停止旧库，再用单副本Recreate发布入口；每个角色SQL/API正常后再处理另一个。
4. 启用后核验 `crg status --config /opt/guards/config/crg.json`、直接回读cgroup三项、CCG readyz/metrics、SQL及业务/test；不能只凭进程存在或Pod Running判成功。
5. 停用CRG用同版本 `crg disable --config /opt/guards/config/crg.json`，确认自有原值恢复。只kill进程不会撤销内核限额。CCG由自己的辅助循环管理，需同时停止循环才不会重启。
6. 完整回退先协调停用，再逐个缩容到0并确认旧实例已停止，然后从节点私有 `/opt/container-guards171/rollback-20261110` 原始Deployment恢复，保留原数据卷，最后单副本启动；不要直接切回RollingUpdate让两实例并行访问同一PGDATA。回退清单需移除导出元数据并使用当前resourceVersion，不盲目apply陈旧完整对象。数据库参数本次未变，无需恢复调参。

节点私有回退目录保留原Deployment、db.ini、postgresql.conf/auto.conf和本次补丁。它们只是配置备份，不是额外备份容器或完整数据库备份，未上传公开仓库。
