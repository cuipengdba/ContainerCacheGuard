# DBS/HISDBS 与独立备份容器：部署和 Web 使用

本页给运维人员一条可检查的接入路线。ContainerCacheGuard（下称 CCG）是常驻进程，不替代 PostgreSQL、DBS 或 `pg_basebackup`。当前94迁移与验证目标为 **PostgreSQL 14.19**；下列配置仍是接入设计，尚未表示四容器已在94完成部署与验收。

## 为什么是四个容器

| 容器 | 业务进程 | 读写的数据 | CCG 管的文件范围 | CCG 读取的内存账本 |
| --- | --- | --- | --- | --- |
| DBS 业务 | PostgreSQL、现有 DBS 程序 | DBS 的 PGDATA | DBS 的 `PGDATA/base` | DBS 容器 cgroup |
| DBS 备份 | PG14 `pg_basebackup`、`pg_verifybackup` 等客户端 | 只读连接 DBS 的 Unix Socket，写独立备份卷 | DBS 备份输出目录 | DBS 备份容器 cgroup |
| HISDBS 业务 | PostgreSQL、现有 HISDBS 程序 | HISDBS 的 PGDATA | HISDBS 的 `PGDATA/base` | HISDBS 容器 cgroup |
| HISDBS 备份 | 同上 | 只读连接 HISDBS 的 Unix Socket，写另一备份卷 | HISDBS 备份输出目录 | HISDBS 备份容器 cgroup |

备份容器不启动数据库、不运行 DBS 业务程序；CCG 作为备份目录的资源控制辅助进程。两个备份输出卷应分别有容量预算和保留规则。它们即使是不同分区，也可能共用一块物理盘，不能把分卷当成磁盘 IO 隔离。

**源端读盘发生在 PostgreSQL 服务端，输出写盘发生在 `pg_basebackup` 客户端。** 两端各运行一个 CCG 后，自动探测得到各自容器的 cgroup，不必从备份容器跨 cgroup 猜测数据库内存。业务容器 CCG 对本容器 PGDATA 发 `POSIX_FADV_DONTNEED` 建议；备份容器 CCG 处理其输出卷。若把源端 CCG 改放到备份容器，必须只读挂载同一实际 PGDATA 文件并显式监控业务 cgroup；核对设备号/inode/UID，不能用复制副本冒充同一页缓存。

### 94 当前的跨 Pod 挂载，以及为何没有合并到同一个 Pod

94 的数据库业务容器和备份容器**不是同一个 Pod**。两个业务 Pod 沿用 YsP 原部署；`ccg-test-dbsbackup`、`ccg-test-hisdbsbackup` 是等待 YsP 正式登记前的临时备份 Pod。四个 Pod 固定到同一节点。压力生成器另作为94宿主机的systemd服务运行，通过本机Unix Socket发送SQL；事务由业务Pod中的PostgreSQL执行，生成器只写独立`ccg_loadtest`库。

| 数据流 | 94 宿主机目录 | 备份容器内 | 业务容器内 |
| --- | --- | --- | --- |
| DBS Socket | `/opt/PUC-784/dbspod/postgresql/pgdata` | 只读`/pgsocket`；`PGHOST=/pgsocket`、`PGPORT=14005` | 原有实际PGDATA和Socket目录 |
| DBS 备份输出 | `/home/ccg-backups/dbs` | 可写`/backup` | 同一宿主目录另挂入业务容器；原厂`basebackup`路径经符号链接指向该目录 |
| HISDBS Socket | `/opt/PUC-784/hisdbspod/postgresql/pgdata` | 只读`/pgsocket`；`PGHOST=/pgsocket`、`PGPORT=16911` | 原有实际PGDATA和Socket目录 |
| HISDBS 备份输出 | `/opt/PUC-784/ccg-backups/hisdbs` | 可写`/backup` | 同一宿主目录另挂入业务容器；原厂`basebackup`路径经符号链接指向该目录 |

`hostPath`共享的是**同一节点的文件和inode**，不是把数据在两个Pod之间复制。备份Pod拥有独立的低优先级、1GiB内存request与2GiB内存limit；节点压力下可以优先停止或驱逐备份Pod，而不把数据库Pod一起驱逐。若改成同Pod双容器，使用Pod内共享卷与Socket会更简便，但kubelet驱逐该Pod时数据库和备份会一起离开，备份容器也不能单独设较低Pod优先级。当前以保护数据库可用性为主，所以先验证独立Pod。这个选择仍须以现场驱逐记录、备份完成率和数据库时延复核，不能仅凭拓扑推断安全。

