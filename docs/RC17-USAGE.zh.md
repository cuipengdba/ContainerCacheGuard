# rc.17：驱逐前单次内核主动回收

本页对应正在验证的rc.17代码。候选验证完成前不作为生产推荐。旧配置默认`kernel_reclaim.enabled=false`，原文件回收行为保持兼容；开启新后端需要额外部署和真实信号，单纯替换二进制不会自动启用。

## 它做什么

平时仍用逐文件mincore/fadvise。在容器或可见父级接近memory.max，或真实kubelet驱逐余量提前不足时，经连续确认、业务和IO保护、文件工作线程停让后，节点执行器请求一次memory.reclaim。调用覆盖整个登记的叶子cgroup，**PGDATA排除、WAL排除及文件冷热判断不约束这次内核操作**。它可能影响业务热点或触发IO；不是准确释放指定字节，也不是保证阻止驱逐。

内核可以多回收或少回收；不足时可能返回EAGAIN。任何返回、EINTR、短写都不自动重试。请求前先fsync持久登记，重启时未决调用转UNKNOWN，绝不当新事件再刷一次。看门狗只发出终止辅助进程的请求，不能保证打断不可中断的内核IO；锁随辅助进程保留到真实退出。[内核接口说明](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-interface-files)。

## 四容器怎么接入

DBS、HISDBS、dbsbackup、hisdbsbackup各运行一个CCG `run`，属于自己的容器cgroup。节点另运行**同一个静态二进制**的`reclaim-agent`子命令。agent使用宿主机PID/cgroup视图，可写cgroup2，并仅登记这四个**叶子**目标；不能登记宿主根、Pod父级或其他容器。客户端不需要写cgroup权限。每个目标有独立配置、状态、日志；节点journal持久保存，不放在容器临时层。

```text
DBS内CCG ───────────┐
HISDBS内CCG ────────┼─ Unix Socket ─ 节点reclaim-agent ─ 固定登记的容器cgroup
dbsbackup内CCG ─────┤                       └ 单个在途内核辅助进程
hisdbsbackup内CCG ──┘
```

它不改变数据库与备份的同Pod/共享卷布局，也不跨目录释放别的cgroup。源端文件页按首次分配者计费，备份端读同一文件可能仍计在源cgroup；按路径可见性不能推断归属。agent直接回收登记的实际计费cgroup。

节点应有唯一agent，以宿主PID命名空间及完整cgroup2视图启动。Kubernetes/OpenShift的安全上下文、SELinux/SCC和具体CRI路径需在授权测试节点验证。不要给所有业务容器开放宿主cgroup写权限；不要仅因`root`就认为一定可写。首版Unix Socket权限为0600，非root客户端用agent的`socket_gid`指定专用共享组，Socket变0660；父目录由agent UID所有且不可被组/其他人写，客户端须有目录执行位。Socket路径可挂载同一个hostPath目录给四容器，不需要共享PGDATA给agent。

## 生成配置并启动

先按原有`init-config`生成四份配置，保持`action=observe`检查真实cgroup和业务信号。下面是**需要合并进完整CCG配置**的增量字段，示例数值不是生产已标定阈值：

```json
{
  "action":"enforce",
  "interval_ms":1000,
  "warmup_seconds":60,
  "node_id":"your-real-node-id",
  "business_signal":"/run/ccg-signals/hisdbs-business.json",
  "business_source_id":"hisdbs-sql-api",
  "max_business_p99_ms":200,
  "max_io_bytes_per_second":52428800,
  "max_file_refaults_per_second":10000,
  "max_io_psi_avg10":10,
  "kernel_reclaim":{
    "enabled":true,
    "executor":"node-agent",
    "guard_profile":"strict",
    "scope":"cgroup",
    "target":"hisdbs",
    "socket":"/run/containercacheguard/reclaim.sock",
    "signal_file":"/run/ccg-signals/hisdbs-kernel.json",
    "signal_source_id":"your-trusted-node-collector",
    "state_directory":"/var/lib/containercacheguard/reclaim"
  }
}
```

DBS与两个备份容器改为各自target、business/signal文件和实例身份。agent读取的是宿主可见配置文件，内含signal/business路径也必须在agent命名空间指向相同真实文件；业务容器可通过只读挂载获得相同路径。agent启动时读取配置，不支持运行时热变更。

agent配置：将下面四个cgroup占位值替换为`crictl inspect`/`/proc/<容器宿主PID>/cgroup`核实的实际systemd或cgroupfs叶子路径；容器重建后inode变化，须重新登记。不要照抄Docker/K8s路径。

