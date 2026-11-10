# 171 节点 DBS / HISDBS 双组件升级实测（2026-11-10）

## 最新百分比调整（2026-11-10，覆盖下方升级时参数）

用户随后明确指定DBS内存25%、CPU30%、IO40%；HISDBS内存35%、CPU30%、IO40。已按指定值生效：

| 参数 | DBS | HISDBS |
|---|---:|---:|
| cpu_percent | 30 | 30 |
| memory_percent | 25 | 35 |
| io_percent | 40 | 40 |
| CPU上限 / cpu.max | 8.4核 / `840000 100000` | 8.4核 / `840000 100000` |
| memory.max | 16603635712字节（15.46GiB） | 23245090816字节（21.65GiB） |
| 253:0 rbps / wbps | 1538183604 / 1809852654 | 1538183604 / 1809852654 |
| 253:0 riops / wiops | 13400 / 13114 | 13400 / 13114 |

本次仅备份并原子替换两份CRG配置，再让supervisor重新启动各自的CRG helper加载新值。没有重启数据库或容器；容器ID和`pg_postmaster_start_time()`前后相同，SQL/API正常，memory.events未增加OOM。DBS下调前检查了当前内存与新上限之间的余量，CRG自己的5%+16MiB余量、增长和事件检查保留；没有强制清全局缓存。HISDBS为同一版本、同一cgroup/journal内的上调，按owned原值规则应用。

首次自动验收脚本将无journal上下文的`doctor`保守预览值当作HISDBS上调目标，导致等待超时；实际限制已成功生效。随后独立读取运行Agent的计划和三项cgroup回读，均与上表一致，并修正验收比较逻辑。完整脱敏前后核对见[retune-summary.json](deployment-upgrade-20261110/retune-summary.json)。

两库内存上限合计约37.11GiB，占宿主60%，低于51.85GiB父级上限；仍是上限而非预留。CCG的auto/enforce、2GiB软目标和既有保护配置保持不变。当前完整配置文件`dbs.crg.json` / `hisdbs.crg.json`已更新，`test-summary.json`保留原升级测试快照，`retune-summary.json`记录本次改值。监控图表和每小时自动报告同步使用新值；小时窗口内若发生限额变化，会列出采样观察到的变更时间和前后值，累计量不重置。

查看进程时使用二进制实际名称：`ps -ef | grep -E '[c]rg|[c]ontainercacheguard'`，对应 `/opt/guards/bin/crg run --config /opt/guards/config/crg.json` 和 `/opt/guards/bin/containercacheguard run --config /opt/guards/config/ccg.json`。Deployment名称为`dbs-11099` / `hisdbs-11102`，namespace为`puc`；应核对当前容器ID。

以下为本次版本升级时的历史记录，参数已由上表取代。


本次在既有 DBS、HISDBS 容器中，将 CRG 从 `v0.1.0-rc.2` 升级到 **`v0.1.0-rc.3-preview.1`**，CCG 从 `0.2.0-rc.9` 升级到 **`0.2.0-rc.11`**。两者均为已发布的预览/候选版本，不作为稳定版或长期业务验收结论。沿用原 PostgreSQL 数据挂载，逐个正常停库、Recreate 重建并恢复 SQL/API；没有新建备份容器、初始化新库或修改数据库内存参数。

**本页取代同日首次接入记录中的旧版本和统一 percent 配置。** 原 13/15GiB 方案和旧 CCG HTTP 探针只作为历史事实保留。

## 升级时的百分比与内核回读（历史）

版本升级时用户提出 CPU35%、内存45%、IO60%，并允许按现场情况调整。宿主机为28个逻辑CPU、MemTotal=66414546944字节（61.85337GiB）；`kubepods.slice` 父级内存上限55677128704字节（51.85337GiB）。两库同时45%会合计55.67GiB，超过父级上限。因此 **DBS保留45%，HISDBS按现场约2.4GiB用量设25%**；两库内存上限合计43.30GiB，为父组留下约8.55GiB空间。此余量仍与父组内其他占用共享，不是预留保证。