此`hostPath`方案仅适用于94的**单节点、固定调度**实验。多节点生产环境要重新设计共享存储与本地Socket：跨节点不能用这种Unix Socket；备份Pod漂移后也不能假定另一节点拥有同一备份目录。两个业务容器原厂`start.sh`会递归`chown /opt/postgresql`，因此94把共享备份卷挂在数据库树外，并用符号链接保留原厂路径，避免重启时遍历大量备份文件。正式YsP服务导入与原厂调度衔接尚未验收。

## 先准备四个范围

下面的路径只是示意，`state_file` 的父目录由运行 UID 拥有，权限 0700；两个容器不能共用一个状态锁。DBS/HISDBS 可以使用相同的**容器内**路径，但它们应挂载各自的实际卷。

业务容器 `/etc/ccg/source.json`：

```json
{
  "mode": "paths",
  "action": "observe",
  "paths": ["/opt/postgresql/pgdata/base"],
  "cgroup_path": "",
  "state_file": "/run/ccg-source/status.json",
  "cache_target_bytes": 2147483648,
  "web_listen": "127.0.0.1:9898",
  "web_history_samples": 3600
}
```

备份容器 `/etc/ccg/output.json`：

```json
{
  "mode": "paths",
  "action": "observe",
  "paths": ["/backup"],
  "exclude_names": ["lost+found"],
  "cgroup_path": "",
  "state_file": "/run/ccg-output/status.json",
  "cache_target_bytes": 1073741824,
  "web_listen": "127.0.0.1:9898",
  "web_history_samples": 3600
}
```

配置只写出需要覆盖的字段，其余使用 `init-config` 所示默认值。2 GiB/1 GiB 是实验软目标，**不是推荐生产阈值或硬上限**。根据容器 limit、节点余量、活跃页、业务 p99 和备份时长定标。`PGDATA/base` 中也可能有热点；建议从只观察和低复用备份产物开始，排除已识别热点后才对数据库端启用 enforce。数据库源端的 `global`、WAL 和事务目录默认排除，不要为追求更大回收数字解除这些保护。

**源端与输出端的排除策略不同。** 数据库源端保留默认的 `pg_wal`/`global` 等保护目录；备份输出端的 `/backup` 是成品副本，例子显式把 `exclude_names` 改为仅 `lost+found`，因此会覆盖输出副本中的 `pg_wal`。在94已校验的DBS备份中，`base` 约29GiB、`pg_wal` 约26GiB；若输出端沿用默认排除列表，约一半备份字节不会进入治理范围。`exclude_names` 是**整体替换**默认列表，不是追加。输出端仍要等待文件修改保护期，并在脏页、IO、业务延迟等保护满足时才发建议；必须实测写入中的WAL段是否被跳过、备份校验是否通过及读盘反弹情况。此例目前仍为`observe`，不能把配置说明当作已验证的enforce效果。

## 在容器里启动

将对应架构的静态二进制装入镜像或只读挂载。运行 UID 应能读目标文件并写自己的状态目录。每个容器先执行：

```sh
containercacheguard doctor --config /etc/ccg/source.json
containercacheguard run --config /etc/ccg/source.json

# 备份容器将 source.json 换为 output.json
```

`run` 在前台常驻。用原有 supervisor 管理它，保持 PostgreSQL/DBS 的退出和信号处理。若现有镜像由 `/root/start.sh` 启动，应在独立实验环境检查该脚本何时创建目录、切换 UID 和启动 DBS，再加入 CCG 子进程；不能简单把第二个永久命令接在 `start.sh` 末尾。就绪探针应同时确认数据库/DBS 接口和 `containercacheguard status` 的采样新鲜度，否则 Pod `Running` 可能掩盖控制进程已退出。

仓库提供[`deploy/business-supervisor.sh`](../deploy/business-supervisor.sh)作为**待四容器实测的接入候选**：它先启动原厂`/root/start.sh`，再检查CCG配置并启动CCG；CCG配置有误或进程异常退出时，间隔15秒独立重试，保留业务进程；Pod停止时向原厂启动脚本转发TERM。94上用虚拟业务和反复退出的虚拟CCG验证过这一信号/重启关系，尚未验证真实DBS重启全过程。部署时仍需为CCG状态单独告警，避免它持续失败而业务Pod看似健康。

