# rc.18：数据库与备份同容器的缓存治理

2026-11-17，需求C040。rc.18在一个CCG内增加路径角色、任务阶段保护、可选节点采集命令、同级内核目标公平调度及周期日志精简。候选测试与现场验收是两件事；具体实测以交付记录和Release附件为准。没有在171或94部署此版。

## 1. 放在哪里，解决哪一段

DBS、HISDBS各保留原业务容器。数据库、DBS、原备份工具和一个CCG在同一容器；不另建备份容器，也不要求改数据库入口。PG14的备份、校验、恢复工具仍使用原环境提供的程序。

CCG按目录决定哪些文件范围可以发出DONTNEED建议；所有路径共享一个工作队列和CPU、RSS、扫描、建议、操作及时间预算。备份目录和PGDATA的额度不是两份，路径角色也不是独立cgroup、硬内存限额或硬IO隔离。

常态回收先处理低复用输出，源目录至少保留60秒文件年龄和访问保护。热点可能通过内存映射访问，低磁盘读取不证明它冷；驻留查询同样不提供完整冷热/干净页证明。压低普通文件缓存不能解决所有anon、shmem、slab或容量问题。

## 2. 目录和实例配置

完整observe模板：[DBS](../deploy/rc18/dbs.json)、[HISDBS](../deploy/rc18/hisdbs.json)。它们用于改成实际路径后检查，未在服务器安装，也不是已标定的业务参数。源目录、输出目录必须真实存在；输出目录放在PGDATA之外。

先核对实际PGDATA、表空间、备份输出、进程UID、挂载、cgroup和DBS原清理方式。`pg_basebackup -D`指定的是**备份输出**，不能误填成数据库源目录。

新增配置示意，合并到完整配置中：

```json
{
  "path_policies": [
    {"path":"/data/pgdata","role":"database-source","protect_seconds":120,"min_file_age_seconds":60},
    {"path":"/backup","role":"backup-output","protect_seconds":30,"min_file_age_seconds":30,"low_reuse":true},
    {"path":"/backup/keep-hot","role":"excluded"}
  ],
  "task_signal_directory":"/opt/ccg/tasks",
  "task_source_id":"dbs-tasks",
  "task_max_age_seconds":10,
  "max_active_tasks":32,
  "log_kernel_snapshots":false
}
```

| 参数 | 含义、默认值与边界 |
| --- | --- |
| path_policies | 默认空，最多64项；paths/auto模式都可用，但必须位于显式paths或auto_roots内，不引入额外根 |
| path | 已授权的规范绝对目录；不接受符号链接根；启动绑定真实设备/inode，建议前重验根身份 |
| role | database-source、backup-output、excluded；不同活动角色不能嵌套冲突，excluded可嵌套且始终更保守 |
| protect_seconds | 角色保护期，0..86400秒；实际取全局与角色的较大值，源角色再至少60秒 |
| min_file_age_seconds | 角色文件年龄，0..86400秒；同样不会减小全局保护，源角色至少60秒 |
| low_reuse | 声明已评估输出低复用；不表示CCG已经证明所有页都是冷页，不能覆盖校验/恢复阶段保护 |
| limited_output | 默认false；仅低复用backup-output可显式进入无PSI实验资格，见第5节，不改变源角色strict保护 |
| task_signal_directory | 默认不启用；当前CCG UID所有的0700真实目录，每个容器自己一份；不能挂同一目录给不相关实例 |
| task_source_id | 与包装器一致的来源ID；同时必须配置instance_id |
| task_max_age_seconds | 默认10，范围3..30秒；文件过期、PID重用、乱序或身份不符都记unknown，停止新文件建议 |
| max_active_tasks | 默认32，范围1..128；目录扫描同样有界，临时/无关文件也计入溢出门槛，溢出不当作无任务 |
| log_kernel_snapshots | 默认false；周期日志保留内核结果和episode引用，完整快照保留在状态、节点journal及agent关键事件；true恢复周期全量输出 |

规范路径冲突、重复定义和已知同根inode角色冲突会拒绝启动。同角色文件硬链接别名在有界索引内去重；跨角色别名记冲突并拒绝相关建议，历史冲突表有界，溢出停止新的候选建议。输出硬链接在建议前另行拒绝，因为其其他别名未必被发现。它不是文件系统全量别名审计，累计扫描量不能写成唯一覆盖率。

候选按3:1有界偏向输出；每第四次保留普通轮询机会，优先查找最多16项，仍受同一个操作和时间预算。不是给输出永久抢占权，也不承诺每个目录固定吞吐。事件、coverage_audit和path_roles带路径角色；候选立即/延迟复查沿用原有机制。

## 3. 怎么接到原DBS容器

