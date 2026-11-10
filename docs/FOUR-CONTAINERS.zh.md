# DBS/HISDBS 与独立备份容器：部署和 Web 使用

本页给运维人员一条可检查的接入路线。ContainerCacheGuard（下称 CCG）是常驻进程，不替代 PostgreSQL、DBS 或 `pg_basebackup`。当前94迁移与验证目标为 **PostgreSQL 14.19**；下列配置仍是接入设计，尚未表示四容器已在94完成部署与验收。

## rc.9监控四个实例

沿用每个实例的只读Web监听和认证，读取metrics/healthz/readyz；无需额外扫描PGDATA。[接入说明](MONITORING.zh.md)和[Prometheus例子](../deploy/prometheus.example.yml)记录目标标签、字段可用性和口径。**不要把CCG健康失败用作同一DBS业务容器的liveness条件，也不要因sidecar监测暂不可用就让数据库Pod整体不就绪。** 本轮只交付接口和示例，没有在94自动安装Prometheus或更改Pod探针。

## rc.7接入已有容器时怎么升级

下载对应架构的[候选版本](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.7)，核对SHA256，保留旧程序和配置。配置无需新增必填字段，但事件发现会占用总发现额度的25%，应重测大目录覆盖速度；refault保护启用后，初始基线未知时会暂停。先用独立状态目录与`observe`运行，核对业务容器/备份容器的cgroup和授权路径；同一范围不能同时运行两个`enforce`实例。确认后由既有进程管理方式受控替换CCG，不通过重启PostgreSQL来更换CCG。异常时暂停新CCG，恢复旧程序和配置。此说明是升级方法，**不表示94四容器已换成rc.7或已通过验收**。

## 94 的当前进展（2026-11-09 16:05）

94只有约30GiB内存，171约61GiB。经用户批准，94的DBS `shared_buffers` 从8960MB调至6144MB、HISDBS从8960MB调至4096MB，PG14实际SQL分别返回`6GB`和`4GB`；171未改。两库业务模式数仍为1,445和1,442，CCG进程随业务Pod运行。**这是给30GiB测试机腾出容量的配置试验，不是文件缓存工具已经解决业务中断的证明。**

94原厂`/root/start.sh`默认在每次启动时从镜像复制`/home/config/db.ini`，会覆盖宿主挂载的修改；首次只改宿主文件重启DBS后果然仍为8960MB。该脚本自带`NOT_REPLACE_DBCONFIG`环境开关，可跳过覆盖。最终只在两个94业务Deployment增加此开关，各自修改对应的`db.ini`节，并逐库重启。单副本Deployment采用RollingUpdate，直接滚动重启可能让新旧Pod同时挂同一PGDATA；本次操作先缩容到0并等旧Pod完全退出，再改配置、扩容到1。原配置和Deployment保存在94的受限回退目录。DBS原厂启动流程还会清空`postgresql.auto.conf`，因此不能靠`ALTER SYSTEM`持久覆盖。生产接入应由平台管理员确认受控停启窗口和回退流程。

调整后**无备份**分档：DBS20TPS/180秒完整完成，实际20.13TPS，窗口两库24次SQL探针失败0，节点可用内存最低9.40GiB；DBS60TPS+HIS0.5TPS各300秒完整完成，实际59.37/0.528TPS，窗口50次探针失败0，最低9.21GiB。DBS110TPS+HIS0.5TPS目标300秒时，约220/190秒后因`MemAvailable<7GiB`保护线主动停压，不能算完成。该窗口30次探针失败0，DBS匿名内存约0.20→2.56GiB、普通文件缓存约2.68→0.82GiB，节点可用内存最低采到6.71GiB；无备份且CCG为`observe`。12:12持续110TPS重试也因同类匿名内存峰值被看护停止，分钟采样最低5.79GiB。12:27启动的60TPS/HIS0.5TPS长窗又在12:42因可用内存约6.04GiB触发止损，负载停止；不能称60TPS持续通过。取证将DBS单个PostgreSQL后端的匿名RSS约2.36GiB与`pucbdm`同一大型查询的数据库日志按容器PID15650对应，查询含1,431个SELECT、1,430个UNION ALL分支。窗口内轻量SQL探针未失败、Pod未重启，仍不能把这一点当成备份安全通过。12:50左右启用的20TPS/HIS0.5TPS档在13:12仍因同一查询峰值触及5GiB保护线停压；随后启用7GiB自暂停、4GiB及SQL/PG故障硬止损的自适应低档，持续占空比待测。94临时DBS备份Pod输出挂载改到约373GiB空闲的根卷实验目录，Pod和Socket已验证。**持续等压尚未实现，文件缓存回收无法释放匿名内存尖峰。**

