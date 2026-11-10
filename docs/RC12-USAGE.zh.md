# rc.12：部署预检、扫描等待优化与离线小时统计

这版解决三个运维问题：保护缺什么、文件扫描为什么慢、回收有没有可观察收益。它继续使用已有mincore/fadvise机制，保留无Web、每日有界日志、静态双架构和保护缺项暂停；没有增加全机清缓存或修改宿主机的行为。

## 部署前先检查

```sh
/opt/ccg/bin/containercacheguard doctor --config /etc/ccg/dbs.json --require-ready
```

备份容器中的程序可能位于`/opt/ccg/containercacheguard`；实际路径按部署选择。`--require-ready`仅支持doctor：按配置采样间隔取得两个样本，建立CPU/IO/refault速率基线，再检查所配置保护的输入。缺项、cgroup错误或业务信号错误返回非零，同时尽可能输出JSON诊断。它不会进行文件内容扫描、释放缓存、创建常驻进程、启用PSI或关闭保护。

普通doctor保留单样本诊断，速率尚未建立时可成功输出报告；部署脚本若要阻止缺项上线，必须使用`--require-ready`。预检通过只表示该时刻启用保护的输入可用，不代表所有可选保护已启用、策略允许释放、目录全部可读或Kubernetes不会驱逐。

| 诊断代码 | 意思与处理 |
| --- | --- |
| cgroup_path_missing / cgroup_permission_denied / cgroup_resolution_failed | 路径、权限或真实cgroup定位错误；检查实际挂载，不用普通目录冒充 |
| io_psi_permission_denied | 目标IO PSI接口不能读取；先检查UID、挂载和访问策略 |
| io_psi_invalid | 接口有内容，但没有有效some avg10；不能按零压力解释 |
| cgroup_io_psi_missing | 容器可见宿主PSI接口存在，目标cgroup接口缺失；核对目标与内核能力 |
| psi_disabled_boot_argument | 可见启动参数明确psi=0/off，并且接口缺失；需要宿主维护流程处理 |
| psi_host_interface_unavailable | 两处接口不可用，原因仍可能是内核配置、默认关闭或挂载隐藏；到宿主只读核对 |
| host_psi_visibility_denied / io_psi_read_failed | 宿主接口不可见或临时读取错误，不能据此下内核关闭结论 |
| required_input_missing:字段名 | 配置所需输入缺失；保持原保护，修复采集 |