1. 放入对应架构二进制，保留旧版用于回退。每个数据库容器只运行一个CCG；交给原进程管理器看护，不替代原业务PID1。
2. 为CCG实际UID创建配置、私有状态、日志和任务目录。数据库文件需允许该UID读取和真实驻留查询，不以root名称猜测权限。不要递归修改PGDATA权限。
3. 修改模板中的PGDATA、备份路径、实例ID、状态和日志路径。任务目录0700；状态/日志建议位于私有持久目录。配置改动需重启CCG，不重启数据库。
4. 先检查并observe。`doctor --require-ready`是保护输入预检，不能代替全量目录扫描、业务基线或生产资格。无PSI时strict源策略可能持续暂停，应保留原因。

```sh
containercacheguard check-config --config /etc/ccg/dbs.json
containercacheguard doctor --config /etc/ccg/dbs.json --require-ready
containercacheguard run --config /etc/ccg/dbs.json --action observe
```

HISDBS用自己的配置。enforce之前核对IO/refault/业务/节点保护阈值，做配对对照；不要同时运行CCG与vmtouch/nocache处理同一文件范围。observe可与原方案共存观察，但不能归因回收收益。这里没有自动修改DBS源码、备份时刻、限速、CPU、内存或Pod配置的动作。

## 4. 可选任务包装：让校验、恢复的数据先保留

```sh
containercacheguard task-run \
  --directory /opt/ccg/tasks --source-id dbs-tasks --instance-id dbs \
  --stage backup --root /data/pgdata --root /backup \
  -- pg_basebackup -h /var/run/postgresql -U backup_user -D /backup/full -X stream --max-rate=20M

containercacheguard task-run \
  --directory /opt/ccg/tasks --source-id dbs-tasks --instance-id dbs \
  --stage verify --root /backup \
  -- pg_verifybackup /backup/full
```

限速和连接参数只是示例；凭据用原有安全配置，不放进命令日志。`--`后是原程序及其全部参数，不拼shell字符串，不记录完整命令行。stdout/stderr/stdin原样连接，返回原程序退出码；信号转发到单独任务进程组。信号结束返回常用的128+信号值，并不伪装成成功。

| 参数 | 用法 |
| --- | --- |
| --directory | 预创建0700任务目录，与CCG配置一致 |
| --source-id / --instance-id | 来源、实例必须与CCG一致 |
| --stage | backup（默认）、verify、compress、transfer、sync、restore之一 |
| --root | 本阶段访问的目录，可重复，最多64项；不能扩大CCG扫描授权 |

包装器在启动子程序前登记任务，每2秒心跳；每个并发任务单独随机ID，绑定boot、PID和进程启动时间。正常结束清除自己的标记；崩溃/心跳失败留下过期标记，CCG保守暂停。运行中遥测失败不会杀业务任务或改写其退出码，stderr报错；确认原任务确已结束、PID身份不符且没有在途任务后，由运维处理该任务标记，不能据此清除内核journal或OOM历史。

verify、compress、transfer、restore对指定路径停止**新的文件建议**，已有延迟复查继续；backup、sync仍受文件改写/访问保护。unknown暂停所有新文件建议。调用者必须声明所有会读的路径，漏接其他校验任务不能被包装器凭空发现。任务目录没有标记表示已接入任务集合为空，不证明整个DBS无其他任务。此版未内置PG SQL自动进度适配；需明确包装接入。任务目录不与数据库目录混放。

包装器是可选接入，不要求CCG启动业务。它只适用于已验证的前台命令；会自行daemonize、脱离进程组或后台派生长期任务的程序，不能把父进程退出当作完整任务结束，需要专门适配，暂不验收。**路径阶段不能约束memory.reclaim**，内核资格单独按整个cgroup的保护检查。

## 5. 可选节点采集，不开Web服务

同一静态二进制提供`node-collect`。只有此可选命令需要节点侧既有kubectl和只读认证，普通CCG run不需要。它读取node、stats/summary、configz，不创建RBAC、不改配置，显式保持TLS验证。节点管理权限需按集群制度配置，API proxy权限应最小化。

```sh
containercacheguard node-collect \
  --node YOUR_NODE_NAME --source-id kubelet-local \
  --output /opt/ccg-signals/dbs-node-v2.json \
  --legacy-output /opt/ccg-signals/dbs-node-v1.json \
  --business-signal /opt/ccg-signals/dbs-business.json \
  --business-source-id sql-api-dbs \
  --path /HOST/ACTUAL/PGDATA --path /HOST/ACTUAL/BACKUP
```