旧 DBS 代码有多个硬编码 `vmtouch` 路径，涉及备份、同步和源端 `releaseCache`。在 observe 阶段可并存以看现状，但无法把下降归因于 CCG；**enforce 阶段同一范围只能有一个回收执行者**。若现有 DBS 发行包没有关闭旧清理的配置开关，需要做明确的 DBS 接入改造，并验证备份/恢复退出码和异常路径。把 `vmtouch` 换成静默返回成功的空命令会掩盖问题，不应采用。`nocache` 若仍包装某个实际命令，也要核对其进程生命周期与缓存范围。

## 备份容器怎样连接 PostgreSQL

将数据库的 Unix Socket **目录**共享给对应备份容器；备份输出使用独立挂载卷。94 的候选部署用各自的主机目录挂载做实验，生产可改用有容量与故障隔离能力的 PVC。备份容器无需写 PGDATA，也无需共享数据库网络命名空间。当前 94 的 Socket 位于 PGDATA 内，因此候选部署只读挂载整个目录；应限制备份容器权限，后续可把 Socket 改到专用共享目录。Socket 目录权限及 PostgreSQL `pg_hba.conf` 的 `local replication` 规则应经过实际验证。示例：

```sh
pg_basebackup -F p -P -v --checkpoint=spread -X s \
  -h /run/pg-socket -p 5432 -U replica \
  -D /backup/20261107-full --max-rate=60M
pg_verifybackup /backup/20261107-full
```

这里的`--checkpoint=spread`让检查点尽量摊开，避免主动请求快速检查点造成额外尖峰；正式接入时仍要核对DBS发行版参数和备份时长。`--max-rate`负责备份数据传输的限速，**不是全盘 IO 硬上限**，WAL流、校验和业务写入仍会增加IO。输出目录创建后才会被路径发现；初始峰值要计入余量。一次完整备份结束后，记录`backup_manifest`校验和、`pg_verifybackup`结果与恢复演练，再按容量规则淘汰较早的实验备份。真实生产保留策略必须满足RPO/RTO，不可照搬实验清理。

仓库的 [`deploy/backup-once.sh`](../deploy/backup-once.sh) 是一次性备份执行脚本，供独立备份容器或调度任务调用；它**不是**CCG daemon，也不自动替换DBS原有调度。运行前设置`BACKUP_ROOT`（已挂载的输出目录）、`PGHOST`（共享的Unix Socket目录）、`PGPORT`、`PGUSER`（脚本默认`replica`，94候选包指定独立的`ccg_backup`复制用户）和`MAX_RATE`（默认`60M`），再传可选的日期备份名，例如`20261108-lab`。94候选包从各自的Kubernetes Secret注入`PGPASSWORD`；密码不能放在脚本、镜像或公开配置中。脚本用`flock`拒绝并发，在隐藏的临时目录完成PG14 `pg_basebackup`与`pg_verifybackup`，两者成功后才原子改名为正式备份目录，并保存清单SHA256；失败时保留带`.inprogress-`前缀的目录供诊断，绝不可当成有效备份，也要有容量告警和人工清理规则。恢复演练仍是独立验收步骤。

[`deploy/backup-supervisor.sh`](../deploy/backup-supervisor.sh)只维持备份容器中的CCG常驻；CCG失败后独立重试，不重启整个Pod，避免中断正在运行的备份。它目前**不会自动按时发起备份**。94的候选包也不把CCG状态作为Kubernetes liveness条件；监控必须单独发现CCG故障并告警。外部监控页使用各自的认证令牌，不能裸露在公网。

原 DBS 内置的 03:00 备份调度若仍在业务容器调用 `pg_basebackup`，就没有真正分离。已检查本地 DBS 源码：`DbUtil.startUp()`总会注册备份定时任务，`mod_phy_backup_time`只改变时刻，没有关闭开关；当当天备份目录存在且`backup.conf`的状态为`ok`时，`PostgresqlUtil.backup()`才跳过。因此不能只把新容器装上就声称分离，也不能让它在另一目录生成重复的全备。接入可选两条路线：给DBS增加明确的外部备份委托接口；或使备份容器在DBS认得的共享目录完成校验后，原子登记DBS格式的备份元数据，再把DBS原调度放在不会与新备份重叠的兜底时段。后一条路线必须测试失败、超时、重启及恢复选择，不可提前标记`ok`。94当前候选包尚未做到自动调度与元数据对接；从备份容器手动运行只验证数据通路，**不等于DBS自动任务链已经迁移验收**。