两个备份Pod仍是临时实验Pod，并未成为YsP正式服务。上传自制产品候选包后YsP虽然解析成功，但其缩减内容包与原始安装ZIP有大量差异，尚未证明符合厂家增量升级规则；**不能执行该候选包的产品升级**。候选清单的`nginx-ingress`版本与原始024 ZIP不同，但与94当前运行镜像相同，不能误称这次上传把nginx实际升级了。两库当前Deployment镜像也没有因此变化。DBS低平均压力观察模式全备、校验和隔离恢复已通过；HIS新全备、CCG回收对照、业务SLO和Kubernetes持续稳定性验收仍未通过。

13:39–15:38完成94 DBS约57.9GB PG14新全备，`pg_basebackup --max-rate=20M`、`pg_verifybackup`均成功，146MiB清单SHA256复算一致。输出CCG全程为`observe`：119个连续分钟样本中源DBS普通文件缓存峰1.499GiB、备份Pod峰1.868GiB；节点可用内存最低4.663GiB，物理盘分钟读/写峰37.46/28.14MiB/s，忙碌峰94.38%（校验阶段）。两库各475次轻量SQL探针失败0、PG启动时间不变，四容器OOM kill增量0、未见Evicted。随后同卷reflink隔离恢复成功：克隆119.441秒、约206.882秒可查、237.617秒完成对象核对和停止；恢复的`clouddb`为1,445模式/228,296普通表/401,137索引。**这不是等压结果**：全窗DBS测试库实际平均3.482TPS、原业务库17.371TPS，明显低于171约105TPS中位数；亦不含远端拉取恢复时间。观察模式下的缓存下降来自内核等因素，不是CCG释放效果。

17:49–18:42在同一DBS数据量上启动第二轮20M限速全备，备份Pod输出CCG改为`enforce`、目标普通文件缓存1GiB，业务Pod仍为`observe`。文件传输约90%时节点可用内存跌破4GiB，独立保护程序终止备份；未产生有效备份，也未开始`pg_verifybackup`。54个连续分钟样本的输出普通文件缓存峰1.873GiB，与首轮1.868GiB近似；CCG累计发出约0.91GiB释放建议，区域复查净下降约0.51GiB，但**没有证明峰值降低，也没有使备份完成**。本轮DBS模拟库实际18.778TPS，仍低于171约105TPS。DBS同一PostgreSQL后端在窗口末匿名RSS约2.36GiB，PG日志与已知约每30分钟执行的PUCBDM大型查询PID一致；文件页释放不能处理这部分内存。两库各213次轻量SQL探针失败0、PG启动时间不变、OOM kill增量0，说明保护及时，不能把中断的备份算作验收通过。默认`critical_mode=pause`在2GiB备份容器接近限额时会暂停回收，脏页较多时也会退让；单看`enforce`标志或释放建议字节数会误判效果。

更关键的是，19:12在**无备份运行**时同一PUCBDM查询再次造成约2.36GiB后端匿名RSS，节点可用内存低于4GiB，两库轻量SQL探针各超时一次；PG日志记录绑定阶段约72.82秒。负载保护程序停止模拟写入，PG启动时间未变，未见Evicted。由此可见，94约30GiB节点上即便完全停止备份，业务查询仍能造成短暂不可用。缓存回收工具只能解决普通文件页的一部分竞争；要完成验收，还必须处理该查询峰值和节点内存余量，并在最后一次调整后重新计时连续48小时。