| 参数 | 含义 |
| --- | --- |
| --node / --source-id | 本机Kubernetes节点名和稳定来源ID；核对真实bootID，不能在其他节点代写 |
| --output | schema2文件给内核后端/逐路径磁盘保护；父目录当前UID所有、0700，目录预创建 |
| --legacy-output | 可选schema1文件给现有文件后端；不是简单改版本号，按最低实际字节余量选择约束并保留其原时间 |
| --business-signal / --business-source-id | 可选现有业务采样来源，原始time/source_id/samples/error_rate；内核另从同一业务文件读取P99，不能生成固定健康值 |
| --path | 当前节点命名空间中的真实源/输出挂载路径，可重复最多16项；不是容器路径字符串直接照抄 |
| --interval | 默认2s，范围1..5s；含API调用耗时，实际周期应实测；过慢会失效，不能刷新旧时间掩盖 |
| --once | 单轮采集；首轮没有磁盘差分，磁盘指标为unknown；持续运行至少两轮才可能有盘指标 |

适配器当前接受已审阅的Linux kubelet v1.24—v1.31 Summary/configz形态；超出范围拒绝更新，不能删除版本检查绕过。各发行版仍需实环境核对。完整有效配置有指纹，缺失必要字段不猜默认；缺软阈值表示该有效配置未启用软策略。百分比按Summary available+workingSet的对应容量换算；启用pods强制且存在硬内存阈值时，另取SystemContainers/pods原始指标，不能用Node.Status.Allocatable替代。最低回收量加入停止线是保守余量，并非原始硬驱逐线已改变；软宽限仍单独保留。

磁盘映射只接受能确认的ext3/4、xfs块设备；overlay、NFS、多设备文件系统或映射不明记unknown。对分区/设备映射关联底层块设备，最多16层/设备；层间取await和队列的最大值，不把父子IO重复相加。计数缺失、重置、设备集合改变或区间超过5秒不产生健康差分。此版不自动挑最大分区、不写盘标定，也不承诺识别全部存储拓扑。不能用无磁盘读推断冷页。

采集失败不续期旧输出，stderr打印原因；管理器需采集该命令stdout/stderr并看护。业务文件可以包含额外指标，采集器只取所需字段，必须当前UID所有且非组/全局可写。两个业务实例应各自绑定业务文件/输出，节点agent仍唯一；需测量重复节点查询的总成本。

逐路径`limited_output=true`需要原limited-output的全部保守预算和来源绑定，并额外设置`kernel_reclaim.signal_file/signal_source_id/disk_devices/max_disk_await_ms/max_disk_queue`用于真实盘保护，即使内核后端关闭也要填写这些信号字段。设备集合必须与采集结果严格匹配；磁盘/业务时间最多5秒，至少20个真实业务样本且错误率0。source仍strict。由于无PSI端到端生产资格尚未完成，这仍是显式实验功能，默认不开启。

## 6. memory.reclaim和日志变化

内核后端默认关闭，仍需受限节点reclaim-agent。同级目标按已通过资格的风险和等待时长排序；等待超过风险加分上限后不会永远被新目标插队。显式priority仍先比较，公平排序不授予资格、不绕过停让确认。节点串行、额度、每cgroup压力事件一次、持久journal、UNKNOWN、历史OOM和恢复门槛保留，任务ID不参与事件重置。

无PSI内核配置现在还要求`disk_devices`，与schema2采集文件匹配；旧实验配置漏填会明确报错，这是保护性收紧。普通旧配置无需增加角色即可继续使用。内核覆盖整个容器，不能承诺只清备份、WAL被排除或不会影响热点。

周期日志默认引用episode并报告快照数量，避免每10秒重写多份完整内存快照。状态文件、持久journal和agent关键事件保存完整证据；30天/1GB日志清理不删除journal。离线report/compare仍读取原有顶层指标/计数，不把日志变小当作整个守护进程CPU/IO降低的证明。无HTTP端口。

## 7. 验证、上线与回退

候选应通过原生双架构race/CLI、至少1GiB回收及120秒持续性、内核单次/重启去重、同容器PG14写入+备份+校验+独立恢复。单容器实验数据量和真实DBS不同，未证明缓存峰值降低30%、低影响SLO、无PSI真实共享盘或48小时不驱逐。实际数字、失败和延期见Release，不把曾运行旧版的94记录算作本版部署。

后续现场先observe、再小范围enforce，事先填写备份窗口、SQL/API绝对SLO、相对P99、磁盘峰值、RSS/CPU、有效节点覆盖和回退触发线。回退仅停止CCG新请求并等待在途辅助进程退出，换回旧CCG和已保存配置；不重启数据库、不drop_caches、不清OOM或journal。同一范围恢复旧清理前确认CCG已退出。

依据：[Kubernetes v1.31内存信号与阈值实现](https://github.com/kubernetes/kubernetes/blob/v1.31.0/pkg/kubelet/eviction/helpers.go)、[Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)。[全部原配置项](CONFIGURATION.zh.md)、[原始rc.18规划与验收门槛](RC18-PLAN.zh.md)保留供核对。