| 项目 | DBS | HISDBS |
|---|---:|---:|
| cpu_percent | 35 | 35 |
| memory_percent | 45 | 25 |
| io_percent | 60 | 60 |
| CPU上限 | 9.8核 | 9.8核 |
| cpu.max | `980000 100000` | `980000 100000` |
| memory.max | 29886545920字节（27.83402GiB） | 16603635712字节（15.46334GiB） |
| IO设备 | 253:0 | 253:0 |
| rbps / wbps | 2307275406 / 2714778981 | 2307275406 / 2714778981 |
| riops / wiops | 20100 / 19672 | 20100 / 19672 |
| CRG状态 | managed，三项applied回读通过 | managed，三项applied回读通过 |

计算口径：CPU是宿主在线逻辑CPU数量的百分比；内存是宿主MemTotal的百分比，最终字节值以页对齐后的内核回读为准；IO是设备标定profile四个维度的百分比。不是父组内存的45%/25%，也不是CPU当前利用率；不混用旧 `percent` 字段。

本次自动IO选择报告 `reused`：稳定设备身份匹配，复用此前独立临时文件的profile，**没有再执行磁盘测速**。旧基准为读3845459010 B/s、写4524631635 B/s、读33501 IOPS、写32787 IOPS，方法为direct同步、QD1、顺序1MiB/随机4KiB、128MiB独立文件、每阶段3秒。60%是此方法的观测基准比例，不是磁盘物理极限或吞吐预留。两个容器共享设备，各自60%合计可能竞争；本次没有新增压测验证最大吞吐。

## 包来源与校验

使用官方发布附件，未把main临时编译产物当作发布包：