19:39–19:45做了一次94只读的业务SQL分批候选试验：当前1,431个目标表的堆文件合计0字节，原SQL为1,431个`SELECT`分支；前50/100/200分支执行约0.05/0.12/0.30秒，全部分支每批50个、29条串行执行约0.99秒。这个结果只支持**进一步评审分批方向**，不等于原应用72.82秒查询的正式加速比：测试负载不同、没有量到分批时的数据库后端RSS，也没有验证非空数据和并发写入的结果一致性。中期可考虑集中注册表，减少每30分钟跨大量空关系生成巨型SQL；不能仅依据表当前为空就直接跳过查询。

## 94 的先前阶段结果（2026-11-09 03:55）

**新增的原厂备份对照也未通过。** 03:00 由 DBS/HISDBS 原有程序同时启动的两份 PG14 `pg_basebackup --max-rate=60M`，仍在业务容器内写 `/opt/postgresql/basebackup/20261109`，而不是由两个临时备份 Pod 执行。03:04 左右的底层盘5秒样本读约39MiB/s、写约24MiB/s、忙碌约64%、平均读等待约27ms；HISDBS只读SQL探针连续约330–650ms，备份前通常约30ms。03:05:37实验安全监视因节点 `MemAvailable≈3.19GiB` 跌破3.5GiB保护线而停止当天备份进程，两份11月9日备份均不完整。现有11月8日有效备份保留。

更重要的是，停备份后服务器仍持续卡顿：03:00–03:19期间，两库各只记录40次探针（15秒计划间隔本应约76次），DBS失败22次、HISDBS失败15次；采样空白不能视为成功。HISDBS PG约03:15:56内部重启，DBS PG约03:17:51内部重启；DBS管理进程还在03:34再次因`missing process`调用`pg_ctl stop -m fast`，约03:37:07再次重启PG。四个相关Pod仍显示Running/重启0，当前Kubernetes事件未查到Evicted、节点MemoryPressure=False，**但数据库连续性已经失败**。现阶段无法把停备份后的卡顿归因于某个单独进程或CCG；当时HIS源端CCG已恢复`observe`。

在没有新实验备份时，02:29–02:31 DBS匿名内存再次约2.57GiB，PG日志同期有同一条PUCBDM大型查询。两库PG配置的`shared_buffers`均为8960MB，94节点约30GiB、171约61GiB；普通文件缓存治理不能清理PG共享缓存和大型查询的匿名内存。03:34为隔离故障已暂停94的两套**实验**模拟负载，故后续时段不能算作持续等压验收。仅调整回收速度无法从根本上消除容量与查询峰值冲突；下一步需先评审94的内存预算和PUCBDM分批查询，再在安全余量下重新比较无回收/输出回收/PGDATA回收。候选的cgroup v2文件页refault退让指标已加入源码，默认关闭，尚未部署或标定；它也不能单独证明是谁造成再次读盘。

03:38–03:45的隔离窗口进一步验证这一点：无备份、无实验模拟负载，HIS源端CCG也只观察，DBS匿名内存仍从约0.27GiB升至约3.71GiB，宿主可用内存从约10.0GiB降至约5.3GiB；03:40–03:41 `workingset_refault_file`差分达到约7,278/5,749页每秒，底层盘忙碌约75%–76%，而03:40物理读吞吐仅约7.5MiB/s。PG日志同时记录同一PUCBDM大型查询，03:41两库轻量SQL探针各有一次5秒超时。多个指标强烈时间相关，但分钟聚合不能证明唯一因果；**尤其高refault并非CCG释放所独有**。即使没有发生Kubernetes驱逐，业务已经出现超时，必须把它纳入验收。

