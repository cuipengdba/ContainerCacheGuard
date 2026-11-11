# 171 DBS/HISDBS 双组件更新实测（2026-11-11）

本次按用户要求更新到核对时GitHub最新已发布版本，所有既有配置保持不变。CRG为[v1.0.0-preview.1](https://github.com/cuipengdba/ContainerResourceGuard/releases/tag/v1.0.0-preview.1)，CCG为[v0.2.0-rc.12](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.12)。CRG包内`crg version`显示`v1.0.0`，是该预发布包内嵌版本，不能称稳定1.0。

## 结果与非计划故障

两容器的四个运行进程/proc/exe摘要均与官方附件一致；配置原始字节、Deployment spec、数据挂载和实际CPU/内存/IO限额相同。两个CCG和两个CRG分别完成9次短期SQL/API检查，检查区间内均正常。

**整体健康验收未通过：DBS随后在服务器北京时间10:58:17和10:58:21发生2次新的cgroup OOM kill，内核确认均为postgres进程；postmaster于10:59:02重新启动，10:59:46采样SQL/API恢复。** 10:56、10:57样本SQL/API超时，10:58样本SQL socket缺失。这是非计划数据库故障，不能按helper更新窗口豁免。HISDBS未见新增OOM或SQL/API错误。

DBS现有容器累计oom=731、oom_kill=3（本次窗口新增730次OOM事件、2次kill）；另有旧DBS容器历史1次kill，分别保留。早先01:29:49首次采到的1/1并非本次新故障。故障前DBS上限15.46GiB、shmem约9.33GiB、anon约5.58GiB、普通文件页约4MiB；CCG无法回收anon/shmem，故障后内存下降不能算CCG优化。尚未证明新二进制导致或排除其影响，资源限额回读未变。

原库system identifier、数据库/关系/namespace目录统计前后一致，原PGDATA挂载保持；没有删除、初始化、迁移数据。此项不是全量行校验或备份恢复验收。HISDBS启动时间保持；DBS启动时间因上述OOM变化，不能报告“两库全程未重启”。

## 保持的参数

| 参数 | DBS | HISDBS |
|---|---:|---:|
| cpu_percent / cpu.max | 30 / `840000 100000`（8.4核） | 相同 |
| memory_percent / memory.max | 25 / 16603635712（15.46GiB） | 35 / 23245090816（21.65GiB） |
| io_percent | 40 | 40 |
| 253:0 rbps / wbps | 1538183604 / 1809852654 | 相同 |
| 253:0 riops / wiops | 13400 / 13114 | 相同 |
| CCG mode / action | auto / enforce | 相同 |
| 普通文件缓存软目标 | 2147483648（2GiB） | 相同 |
| max_io_psi_avg10 | 0，明确关闭可选门槛；PSI实际未知 | 相同 |
| 日志 | 每天一文件，30天/1000000000字节 | 相同 |

CRG/CCG配置逐字节SHA256比较一致，全部现有参数保留。数据库shared_buffers=8960MB、maintenance_work_mem=2GB、autovacuum_work_mem=1GB、work_mem=2293kB、max_connections=1000保持。cpu.weight、memory.high、memory.swap.max、pids.max、io.weight也前后一致。未执行新IO标定、压测、全局drop_caches或数据库调参。

## 下载与验证

- CRG release ID `441132178`，asset ID `733665206`；包/附件SHA256 `78d3ba216fb61e848f20df394bf814fcc4de796e2d8fcd4edaab0155f4078f89`；ELF SHA256 `dbc48aa5a4aa4d4a77c6af03d3294a9fcf08a6855672a80b003fccd1fed8a2c8`。
- CCG release ID `440656474`，asset ID `732246953`；包/附件SHA256 `d563e4ce40580ed07a404a06c42165b626ba8cceb6d851e5ab05a2583d8fc292`；ELF SHA256 `d563e4ce40580ed07a404a06c42165b626ba8cceb6d851e5ab05a2583d8fc292`。

下载对应amd64附件和SHA256SUMS，核对GitHub asset digest及发布校验文件；CRG解包于独立目录，核对BUILD.json。CRG源码修订d3706f3ea7ad279fea818e5ad1aa62ac5d3f8710，包修订01d311f5637df95891975d3083961b5a03db281b；CCG源码修订f9808175914466d5162cef77a803b89a5be5955c。没有在节点临时编译。

## 现场更新方法

1. 在节点私有`/opt/container-guards171/update-20261111/backup/{dbs,hisdbs}`保存原bin/config/scripts、Deployment和CRG私有状态，保留旧程序以回退。这不是数据库全量备份，未创建额外容器。私有journal/原值备份不上传GitHub。
2. 当前程序目录仍为`/opt/container-guards171/upgrade-20261110/{role}`，通过原只读hostPath挂载到`/opt/guards`；运行目录仍为`/opt/container-guards171/runtime/{role}`，容器内`/opt/guards-runtime`。没有改挂载或Deployment。
3. CCG候选在原容器执行`doctor --config /opt/guards/config/ccg.json --require-ready`，严格预检通过后，节点侧临时文件+原子rename替换`bin/containercacheguard`，只向对应旧CCG子进程发TERM，由原辅助循环重启。逐个完成后再处理另一容器。
4. CRG候选`doctor --config /opt/guards/config/crg.json`预览仍仅三项且与原值相同。旧rc.3缺维护协议广告，未强行使用binary-install。核对CRG/CCG独立辅助循环的父子关系，仅暂停CRG循环、正常终止旧CRG，确认旧写入者退出且三项内核限额仍保留；新候选`upgrade-check`读取原journal成功，再原子替换`bin/crg`，恢复同一辅助循环。没有新state_dir、删除/改写身份或清空journal。
5. 等待新Agent，核对实际/proc/exe摘要、单实例PID、CRG managed与三项applied、CCG auto/enforce、配置/IO profile摘要、其他控制项、容器ID和SQL/API。记录每段短检查的真实区间；之后的OOM故障单独记录。

## 实际组件窗口与PID（服务器北京时间）

| 容器/组件 | 开始 | 新进程就绪 | 短检查结束 | PID旧→新 |
|---|---|---|---|---|
| dbs/CCG | 2026-11-11 09:56:47 | 2026-11-11 09:57:08 | 2026-11-11 09:58:20 | 2663→615229 |
| hisdbs/CCG | 2026-11-11 10:17:02 | 2026-11-11 10:17:23 | 2026-11-11 10:18:35 | 1768→433381 |
| dbs/CRG | 2026-11-11 10:38:22 | 2026-11-11 10:38:43 | 2026-11-11 10:39:55 | 13685→639809 |
| hisdbs/CRG | 2026-11-11 10:56:45 | 2026-11-11 10:57:07 | 2026-11-11 10:58:19 | 12142→452293 |

## 监控与回退边界

原每60秒采样、每小时只读汇报继续；CRG同状态约5分钟落盘，过期阈值360秒，实际限额每分钟回读；CCG状态阈值180秒。PSI缺失标未知。服务器时钟比客户端慢约100秒，本文事件均使用服务器时钟。CCG rc.12不提供HTTP服务。

本地component-events.json保存helper变更窗口，不将它们加入数据库计划停机白名单；SQL/API错误仍独立报告。CCG计数按PID分段，原容器OOM累计不重置；显式更新重启形成的新CCG启动基线不代表清除了历史OOM。DBS本次新OOM后CCG再次CRITICAL/new_oom_kill，保留该状态。

回退须逐个停止对应辅助写入者、核对同一容器身份和当前journal兼容性，仅原子替换对应旧二进制，再恢复辅助循环；不得跨ID导入原值，不删除journal绕过校验，不把当前版disable恢复原值当成普通进程停止。若状态已经变化，先读取当前实际控制项和兼容结果，不能盲目覆盖旧journal。

本次只证明发布附件安装、配置保持和所记录的短期探针结果。DBS发生非计划OOM，不能宣称整体稳定、数据全量校验或长周期业务验收通过。