94的原厂`/root/start.sh`每次启动还会执行`chown -R postgres:postgres /opt/postgresql`。若将备份卷直接作为`/opt/postgresql/basebackup`的嵌套挂载，业务Pod重启会递归遍历整套备份文件，造成额外IO和启动延迟。接入脚本因此计划先停业务Pod、保留原`basebackup`目录，再把备份卷挂载到`/opt/postgresql`之外的容器路径，并在原厂`basebackup`路径建立指向它的符号链接。原厂备份/恢复仍使用同一路径；部署失败时脚本恢复旧目录和原Pod模板。上线前必须实测业务重启、备份识别和回退，不能把候选路径方案当成已验证结果。

## 打开实时页面

每个容器的 Web 监听地址需按 Pod 网络设置。**94当前 DBS/HISDBS 均为 `hostNetwork: true`，所以两者共享宿主机网络命名空间，不能同时绑定 `127.0.0.1:9898`。** 四实例须选择互不冲突的监听端口，并由安全的本地端口转发或鉴权代理访问。普通 Pod 可按下例使用端口转发：

```sh
kubectl -n <namespace> port-forward pod/<hisdbs-pod> 9898:9898
# 浏览器打开 http://127.0.0.1:9898/
```

直接打开 `http://<节点IP>:9898/` **不会自动连到普通 Pod 内页面**：需要显式转发或网关。94的四个候选实例均采用`hostNetwork: true`，计划按DBS业务9898、HISDBS业务9899、DBS备份9900、HISDBS备份9901分配不同端口，并用每实例令牌保护只读页面。只有实际Pod启动、端口监听与认证测试通过后，才能从节点IP访问。若改成普通Pod网络，则使用`kubectl port-forward`或有TLS与鉴权的代理；转发本身不增加用户鉴权，Pod重建后还需重新核对页面状态。

逐个查看四个 Pod 时可以选不同本地端口：`9899:9898`、`9900:9898` 等。页面每 2 秒获取状态，曲线每 15 秒刷新，显示采样时间和过期提示。`file` 包含 `shmem`，页面单列 `max(0,file-shmem)` 估算普通文件缓存；`active_file`/`inactive_file` 是 cgroup 的 LRU 分类。匿名内存、共享内存、slab 只展示，不能被文件范围释放建议清除。页面显示的 `advice_bytes` 是建议范围，净驻留下降另列，两者不能混为一谈。

若必须监听非回环地址，配置 `web_token_file`，只授予运行UID读取权限（单UID可用0600，94候选用root属主、进程组998、0640），用户名 `ccg`，并通过 TLS 反向代理或安全隧道访问。HTTP Basic 自身不加密，不应将开放的 HTTP 端口直接暴露在生产网络。页面与 API 均只读：`/api/v1/status` 返回最新状态，`/api/v1/history` 返回最近有界采样；重启后历史清零。48 小时统计要由持久采集保存，不能依赖浏览器进程内曲线。

## 压力测试与验收

1. 记录四个 Pod 的镜像摘要、DBS 程序校验和、PG 版本、配置差异、requests/limits、UID、PVC 与底层磁盘设备。版本不同就明确写出来。
2. 在迁移数据副本上先跑 12 小时 observe，对齐写入 TPS、WAL、备份时间、同步突发和双份备份重叠。旧清理措施是否运行必须单独标记。
3. 在不改变数据量/限速/资源配置的前提下，对**同一明确范围**启用 enforce；至少记录 PGDATA 与备份端的 `file`、`active_file`、`inactive_file`、`anon`、`shmem`、dirty/writeback、IO、节点可用内存、CCG CPU/RSS 和业务 p95/p99。
4. 每 12 小时生成统计图和说明；48 小时全程保留原始采样、Kubelet `Evicted` 事件、OOM 与容器重启记录。完整备份必须通过 `pg_verifybackup`，并做独立恢复演练。
5. 若 `active_file` 未复现、CCG 没发出建议、业务延迟恶化、读盘反弹或旧 vmtouch 同时运行，不宣称问题已解决。有限测试只能证明明确压力范围内的结果，不能保证所有未来负载下“百分之百不驱逐”。

