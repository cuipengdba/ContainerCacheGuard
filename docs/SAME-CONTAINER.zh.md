# 当前架构：数据库、备份和CCG在同一个容器

2026-11-17，需求C039。本文取代原先“DBS、HISDBS各拆一个备份容器”的推荐方案。它是新的部署约定，**不是服务器已经调整完成的记录**。本轮仅修改规划与文档，不连接171或94，不停服务、不搬数据、不删除历史备份容器。

## 一、最终放在哪里

```text
原有DBS Pod                         原有HISDBS Pod
└─ dbs容器                         └─ hisdbs容器
   ├─ 原DBS管理程序                    ├─ 原DBS管理程序
   ├─ PostgreSQL 14                    ├─ PostgreSQL 14
   ├─ 原备份/校验/同步进程               ├─ 原备份/校验/同步进程
   └─ 一个CCG常驻进程                   └─ 一个CCG常驻进程
      ├─ PGDATA路径策略                    ├─ PGDATA路径策略
      ├─ 备份输出路径策略                   ├─ 备份输出路径策略
      └─ 私有状态/每日日志                  └─ 私有状态/每日日志

可选的memory.reclaim能力：
两个CCG ── 受限Unix Socket ── 一个节点reclaim-agent
                              └─ 仅登记dbs、hisdbs两个实际叶子cgroup
```

“同容器”包括数据库进程和备份客户端，**不是同一Pod内再放一个备份sidecar**。继续使用现有DBS调度和PostgreSQL 14备份工具，不要求增加dbsbackup/hisdbsbackup服务、镜像或YsP条目。CCG不替代备份工具，不打包PG客户端，不接管数据库PID 1。

节点agent只是可选的内核回收权限执行器，不承载备份、不存放备份数据。关闭`kernel_reclaim`时常态文件后端无需它；启用rc.17已有内核后端时，仍需要它的宿主PID/cgroup视图和受限写权限。**本次没有把“节点agent”改成已实现的容器内自执行模式**，也不向业务容器自动授予宿主cgroup写权限。

## 二、哪些资源会合在一起

按通常未另外委派子cgroup的容器布局，PostgreSQL、DBS、pg_basebackup、校验/同步和CCG共同计入一个容器cgroup。源读取缓存、输出缓存、日志缓存和数据库匿名/共享内存共同占用容器预算；CCG本身也需要资源余量。实际运行时是否额外分组仍要预检，不能仅凭进程在同一容器就跳过核对。

| 控制方法 | 同容器下的实际含义 |
| --- | --- |
| pg_basebackup --max-rate | 限制备份传输速率，继续由现有DBS传递；不是全盘IO硬上限。PG14使用stream收集WAL时，此参数不限制该WAL流 |
| 容器io.max/现有CRG限制 | 作用于该cgroup的数据库和备份等总IO，不能声称只限制备份 |
| 文件后端mincore/fadvise | 可按明确文件路径选择范围，但共享同一文件页的其他读者也可能受影响 |
| memory.reclaim | 作用于整个登记容器cgroup，不识别“这是备份页还是数据库热点页”，路径/WAL排除不能保护这次内核回收 |
| 每路径软目标 | 是策略与驻留采样目标，不能变成Linux对每个目录独立的内存硬限额 |