94 上的 DBS/HISDBS 仍是 YsP 原有业务 Pod；两个备份容器是另外创建的**临时测试 Pod**，尚未注册成 YsP 正式服务。DBS/HISDBS 数据迁移与活动库切换已完成，两套数据库均为 PG14.19。四处已运行 CCG：DBS 业务端和 DBS 备份端为 rc5，HISDBS 业务端为 rc5，HISDBS 备份测试端为含新目录监视修复的 **rc6 候选镜像**；rc6 尚未作为正式版本发布。HISDBS 业务端于00:48在保留原路径和资源预算的条件下，临时把普通文件缓存软目标改为512MiB并启用回收，DBS 业务端仍只观察。该配置是实验值，不是生产推荐值。

00:39开始的 HISDBS 真实 `pg_basebackup --max-rate=40M` 实验中，备份端 CCG 对新建输出目录收到了文件事件；至00:57累计发出约6.48GB缓存释放建议，复查时观察到约2.83GB净下降，CCG自身约占单核3.1% CPU、RSS约48MB、错误0。底层盘当时约每秒读38–43MB、写39–44MB；输出容器普通文件缓存从初期约1.94GiB峰值回落到约0.4–1.1GiB。源PGDATA缓存亦从约1.10GiB回落，但源端CCG到00:50的复查净下降仅约12MB，**不能把其余下降归功于工具**。这是文件缓存控制的阶段证据，不能替代完整备份校验。

01:03时备份约35%，DBS容器匿名内存又从约0.2GiB升至约2.56GiB；节点可用内存跌破实验保护线4GiB，安全监视停止备份，没有有效新备份。94日志在00:03、00:33、01:03均出现同一条约7.6万字符的PUCBDM查询，显示这类匿名内存尖峰在当前观察段约每30分钟重现。文件页释放不能降低匿名内存。随后节点曾在约12分钟内响应迟缓：备份停止后的监控有效样本显示全机底层盘读忙碌约90%–95%，但目前**未确定**具体读盘进程和各因素的因果关系；缺失采样不可画成实际零值。

恢复监控后补查发现：01:04–01:23这段两库各46次15秒只读SQL探针中，DBS失败25次、HISDBS失败16次；DBS内部PostgreSQL在01:20由DBS管理进程执行`pg_ctl stop -m fast`，约01:23重新启动，Pod自身始终显示Running且重启数0。DBS日志同期有etcd连接异常、`pg_isready`超时及“missing process”；01:03–01:21的PG日志有3,818条带耗时记录，3,765条来自PUCBDM，其中525条超过5分钟。Kubernetes未查到该段`Evicted`事件，节点后续MemoryPressure=False，**但业务连续性验收已经失败**，不能把“未驱逐”写成“问题解决”。HIS源端CCG在00:54–01:02累计发出约4.04GB释放建议、复查净下降约2.05GB；这与后来IOPS/响应异常的关联尚不清楚，01:48已将源端恢复到`observe`/2GiB原配置。失败的HIS备份临时目录已按精确路径清理，已有完整备份保留。94约30GiB内存且模拟压力因5GiB保护线间歇暂停，不能视为与171约61GiB节点连续等压。

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

### 94 的 YsP 注册进度（2026-11-08 21:23）

已在94本机构建两个`V1.0.00.002`单服务包，镜像内含CCG`v0.2.0-rc.5`及PG14.19备份客户端；两个Deployment模板均通过Kubernetes服务端`dry-run`。该镜像包还含原厂PG14客户端和旧运行库，**没有上传到公开GitHub**。两个临时备份Deployment已依次换用`V1.0.00.002`镜像并恢复1/1 Running；容器内CCG版本与PG14 Socket连通检查通过。用户完成YsP图形登录后，已在“添加服务→上传单服务包”页面分别上传两份新版包；安装包目录出现两份适配包，但候选列表仍无`dbsbackup/hisdbsbackup`，服务列表搜索也无结果。为检查缺失原因，曾在94活动产品的`Version.json`和`DPConfig/deployDetails.json`补入两项及模板；仍未完成界面注册，现已**全部回退**到原清单121/147条，原文件回退副本保留在94实验目录。结论是：上传单服务包和直接改活动清单均不足以把新服务注册到当前PUC产品，下一步需查YsP支持的产品升级或新服务注册流程。运行中的两个备份Pod仍是临时Pod，不能把它们写成YsP正式服务。