- [CRG v0.1.0-rc.3-preview.1](https://github.com/cuipengdba/ContainerResourceGuard/releases/tag/v0.1.0-rc.3-preview.1)：linux-amd64压缩包SHA256 `65ad8e81d85249e7d4f96d216eaed8cc09b6e03b41d3b65342da9d62b6c9c45f`；解包二进制SHA256 `f07973272313db26d359797dda4665145f7dd6490bac2a55f9084bd367faf361`。source revision `7638ee9f871287d1056e267034e0c4e46903b66e`，package revision `73bf2aaf38dfd92757cd58e610d4e49debc56b1f`。
- [CCG v0.2.0-rc.11](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.11)：linux-amd64二进制SHA256 `7ab4ae462235c4419ab967189a1d27bb4c515928b63e93502df841882f44617f`，revision `b5b65db8ead1d5b4304047835c4150e8c96028de`。

校验依据为发布附件SHA256SUMS及GitHub release asset digest；新容器内版本输出和实际二进制hash再次一致。

## 部署方法与持久目录

这是 Kubernetes/containerd、namespace `puc`、单副本 Deployment `dbs-11099` / `hisdbs-11102` 的现场升级。没有额外sidecar或备份容器；两个程序与数据库业务同处原容器内。

1. 保存原Deployment JSON、旧程序/配置、业务配置 `db.ini` 到节点私有目录 `/opt/container-guards171/upgrade-20261110/rollback`。这只是回退配置，不冒充数据库全量备份。
2. 下载上述发布包，核对SHA256；将本目录提供的完整JSON配置与两个可执行程序放入各角色的新版本目录。`bin`内分别命名 `crg`、`containercacheguard`，`scripts/supervisor.sh`沿用已验证的业务入口。
3. 新程序与配置：`/opt/container-guards171/upgrade-20261110/{dbs,hisdbs}/{bin,config,scripts}`，挂载到容器 `/opt/guards`，只读。每个容器私有运行目录 `/opt/container-guards171/runtime/{dbs,hisdbs}` 挂载到 `/opt/guards-runtime`，可写。将已验证设备profile复制到对应运行目录 `io-profile.json`；换设备须重新确认身份，不能盲目复用。
4. 两库分别执行 `pg_ctl -D /opt/postgresql/pgdata -m fast -w -t 90 stop`，明确等待 `server stopped`；对DBS应用补丁并等到SQL/API恢复，再升级HISDBS。补丁文件见本目录 `dbs.upgrade-patch.json` / `hisdbs.upgrade-patch.json`。
5. 使用 `kubectl patch deployment -n puc <deployment> --type=strategic --patch-file <对应补丁>` 更新工具挂载。保留单副本 **Recreate**、原节点、原业务镜像/环境和数据卷；不同时启动两份实例访问PGDATA。
6. 通过新容器内 `version`、cgroup实际值、CRG/CCG状态、SQL和业务POST `/test` 检查，再进入持续监控。身份与目录项比较通过，但没有进行全表校验和或备份恢复演练。

原数据路径不变：DBS宿主 `/opt/PUC-280/dbspod/postgresql/`、HISDBS宿主 `/opt/PUC-280/hisdbspod/postgresql`，仍挂载到 `/opt/postgresql`，PGDATA仍为 `/opt/postgresql/pgdata`。工具挂载独立于数据卷。数据库 `shared_buffers=8960MB`、`maintenance_work_mem=2GB`、`autovacuum_work_mem=1GB`、`work_mem=2293kB`、`max_connections=1000` 保持原值。

原入口保持 `/tini -- /bin/bash /opt/guards/scripts/supervisor.sh /root/start.sh`。业务与两个helper分别启动，helper退出重试不会替代原业务退出码。原120秒terminationGracePeriod及60秒fast-stop preStop保留。旧、新cgroup分开启动与记录，不进行在线原值journal迁移。本目录补丁用于已具备首次接入入口的171节点，不是任意新环境的一键安装器。

## CCG auto参数与现场调整

| 参数 | 两容器取值 | 含义 |
|---|---|---|
| mode / action | auto / enforce | 自动发现支持的挂载和目录，策略允许才执行 |
| auto_roots | PGDATA、basebackup | 补充自动发现，不递归扫描整个根文件系统 |
| cache_target_bytes | 2147483648 | 普通文件缓存软目标2GiB，不是总内存硬上限 |
| warmup_seconds | 60 | 启动后先观察 |
| scan_bytes_per_second | 67108864 | 扫描预算64MiB/s |
| initial_reclaim / reclaim | 4194304 / 16777216 | 初始4MiB/s、最高16MiB/s建议范围预算 |
| max_cpu_percent / max_rss_bytes | 3 / 134217728 | CCG自身CPU/RSS预算 |
| worker_budget_ms | 10 | 每次工作时间预算 |
| max_dirty_bytes | 134217728 | dirty+writeback超过128MiB时退让 |
| memory_high / critical_ratio | 0.85 / 0.95 | 总内存压力保护；critical_mode=pause |
| recovery_healthy_seconds | 10 | 退让后连续健康恢复窗口 |
| max_io_psi_avg10 | **0** | 本机PSI不可用，明确关闭这一可选门槛 |
| log_directory | /opt/guards-runtime/ccg-logs | 角色独立的持久每日日志 |
| log_keep_days / log_max_total_bytes | 30 / 1000000000 | 只轮转CCG自己的每日文件 |
| log_timezone | Asia/Shanghai | 日志按北京时间命名 |

rc.11默认 `max_io_psi_avg10=10`，该指标缺失时会 `SUSPENDED/protection_metrics_unavailable:io_psi_avg10`。本次首次启动已观察到该状态；随后将该参数设0，仅重启CCG helper使配置生效，数据库未因此再次重启。最终 `protection.ready=true`，不伪造PSI=0。OOM、脏页、总内存压力和自身资源保护保留。原默认关闭的业务p99、refault速率、宿主可用内存和额外IO速率门槛仍为关闭，状态中的disabled_guards完整列明；不能说全部保护项均启用。

标准WAL/global/事务目录排除项保留，另外排除程序、业务工具、core及运行日志目录，完整值见JSON。CCG不主动回收anon、shared_buffers、tmpfs/shmem或slab。带执行位的普通文件、索引容量与hint溢出会限制实际覆盖，不声称全PGDATA已完成治理。发生新的OOM时本版本仍可能持续 `CRITICAL/new_oom_kill`，需要人工评估，不能承诺它会自动清除该保护状态。

**rc.11取消内置HTTP服务**，没有Web、healthz/readyz或Prometheus端口。实测进程没有TCP监听，`web_service_enabled=false`；监控直接读取JSON状态与日志，不沿用rc.9的19891/19892探针。

## 日志、图标、监控与小时报告

- CRG每日文件：容器 `/opt/guards-runtime/crg-logs/crg-YYYY-MM-DD.log`；CCG每日文件：`/opt/guards-runtime/ccg-logs/containercacheguard-YYYY-MM-DD.log`。映射到节点角色运行目录；两者各自最多30天、受管文件总量1000000000字节。均已产生非空日志。旧supervisor的 `/tmp/container-guards/log` 辅助输出仍随容器生命周期，不属于这套每日文件轮转上限。
- 状态：`/tmp/container-guards/crg/status.json`、`/tmp/container-guards/ccg/status.json`；每容器独立，不跨容器共享控制锁/原值journal。CRG每15秒检查，相同状态约5分钟才落盘，不能将180秒未改mtime误报为进程失效；监控同时每分钟直接读实际cgroup限制。
- 节点 `container-guards-monitor.service` 已确认active/enabled，每60秒采样，CPUQuota=10%单核、MemoryMax=256MiB；此次只升级采集器补充API耗时、memory.peak、Guard状态年龄，保留全部历史JSONL与原14天保留规则。
- 🧠总内存、📁普通文件缓存、🔗shmem、📦anon、⚙️CPU、💽IO、🩺SQL、🏠主机内存、⚡主机CPU、🌐网络、🖴物理磁盘共11张本地图表。图表不跨容器ID连线；253:0逻辑IO与8:0物理IO分开，不重复相加；hostNetwork流量归属宿主机。
- 本地 `refresh-monitor.py` 通过专用强制只读命令SSH密钥取报告；生成 `hourly-latest.json`、`hourly-analysis.json`、`hourly-summary.txt`、`资源监控.html`。私钥和凭据不提交GitHub。小时自动任务已更新到本次版本和最终百分比，仅检查/汇报，不自动部署、重启、调参或压测。
- 累计量按容器ID分段，CCG工作计数按PID分段；计划停机参考 `deployment-events.json`。未采集值写未知；样本超过180秒告警，CRG状态超过360秒另报过期，窗口不足一小时明确说明。现场服务器时钟比报告客户端约慢99秒（含传输耗时），所有采样窗口与停机事件均按服务器时钟对齐；此次未修改系统时钟。

## 本次测试结果与边界

升级前后分别比较两库的system identifier、数据库名称/OID、关系分类数量、namespace数量，8项均相同，SQL与POST `/test` 返回正常，新容器memory.events的oom/oom_kill均0。此检查不等价于全表数据校验或备份恢复验收。旧DBS在本次升级前的16:05附近发生过1次OOM kill；旧ID累计oom_kill=1和当时SQL失败保留，不把新ID归零写成“历史从未OOM”。

停机窗口、版本hash、内核实际值、CCG完整工作计数/退让原因、禁用保护项及稳定期SQL/API样本数，均见旁目录 `test-summary.json`。DBS已观察到CONTROL和非零文件建议/局部复查下降，HISDBS低于目标而IDLE。`advice_bytes`是累计建议范围，不是成功释放的字节；`observed_net_reduction_bytes`是局部复查观测，不是数据库总内存的净减少量。完整扫描尚未验收，已观察到inventory/hint溢出，原业务清缓存也仍存在，不能把全部内存变化归因于CCG。

本次只做发布包校验、顺序冷升级与短期冒烟检查；没有新增压测、全表行数/校验和扫描、回退恢复演练或一小时稳定期验收。后续按小时持续报告实际表现。

## 回退方法

保留 `/opt/container-guards171/upgrade-20261110/rollback` 的旧版本、完整Deployment私有快照与业务配置。本次未修改PGDATA或数据库参数。若需回退，先保存当前监控证据，逐库fast-stop并等待退出，按原Recreate方式将工具hostPath恢复到 `/opt/container-guards171/{dbs,hisdbs}`，再检查SQL/API；不要在原PGDATA上同时启动新旧实例。旧限额DBS13GiB曾触发过OOM，回退前应重新评估资源值，不能将旧上限直接视为安全推荐。