94在2026-11-10实际确认内核默认关闭PSI；本版没有自动修改它。`psi=1`及重启需要另外安排维护，代码升级不能替代。[内核PSI接口](https://docs.kernel.org/accounting/psi.html)说明宿主与cgroup接口；[rc.11现场记录](deployment-rc11-four-containers.zh.md)保留实际证据。无PSI兼容策略本版未实现，不会静默禁用该保护。

## 大量文件时看哪些字段

已有索引仍受max_files等预算约束，超过索引上限会轮换。新优化只针对已经完整走过文件范围、处于NextScan等待期的索引：跨工作时间片记住一轮中最早的唤醒时间，全部等待时暂时跳过索引遍历。新增文件或发现身份变化会作废等待缓存；发现、提示处理和候选复查仍按原预算进行。

它不会缓存永久文件身份，也没有扩大CPU/扫描预算。没有事件提示时，变化仍可能等到常规扫描才发现；缓存等待优化不承诺每个文件立即感知。

| counters新字段 | 含义 |
| --- | --- |
| file_schedule_visits / waiting_rescan_visits | 文件调度访问数、其中等待重扫的次数，累计计数，不是磁盘IOPS |
| inventory_sweep_in_progress / inventory_sweep_age_seconds | 当前目录发现是否进行及历时，不代表驻留查询已经完成 |
| last_inventory_sweep_seconds | 上轮目录发现耗时；首次未完成时为0，结合completed_inventory_sweeps解释 |
| file_scan_wait_seconds | 全索引等待时距最早重扫的时间，不表示整个进程睡眠 |
| recent_write_deferrals / changed_candidates / nonresident_candidates | 因近期改写推迟、身份变化丢弃或候选已不驻留的次数 |
| no_progress_backoffs | 多次无即时净下降后保留区域并延长等待的次数 |

连续无即时净下降时，区域冷却按cooldown_seconds的1、2、4、8倍增长，最多8倍；三次尝试后保留已有受限区域记录再等待，避免普通扫描立即把同一区域当新候选。发生即时净下降会重置该区域连续计数；再次驻留保护仍优先。异步写回可能稍后才见效，所以这只是保守退避，不是“内核拒绝释放”的确证。区域记录仍受max_regions限制，等待可能占用候选名额，需要结合覆盖与收益观察。

## 用已有日志生成小时统计

```sh
containercacheguard report --config /etc/ccg/dbs.json \
  --instance DBS --format markdown --output /reports/dbs-hourly.md

containercacheguard report --log-directory /var/log/containercacheguard \
  --instance HISDBS --from 2026-11-11T00:00:00+08:00 \
  --until 2026-11-12T00:00:00+08:00 \
  --format json --output /reports/hisdbs-hourly.json
```

report是静态二进制内的一次性命令，读取已有日志，不启动HTTP，不连接数据库，不采集cgroup，也不扫描PGDATA。它可以在有日志副本的Linux机器离线运行。report输出Markdown表格或JSON；没有内置PNG画图、定时器或推送服务，后续外部采集和图表程序可读取JSON。

| 参数 | 默认值及说明 |
| --- | --- |
| --config | 可选，仅用于取得默认日志目录；不执行run和cgroup预检 |
| --log-directory | 配置的日志目录；未给config则按默认状态目录推导。只选本层containercacheguard-日期.log，不递归 |
| --log-file | 可重复指定JSONL文件；不能与log-directory同时使用；按文件名排序，跨日应保持日期顺序 |
| --instance | 报表名称，默认“未命名实例”，最多256字节；不能替代cgroup身份 |
| --format | markdown，另可选json |
| --output | 默认标准输出；给出时以临时文件原子替换，输出目录须存在。不能覆盖输入路径或命名为受管每日日志 |
| --timezone | Asia/Shanghai，内置时区数据，最小镜像也可用 |
| --from / --until | RFC3339起止时间，含起点、不含终点；默认取输入记录范围 |
| --max-gap | 30s；100ms至1h，日志间隔超过该值不衔接估算。日志配置超过10秒时应根据实际间隔明确设置 |

普通文件、匿名和共享内存分开统计，未知值为null/未知；IO和自身CPU/RSS保留采样峰值。按cgroup版本/路径分别分组，不把不同容器拼成一条曲线。只统计同PID、同程序版本、同cgroup且无计数回退的连续差值；首条不追算之前工作量，计数归入后一条记录的小时。状态秒数按短间隔内保持前一条状态估算，跨小时分开；不是逐毫秒实测。

重复/乱序记录会计数并跳过；重启、长空窗、计数回退会标记未衔接间隔。没有记录的小时不凭空补成零或健康。损坏、截断、超过4MiB单行日志或特殊文件直接报错；每个输入按打开时文件大小读取，后续追加留待下一次统计。并发日志清理/截断可能使本次报错；正式归档统计应使用稳定副本。

输入最多1024文件、目录最多10000条目、最多4096小时/cgroup分组，超出需缩小时间窗。命令只报告CCG可见事实，不生成业务p99、备份成功、OOM/Evicted或“已解决问题”的结论；这些由独立验证记录提供。建议字节、复查下降、重新驻留必须分开解释。

## 四角色配置怎么用

[DBS](../deploy/rc12/dbs.json)、[HISDBS](../deploy/rc12/hisdbs.json)、[DBS备份](../deploy/rc12/dbsbackup.json)、[HIS备份](../deploy/rc12/hisdbsbackup.json)均默认observe、保留IO PSI保护和30天/1GB每日独立日志。源端示意路径/data/pgdata，输出示意路径/backup，必须替换为真实容器内路径并核对挂载、所有者和排除范围。软目标和速率是保守试验起点，**不是94压力验证通过的推荐值**。

| 检查 | 业务容器 | 备份容器 |
| --- | --- | --- |
| 目录 | 原PGDATA只读可访问即可，默认排除PG关键目录；先观察热点与匿名内存 | 备份输出专用卷；源PGDATA只读挂载用于监控不等于备份输出范围 |
| 连接 | CCG不连接PG | pg_basebackup用共享Socket或受控网络连接，限速由备份工具另设 |
| 权限 | 尽量使用数据所有者UID，核对mincore权限 | 独立UID及可写日志目录；无需特权容器 |
| 持久化 | 状态目录独立、日志单独持久挂载 | 程序固定版本只读挂载、日志专用持久卷，不混入物理备份 |
| 上线 | 严格预检、observe基线、业务指标达标后再评估enforce | 先验证输出目录的完整备份/校验/恢复；临界回收仍默认pause |

同节点同inode的共享文件页可能跨容器受影响；读路径相同不证明inode相同，输出端实例也不能默认代表源端cgroup指标。它们共用底层磁盘时仍会互相竞争。没有把本版配置自动部署到94；实际94部署方式、独立备份Pod、YsP缺口及回退见[四容器记录](FOUR-CONTAINERS.zh.md)。