随后按安装文档核对了YsP的“Product upgrade”入口：94当前没有可选升级版本。新增服务需先注册包含服务清单、部署模板和拓扑的合规产品版本，再检查升级差异；**目前没有执行产品升级**。不能把单服务包上传成功或临时Pod Running写成YsP正式服务上线。

### 业务连续性必须独立于 Pod 状态验收

94的DBS Pod始终显示`Running`且容器重启数为0，但PostgreSQL日志证实22:10和22:23先后两次收到`fast shutdown request`并重新启动。DBS日志显示其健康检查期间出现`pg_isready`和etcd超时，程序主动调用`pg_ctl stop -m fast`；同期分钟采样器多次无法访问Kubernetes API。21:55–22:12窗口，数据库慢日志中的多条`pucbdm`绑定阶段记录耗时约120秒。以上证明94已经出现过**Pod未驱逐但数据库连接中断**，尚不能断定是CCG、备份、某一SQL或文件缓存单独造成。94只有约30GiB内存，171约61GiB，DBS模拟压力又多次触发5GiB安全暂停；目前不能声称与171等强度连续运行，也不能保证“百分之百不驱逐”。验收应同时跟踪SQL连接成功率和p99、PostgreSQL内部重启、etcd/API可用性、节点可用内存、cgroup匿名/文件内存、IO延迟、备份校验与恢复演练。CCG的`fadvise`只能建议回收文件页，不能释放PUCBDM查询造成的匿名内存。

23:04实测94的DBS cgroup `file≈6.49GiB`，其中`shmem≈5.90GiB`；HISDBS `file≈0.69GiB`，其中`shmem≈0.63GiB`。因此当时普通文件缓存近似分别只有0.59GiB和0.06GiB。CCG策略代码已用`max(0,file-shmem)`判断普通文件软目标，Web页也单列该值；按原始`file`总数宣称可释放6GiB会误导。旧rc4在DBS百万文件范围只完成2次全目录扫描、文件索引达到15万上限，故全量覆盖时延仍待实测。23:08–23:11已在不重启DBS/HISDBS业务Pod的条件下，单独把两套业务CCG子进程切到rc5；两套PostgreSQL Socket仍能连接。四处CCG现均为rc5，但业务端仍是`OBSERVE`，还没有取得真实高压全量备份的回收效果结论。

23:35开始的HISDBS真实备份又发现一个覆盖时延问题：备份输出目录是运行后新建的`.inprogress-*`，此前已经存在的完整备份有大量文件。运行中的rc5累计扫描了旧文件，却没有收到新目录内文件的访问提示，初期建议量为0；同时输出容器普通文件缓存约1.4GiB，策略在脏页超出128MiB时按设计退避。已在源码中补充“收到新子目录创建/移入事件即安全地加监视”的修复，并在94 Linux/amd64通过专门的目录创建测试及完整测试集。**该修复尚未放入当前运行的备份镜像**；实际备份仍由rc5执行，结果不能归功于新代码。后续要在下一轮真实文件压力中观察新目录提示、有效释放量、IO与数据库连接连续性。

该轮备份于11月9日00:03被安全监视主动终止：宿主`MemAvailable≈2.8GiB`，低于预设3.5GiB保护线，备份约完成40%，没有有效成品。停机前每库102次、15秒间隔的只读SQL探针均成功，未观察到数据库内部重启。分钟采样在00:02→00:03记录DBS容器匿名内存约0.2→2.7GiB；DBS慢日志同刻记录`pucbdm`的约76千字符大型查询（约10.65秒）。这次尖峰与备份中止直接重叠，**不能把失败归因于HIS文件缓存或CCG策略单项**。94实际为Kubernetes v1.26.12/cgroup v2，kubelet硬驱逐线为`memory.available<1Gi`、系统内存预留5GiB；DBS/HISDBS各只有500Mi内存request却可用到128Gi limit，属于Burstable且实际用量远超request。备份Pod较低优先级有助于先牺牲备份，但在30GiB节点上不能保证业务绝不被驱逐。需要把大型查询的分批/并发控制、备份时间窗和资源余量一起纳入方案。[Kubernetes节点压力驱逐规则](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/)

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