```json
{
  "socket":"/run/containercacheguard/reclaim.sock",
  "socket_gid":9000,
  "state_directory":"/var/lib/containercacheguard/reclaim",
  "log_directory":"/var/log/containercacheguard-agent",
  "targets":[
    {"name":"dbsbackup","cgroup":"/sys/fs/cgroup/REPLACE_DBSBACKUP_LEAF","config":"/etc/ccg/dbsbackup.json","peer_uid":0,"priority":0},
    {"name":"hisdbsbackup","cgroup":"/sys/fs/cgroup/REPLACE_HISDBSBACKUP_LEAF","config":"/etc/ccg/hisdbsbackup.json","peer_uid":0,"priority":1},
    {"name":"dbs","cgroup":"/sys/fs/cgroup/REPLACE_DBS_LEAF","config":"/etc/ccg/dbs.json","peer_uid":0,"priority":10},
    {"name":"hisdbs","cgroup":"/sys/fs/cgroup/REPLACE_HISDBS_LEAF","config":"/etc/ccg/hisdbs.json","peer_uid":0,"priority":11}
  ]
}
```

`peer_uid`填**实际CCG进程的宿主UID**，不是默认猜测数据库UID。rootless/user namespace和沙箱运行时尚未验收。priority越小越先；它只影响已满足全部资格并已停让的目标，不能迫使没通过资格的备份先执行。节点同一时刻只有一个写入，每30秒至多一次，5分钟最多请求1GiB。所有agent必须共享宿主`/run/containercacheguard-node`的固定锁及相同持久journal，不可用私有挂载绕过。

```sh
# 由节点管理员创建受保护目录。不要改业务数据目录权限。
install -d -m 0755 /run/containercacheguard
install -d -m 0700 /run/containercacheguard-node /var/lib/containercacheguard/reclaim
containercacheguard check-config --config /etc/ccg/hisdbs.json
containercacheguard doctor --config /etc/ccg/hisdbs.json --require-ready
# doctor只读；它不能替代agent宿主视图检查，也不证明实际写权限/收益。
containercacheguard reclaim-agent --config /etc/ccg/agent.json
# 各容器用原来的进程监管方式运行，不需要重启PostgreSQL。
containercacheguard run --config /etc/ccg/hisdbs.json
```

## 信号必须来自真实监控

此版本不内置kubelet认证、API服务或采集HTTP。既有`scripts/kubelet_signal.py`输出schema1，**不能直接拿来冒充下面的schema2**。需要节点可信采集器结合实际kubelet配置、Node/allocatable原始信号、真实SQL/API采样和（无PSI时）物理盘指标生成JSON；当前交付包含契约和隔离测试生产者，不包含已验收的生产schema2采集器。未接入即不执行内核回收，不能填写固定的“健康数值”绕过保护。

schema2的必填项：`schema_version=2`；固定`source_id/node_id`；宿主`boot_id`；`time`；实际驱逐配置指纹`config_version`；布尔`allocatable_required`和`memory_pressure`；`constraints`数组含node，若allocatable实际有效还必须含allocatable。每项有`kind/time/available_bytes/hard_bytes/soft_bytes`，原始时间不可被包装刷新。关闭的hard/soft阈值明确填0，未配置/未采到不能假填0。比例阈值换算须用实际容量，不能借宿主MemAvailable替代kubelet信号。Node.MemoryPressure已为真或hard线已到达时，首版停止新内核操作并报警，不能称作临门救援已经可控。

业务字段为`business_time/business_samples/business_error_rate`；时间须与独立business文件中的真实原始`time`相等，最近有效样本至少20、错误率为0，P99不超配置。business文件沿用`{time,source_id,p99_ms,pause}`。agent要求配置、journal、信号由其UID所有且组/其他人不可写；生产root agent可用root:root 0644信号，业务容器只读。若采集器使用独立UID，需要经审计的受控写入/发布路径，不能chmod777。

## 全部新增控制参数