截至2026-11-08 19:11，94已通过YsP部署与171同标签的DBS/HISDBS镜像，均运行PostgreSQL 14.19；两套当日完整种子备份均在94通过原生校验、隔离恢复并切换活动库。两个业务Pod已接入CCG`observe`；两个临时备份Pod中HIS仍为`observe`、DBS备份端改为`enforce`但缓存低于软目标，尚未发生有效释放，**两者都不是YsP正式备份服务**。DBS在持续模拟压力下的新全备于17:45通过PG14原生`pg_verifybackup`，19:11又从该备份在独立目录完成恢复、WAL重放、结构核对与正常关闭；克隆约13分钟，总演练约20分钟，恢复出的`clouddb`为1,445用户模式、228,296普通表和401,137索引。HISDBS新全备到54%时，目标机可用内存低于3.5GiB保护阈值，被实验安全守护主动停止，失败的临时目录不可用；根分区容量并非此次停止原因。正式YsP接入、CCG有效释放、HISDBS新备份恢复与回退演练均待验收。94约30GiB内存、171约61GiB，不能声称等规格对照。

这次HISDBS停备份时，分钟采样显示94的DBS容器匿名内存在一分钟内从约0.16GiB升至2.51GiB，宿主可用内存从4.56GiB降到2.18GiB；同一时段HISDBS普通文件缓存从0.36GiB降到0.04GiB、HIS备份容器从0.42GiB降到0.01GiB。**CCG只对所选普通文件发释放建议，不能清除PostgreSQL进程的匿名内存或共享内存。** 因此不能把这次内存低谷归咎于备份文件缓存，也不能用“CCG运行着且没驱逐”来证明它已经解决全部内存压力。DBS匿名内存尖峰的具体进程、事务和触发条件还需结合进程级快照分析；在明确原因前，不能简单降低安全停备份阈值重试。

同一轮实验在18:29还出现多个PostgreSQL任务被内核报告在XFS文件系统等待超过122秒，DBS模拟压力程序曾因连接失败由systemd重启；Pod没有重启或被驱逐，仍不满足业务可用性目标。为保护94，18:46主动暂停两套模拟压力，18:49两个活动库的Socket探测重新接受连接。**暂停时段不能计入连续压力验收**。XFS等待与百万文件扫描、同盘备份写入、业务读写、容器内递归`chown`之间的因果关系尚未确定；要分别记录IO延迟、阻塞任务和业务SQL响应，不能只按备份MB/s判断安全。

94的压测辅助程序还提供`--min-mem-available-gib`（默认5）和`--min-free-gib`（默认64）。每轮启动前检查节点可用内存和PGDATA所在文件系统剩余空间；运行中每约5秒检查内存，低于阈值即终止该轮`pgbench`并记录`paused_low_memory`，恢复余量后继续。暂停时段不能算成达标负载时段。这个保护只是实验熔断，不代替Kubernetes驱逐事件、业务延迟和容器OOM监测。

模拟程序默认`--payload-mode=compressible`，适合低成本冒烟，但重复文本容易被PostgreSQL压缩，产生的WAL可能远低于源端。`--payload-mode=random`只在独立的`ccg_loadtest`库中启用`pgcrypto`并生成难压缩载荷，可用`--event-bytes`、`--update-bytes`和`--rate`校准事务、行变更及WAL三类速率。随机模式把测试列设为行内存储，避免TOAST元组把`pg_stat_database.tup_inserted`虚高；单行载荷上限为6000字节。94在2026-11-08的120秒DBS标定中以110 TPS、每事务3次插入/4次更新、2048/4096字节随机载荷得到约111.1提交/s、332.6插入元组/s、443.6更新元组/s、2.78 MB/s WAL。源端备份时段相应中位数约105.4、367.2、452.5及2.86 MB/s；这是接近的**聚合指标**，不是原业务SQL或gissync数据流的逐条重放，也尚未证明长期备份并发安全。持续运行时需监控独立测试库增长和磁盘余量，不能只看TPS。当前辅助程序在测试库达到默认12GiB时于轮次之间清空实验事件表；若其余空间增长不能因此降低，便暂停并报告，不修改业务`clouddb`。此低频TRUNCATE也要在结果中标记。