**跨容器释放已做一个限定范围的实测。** 2026-11-08在94的DBS PGDATA中创建64MiB临时探针文件，由数据库容器读取后，`vmtouch`测到全部16,384页驻留。独立备份Pod通过只读PGDATA挂载运行CCG rc4、对同一文件发`fadvise`建议，CCG记录64MiB`advice_bytes`和64MiB`observed_net_reduction_bytes`，DBS容器再次测得0页驻留。临时文件随后删除。由此可确认：**同节点、同一底层inode且备份容器具有读权限时，建议可跨容器影响数据库容器读入的普通文件页缓存；只读挂载足够。** 它不能跨主机/PVC副本影响另一inode，也不能释放数据库匿名内存、shared buffers或保证热点不重新驻留。首轮用1GiB真实测试库关系文件时，旧版CCG虽进入CONTROL，但在25秒窗口`advice_bytes=0`。修复扫描令牌饥饿和重复扫描后，rc5候选在相同64MiB临时文件、较低64MiB/s采样预算下，25秒仍记录64MiB建议、64MiB净下降，数据库容器再次测得0页驻留；累计采样512MiB、自报CPU约0.73%单核、RSS约9MiB。第二次临时文件已删除。**这仍只是单文件机制测试，不能推断百万文件、真实备份和业务并发已通过。**

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

截至2026-11-08 21:23，94已通过YsP部署与171同标签的DBS/HISDBS镜像，均运行PostgreSQL 14.19；两套当日完整种子备份均在94通过原生校验、隔离恢复并切换活动库。两个业务Pod已接入CCG`observe`；两个临时备份Pod中HIS仍为`observe`、DBS备份端为`enforce`但自然缓存低于软目标，**两者都不是YsP正式备份服务**。另以独立CCG rc5进程对64MiB临时PGDATA文件做了跨容器有效释放机制测试，不能替代真实备份压力验收。DBS在模拟压力下的新全备于17:45通过PG14原生`pg_verifybackup`，19:11又从该备份在独立目录完成恢复、WAL重放、结构核对与正常关闭；克隆约13分钟，总演练约20分钟，恢复出的`clouddb`为1,445用户模式、228,296普通表和401,137索引。HISDBS新全备到54%时，目标机可用内存低于3.5GiB保护阈值，被实验安全守护主动停止，失败的临时目录不可用；根分区容量并非此次停止原因。正式YsP接入、真实负载下的缓存控制、HISDBS新备份恢复与回退演练均待验收。94约30GiB内存、171约61GiB，不能声称等规格对照。

这次HISDBS停备份时，分钟采样显示94的DBS容器匿名内存在一分钟内从约0.16GiB升至2.51GiB，宿主可用内存从4.56GiB降到2.18GiB；同一时段HISDBS普通文件缓存从0.36GiB降到0.04GiB、HIS备份容器从0.42GiB降到0.01GiB。**CCG只对所选普通文件发释放建议，不能清除PostgreSQL进程的匿名内存或共享内存。** 因此不能把这次内存低谷归咎于备份文件缓存，也不能用“CCG运行着且没驱逐”来证明它已经解决全部内存压力。后续已用容器PID与PG慢日志对应确认，大型PUCBDM查询是这一类DBS匿名内存尖峰的来源；在应用查询分批或留出更多内存之前，不能简单降低安全停备份阈值重试。