同容器减少跨容器挂载和身份映射问题，**不会自动降低磁盘读写量或消除页缓存竞争**。使用Unix Socket也只改变连接通路，不能据此认定磁盘IO或页缓存下降。参见[PG14备份参数](https://www.postgresql.org/docs/14/app-pgbasebackup.html)、[cgroup2内存记账与回收](https://docs.kernel.org/admin-guide/cgroup-v2.html)。

Kubelet的memory.available口径可能已扣除inactive_file；容器file下降不能直接换算成驱逐余量改善。验收必须单独看真实节点/allocatable信号、OOM及Evicted事件。[Kubernetes驱逐口径](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/)。

## 三、目录和挂载

下面是容器内部路径示例，不表示现场路径已核实；PGDATA、输出和状态路径均可配置。

| 用途 | 示例 | 规则 |
| --- | --- | --- |
| 数据库目录 | /opt/postgresql/pgdata | 原数据卷；不因接入CCG改变数据库权限 |
| 备份目录 | /backup | 原备份卷或同磁盘上的独立目录；不能放进PGDATA，不因CCG接入改变备份格式 |
| 配置 | /etc/ccg/dbs.json 或 hisdbs.json | 每容器一份，建议只读挂载 |
| 状态 | /opt/ccg/state/dbs/status.json | 每实例独立，父目录0700、归属实际CCG UID |
| 日志 | /opt/ccg/state/dbs/logs | 每天一个文件，默认30天/1GB，不放入PGDATA、某次备份或tmpfs |
| 可选信号/Socket | /run/ccg-signals、/run/containercacheguard | 仅按权限共享监控与IPC；agent不需要挂载业务数据卷 |

这里PGDATA指源数据库目录，pg_basebackup的`-D/--pgdata`指备份输出，二者不能混用。同一容器中的进程直接使用相同卷路径，不再需要数据库容器与备份容器之间共享PGDATA。两个数据库容器的备份可以位于同一宿主卷的不同子目录，但不能共用同一个备份实例输出、状态文件或日志目录。卷分开不等于底层物理磁盘分开。

PGDATA如果有外部表空间，须核实表空间真实路径并单独授权。不要为覆盖表空间而自动跟随所有符号链接；重复挂载/硬链接按实际文件身份去重，避免把重复查询称为更多覆盖。备份输出与源路径重叠、角色冲突或共享inode的策略冲突，规划中的rc.18将拒绝或采用更保守策略。

## 四、rc.17现在可以怎样接入

先在每个原数据库容器内放一个对应架构的静态CCG，由现有进程管理器看护。单独运行`run`，不接管数据库启动；没有进程管理器时，启动包装方式需要单独核对退出与信号转发，不能把随手加一个后台`&`当成完成部署。CCG失败不应触发数据库重启。

```sh
# 以下是在各自容器内手工核对的示例，本轮不执行。
# 先从低复用输出观察；将/backup替换为实际路径。
containercacheguard init-config --mode paths --path /backup > /etc/ccg/dbs.json
containercacheguard doctor --config /etc/ccg/dbs.json --require-ready
containercacheguard run --config /etc/ccg/dbs.json --action observe
```

生成配置前需确保父目录可写且文件未占用，不盲目覆盖既有配置。生成后、运行doctor/run前，手工把state_file和log_directory设为表中每实例自己的持久路径，补齐信号与保护，再逐项检查。HISDBS使用自己的配置、状态与日志路径。以原有配置项可指定源与输出多个路径，但rc.17仍共用一套文件策略与预算，**没有实现rc.18规划中的每路径角色策略、任务阶段识别和生产schema2采集器**。上述仅观察输出，不意味着已治理PGDATA。实际UID/mincore权限、cgroup、排除规则和PSI/业务/IO信号都要经过doctor及小范围验证；无PSI时严格策略可能保持暂停。

不要在同一个容器内同时启动“源端CCG”和“备份端CCG”来假装获得两份隔离预算。当前部署约定是一容器一个CCG，下一版在单实例内部调度两类路径。旧vmtouch/nocache在observe阶段可保留用于观测现状；进入可归因的enforce试验时，同一文件范围只允许一个回收执行权威。DBS没有关闭开关时需显式适配并验证退出码，不能把旧命令替换为静默成功的空壳。

## 五、memory.reclaim如何调整

rc.17的固定注册列表可以只登记DBS、HISDBS两个实际叶子cgroup，具体配置见[rc.17手册](RC17-USAGE.zh.md)。容器重建、UID或cgroup身份变化后重新核对；node-agent的固定节点锁和持久journal继续保留。

“优先处理备份”改为**文件范围调度优先**。由于备份与数据库在同一cgroup，不能把内核回收请求标成“仅清备份”。内核事件仍按整个真实cgroup去重，备份开始、结束或任务ID变化不能重置一次尝试的额度；UNKNOWN、OOM历史及持续恢复条件照旧。默认关闭，保护信号完整且明确接受整个容器范围才考虑灰度。

## 六、旧四容器环境如何过渡

1. 先清点实际容器、DBS调度、正在运行的备份/同步/校验、输出位置和CCG版本。GitHub旧记录不是当前现场盘点。
2. 保存原配置和完整备份清单，确认不会有两个调度同时备份同一个库。由DBS在原数据库容器内执行备份，工具、凭据、空间与权限先做预检。
3. 只有在另行安排的受控迁移窗口，才调整旧备份容器调度与YsP显示。未完成备份、校验、恢复及回退核对前，不删旧输出、持久卷或服务。
4. 每个数据库容器先运行一个observe实例，确认合并计费、实际目录覆盖和业务基线。满足条件后再做enforce对照，旧清理避免重叠。
5. 如有内核执行器，登记两个业务叶子目标并保留历史journal；旧在途调用或UNKNOWN未解除时，不通过删记录、换目录或重建容器绕过去重。

这些是未来实施步骤，不是本轮执行清单。171仍禁止修改/测试；94后续验证以专门安排为准，不因本文发布自动启动负载或替换进程。

## 七、当前事实与下一版

| 分类 | 结论 |
| --- | --- |
| 已发布 | rc.17支持常驻文件后端和默认关闭的受限内核后端，节点注册不要求一定有四个目标 |
| 当前设计变更 | DBS、HISDBS各自数据库/备份/CCG同容器，不再新增备份容器 |
| 待开发 | rc.18统一预算下的路径角色、备份阶段、真实保护信号接入和低开销日志 |
| 待验证 | 同容器PG14实际规模、业务/备份重叠、无PSI真实盘替代、恢复及48小时；历史四容器结果不能冒充新架构验收 |

下一版任务拆分、参数草案和验收设计见[rc.18规划](RC18-PLAN.zh.md)。