以下均在`kernel_reclaim`中；字节用整数bytes，比例0—1。未写字段继承`init-config`默认值。

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| enabled | false | 必须显式启用；action=observe不会调用 |
| executor / scope | node-agent / cgroup | 首版固定；没有self写入公开命令 |
| guard_profile | strict | 或显式实验no-psi-guarded |
| target / socket | 空 / /run/containercacheguard/reclaim.sock | 固定登记名与共享Unix Socket |
| signal_file / signal_source_id | 空 | 可信schema2文件与来源，不是旧node_signal |
| state_directory | /var/lib/containercacheguard/reclaim | 与agent配置相同；目录0700，必须持久化 |
| max_calls_per_episode | 1 | 首版固定1，失败也消耗尝试 |
| memory_trigger_ratio / memory_rearm_ratio | 0.90 / 0.85 | 容器及有限父级分别检查，不把用量相加 |
| min_headroom_bytes | 1GiB | 任一级剩余内存少于此值提前预警 |
| predict_seconds | 20 | 基于连续正增长的提前量；预测不是保证 |
| node_trigger_margin_bytes / node_rearm_extra_bytes | 1GiB / 512MiB | 真实驱逐线以上余量及恢复带，快速增长会提前触发 |
| confirm_samples | 3 | agent每秒采样连续确认；客户端采样必须≤1秒 |
| request_min_bytes / request_max_bytes | 64MiB / 512MiB | 64KiB倍数；首版上限可配至1GiB；无PSI≤128MiB |
| min_file_estimate_bytes / file_budget_ratio | 512MiB / 0.25 | F=max(file−shmem−dirty−writeback,0)的资格与比例；F不是冷页证明 |
| call_watchdog_ms | 2000 | 100—5000ms；超时结果未知，不保证终止内核IO |
| recheck_seconds | [1,5,30,120] | 自实际返回时刻计时，记录实际采样时间，1—8项且递增 |
| rearm_healthy_seconds / min_episode_interval_seconds | 60 / 300 | 持续健康及最短事件间隔，两个条件都满足才重新武装 |
| require_swap_disabled | true | 固定true；swap.max和swap.current必须都为0，不自动改swap |
| max_memory_psi_avg10 | 10 | 严格策略要求真实PSI，过高/无效停止；IO PSI沿用外层配置 |
| max_disk_await_ms / max_disk_queue | 0 / 0 | 无PSI实验必须显式设正阈值并提供真实、新鲜的盘指标 |
| min_business_samples | 20 | 20—1000000；真实P99样本覆盖，默认不容许错误 |

请求量为缺口、单次上限、F比例和节点剩余请求预算四者最小值；不足最小请求则不写。它不构成磁盘IO限速，沿用已有CRG/cgroup IO设置。非文件占用过多、旧OOM未处理、业务变慢、脏页/IO/refault偏高、信号过期等情况均可拒绝调用。

`no-psi-guarded`只在确认io.pressure接口不存在时，用≤5秒的`disk_time/disk_await_ms/disk_queue`以及真实业务、IO、refault替代；接口权限错误/解析失败不算“确认不存在”。有PSI时仍使用实际PSI保护。它不是自动降级或生产等效资格。

## 看日志、异常和回退

原CCG状态新增`kernel_reclaim`，agent每日日志中的`event=kernel_reclaim`记录阶段、原因、cgroup身份、episode、请求与估算、写入返回和1/5/30/120秒指标。`accepted_request_not_measured_bytes`只表示内核接受；`write_failed_or_partial_no_retry`可能已有部分回收，不能再次补齐。UNKNOWN一直保持该目标文件建议停让，需人工查明在途进程与影响，不自动清journal“恢复”。

只有容器/各父级回到恢复线下、真实节点余量恢复并连续健康60秒，且距离本次开始至少300秒，才重新武装。历史OOM不被自动洗掉；本版不提供清零OOM或跳过历史的开关。已确认的历史问题如何解除需后续独立流程，不能因此重建业务容器掩盖证据。

启用后agent失联/状态过期，客户端暂停文件建议；防止两后端并发时失去控制。回退时先停止新请求，确认辅助进程真正退出，保留journal和日志，然后把各容器`kernel_reclaim.enabled=false`并重启**CCG进程**，恢复旧常态流程。不重启数据库、不改memory.max/high、CPU或IO配额。不能在回退时删持久记录后再启用以绕过去重。

agent日志遵守每天一个文件、30天/1GB；journal独立管理，不跟日志滚动删除。未决身份历史上限128，满了停止并要求审查；目标最多32，复查最多8，Socket处理最多8个并发和4096字节请求、有限超时。权限或写日志失败按停止处理。

## 验收边界

原生双架构回归和真实内核隔离结果将写入交付文档及Release附件。隔离实验使用合成node信号隔离测试cgroup触发，不等于真实kubelet驱逐实验；SQL/API来自独立PG14探针，不是1200模式实际重放。生产schema2采集器、无PSI真实盘关联、94四角色匹配压力/备份恢复及48小时均需另行验收。inactive_file本来就可能被kubelet扣除，必须分别看容器总内存、文件缓存与节点驱逐余量。