同一轮实验在18:29还出现多个PostgreSQL任务被内核报告在XFS文件系统等待超过122秒，DBS模拟压力程序曾因连接失败由systemd重启；Pod没有重启或被驱逐，仍不满足业务可用性目标。为保护94，18:46主动暂停两套模拟压力，18:49两个活动库的Socket探测重新接受连接。**暂停时段不能计入连续压力验收**。XFS等待与百万文件扫描、同盘备份写入、业务读写、容器内递归`chown`之间的因果关系尚未确定；要分别记录IO延迟、阻塞任务和业务SQL响应，不能只按备份MB/s判断安全。

19:18恢复原档位模拟压力后，首个300秒轮次完成约110.63提交/s、331.58插入/s、442.16更新/s；第二轮未叠加备份仍触发匿名内存尖峰。DBS某个PostgreSQL后端的匿名RSS最高约2.36GiB，宿主`MemAvailable`降到约5GiB，模拟程序按保护阈值暂停。只读取PG14 CSV慢日志的**非SQL元数据**发现，18:19与19:25两个尖峰附近，`pucbdm`均执行同一`query_id`的超长`SELECT`：约1,431个`SELECT`分支由1,430个`UNION ALL`连接，文本约7.6万字符，`BIND`阶段分别记录约29.6秒和10.7秒。20:25再次出现相同尖峰；取证到的宿主PID映射为容器内PID89036、匿名RSS约2.36GiB，**同一个容器PID**在PG14日志记录该查询10.47秒。因此已经直接确认这条应用查询是这一类匿名内存峰值的来源。大模式数量在此通过应用生成的大型SQL增加数据库后端的匿名内存，**不只是让备份扫描更多文件**；CCG的文件缓存释放能力不能代替应用端把查询分批并限制并发。这里已经是`UNION ALL`，不能把“改为UNION ALL”当作修复建议。

94的压测辅助程序还提供`--min-mem-available-gib`（默认5）和`--min-free-gib`（默认64）。每轮启动前检查节点可用内存和PGDATA所在文件系统剩余空间；运行中每约5秒检查内存，低于阈值即终止该轮`pgbench`并记录`paused_low_memory`，恢复余量后继续。暂停时段不能算成达标负载时段。这个保护只是实验熔断，不代替Kubernetes驱逐事件、业务延迟和容器OOM监测。

21:37统计自19:17恢复后的DBS辅助负载：8个已结束轮次仅1个完整300秒退出0，其余7个因节点可用内存低于5GiB提前暂停，累计实际运行约835秒，而观测窗口已约2小时20分钟。**systemd显示active并不代表110 TPS一直在产生**。当前30GiB节点与171约61GiB节点的资源差异已经实质阻止连续同强度验收；降低保护线不能把失败变成通过。

模拟程序默认`--payload-mode=compressible`，适合低成本冒烟，但重复文本容易被PostgreSQL压缩，产生的WAL可能远低于源端。`--payload-mode=random`只在独立的`ccg_loadtest`库中启用`pgcrypto`并生成难压缩载荷，可用`--event-bytes`、`--update-bytes`和`--rate`校准事务、行变更及WAL三类速率。随机模式把测试列设为行内存储，避免TOAST元组把`pg_stat_database.tup_inserted`虚高；单行载荷上限为6000字节。94在2026-11-08的120秒DBS标定中以110 TPS、每事务3次插入/4次更新、2048/4096字节随机载荷得到约111.1提交/s、332.6插入元组/s、443.6更新元组/s、2.78 MB/s WAL。源端备份时段相应中位数约105.4、367.2、452.5及2.86 MB/s；这是接近的**聚合指标**，不是原业务SQL或gissync数据流的逐条重放，也尚未证明长期备份并发安全。持续运行时需监控独立测试库增长和磁盘余量，不能只看TPS。当前辅助程序在测试库达到默认12GiB时于轮次之间清空实验事件表；若其余空间增长不能因此降低，便暂停并报告，不修改业务`clouddb`。此低频TRUNCATE也要在结果中标记。
