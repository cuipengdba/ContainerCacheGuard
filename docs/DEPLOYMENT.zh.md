# 容器和 Kubernetes 怎样部署

本文是部署与验收设计。实验机目前运行 containerd，kubelet 未运行；已有双容器实验不能称为 Kubernetes/OpenShift 驱逐验收通过。不要把以下示例直接套到业务环境，先替换路径、UID、资源预算和监控配置，在已授权的测试集群验收。

## 推荐的职责划分

数据库服务端负责 SQL、WAL 和复制协议。备份容器运行 pgcacheguard，写独立备份目录，并只读挂载数据库实际的数据卷。控制器不需要注入数据库进程，也不需要宿主机特权。

同一 Pod 的两个容器比较容易共享 Unix Socket、卷和 SELinux 标签，适合第一阶段落地；但是 Pod 被驱逐会影响整个 Pod，不能把两容器误认为两份节点资源或独立故障域。两个容器的 requests、limits 都要纳入容量规划。

独立备份 Pod/Job 能独立分配优先级、资源和生命周期，但须调度到数据库所在节点，挂载同一实际卷，并解决卷访问模式、UID 和 SELinux 标签。两个 Pod 各有一份相同内容的数据副本不满足同 inode 条件。RWO 的具体共享行为取决于 CSI，不能把 RWO 等同于任意多 Pod 都能同时写。[Kubernetes 卷访问模式说明](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#access-modes)

更长期的架构是在专用备用库或备份节点执行备份，仓库放独立物理设备，并保留本地可直接演练的完整恢复副本。当前 PGDATA 跨容器缓存处理依赖同一宿主机内核；不能通过在另一台机器挂同名路径，去控制原节点的本地页缓存。

## 最少需要哪些挂载

| 挂载 | 数据库容器 | 备份容器 | 原因 |
| --- | --- | --- | --- |
| PGDATA 实际卷 | 读写 | **只读** | 同 inode 的驻留查询和建议释放，不写源数据 |
| Unix Socket 目录 | 读写 | 能连接 Socket 的权限 | 当前实际复制连接身份核对要求本机共享 Socket |
| 备份仓库 | 通常不需要 | 读写 | 原生备份、父链及校验 |
| 独立运行目录 | 不需要 | 读写、允许执行 | 展开客户端、控制状态、日志和结果；约 40 MiB 客户端展开空间另加位图和日志 |
| 同盘任务共享锁目录 | 所有受控任务共同访问 | 读写 | 互斥安排同盘备份；未接入锁的 gissync 不会自动被锁住 |
| kubelet Summary 或认证文件 | 不需要 | 只读 | 使用真实驱逐相关内存指标 |

Socket 如果直接位于 PGDATA 内，也可以通过该只读目录连接已存在的 Socket，但恢复会移动 PGDATA；建议使用独立共享目录，避免路径生命周期混在一起。

## 身份和安全上下文

优先让备份进程使用数据库文件所有者的数字 UID。SQL 的 `replica` 账号和 Linux UID 是两回事，复制权限不会赋予文件驻留查询权限。建议 `runAsNonRoot: true`、`allowPrivilegeEscalation: false`、删除不需要的 capabilities，并使用集群允许的 seccomp 配置。

PGDATA 的只读挂载不妨碍文件所有者执行本工具的 mincore/fadvise 路径。OpenShift 随机 UID 如果与现有文件所有者不一致，不能仅依靠 fsGroup 可读就宣称驻留查询准确。应使用集群批准的 UID/存储权限方案，或者先验证内核权限条件；工具不明时拒绝工作。

还要检查 `pidfd_open` 是否存在并获 seccomp 允许。它用于客户端检查控制器存活，Linux 从 5.3 提供；当前观察器还使用 inotify，直接 PGDATA 发布使用 renameat2。静态程序不能补齐不存在的内核系统调用。[pidfd_open 接口](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)

同 Pod 不代表自动具备文件权限。SELinux、SCC、AppArmor、seccomp、只读文件系统、`noexec`、网络策略和 CSI 都需要验收。不要为了省事默认启用 privileged。

## 监控怎样接入

生产建议 `require_kubelet_stats=true`，选择下列一种来源：

1. `kubelet_summary_url`：验证 HTTPS 证书，通过受限 ServiceAccount 身份读取本节点 Summary；配置 CA 与 token 文件，不关闭 TLS 校验。
2. `kubelet_summary_file`：由已有可信节点监控组件定期原子更新 JSON；文件中的样本时间必须新鲜。该组件需要单独运维，复制一份静态 JSON 不能长期提供保护。

必须核对 Summary 确实来自数据库所在节点。令牌的 RBAC、kubelet 认证授权与证书要求取决于集群，不要把本地 root SSH 权限当成 Kubernetes API 已授权。

可选的 `cgroups` 读取 cgroup v2 的 memory.current/stat/events，提供工作集、active_file 和新 OOM kill 检测。如果备份容器无法看到数据库 cgroup，不能填写一个猜测的路径。只读挂整个宿主机 cgroup 的 hostPath 可能不符合 Pod Security/SCC；生产应由运维选择允许的只读监控方式。没有直接 cgroup 数据时，节点 Summary 的停止线仍可启用，但不能声称已经具备缺失的逐容器 active_file/OOM 监控。

Linux `/proc/meminfo` 的 MemAvailable 与 kubelet memory.available 不是同一指标。工具的本地实验回退会明确标识前者；生产针对驱逐的验收必须检查后者。

## 资源值怎样选择

不能把某次测试的 20 MiB/s 或 64 MiB/s 当作统一安全值。先测业务峰值、同盘可用带宽和延迟，再给备份读写预算。1.5 TB 在 24 小时内仅完成一次传输，平均至少需要约 17.4 MB/s；若按 1.5 TiB 则约 19.1 MB/s。这还没有计入估算、检查点、WAL、校验和停顿。同盘读写以及完整校验增加实际设备工作量，低于所需预算时应调整存储或备份架构，不能同时保证任意低速与固定窗口。

requests 应覆盖业务稳定工作集与合理缓存需求，limits 需结合节点实际容量；不要给一台几十 GiB 的节点配置几乎不可能兑现的数据库内存上限，然后依靠驱逐兜底。Guaranteed/Burstable 等 QoS 和 Priority 影响行为，但不保证在节点持续内存不足时永不驱逐。

`memory.high` 控制的是整个 cgroup 的内存回收和节流，不是普通文件缓存专用上限。`io.max` 给数据库 cgroup 限速会连 SQL、WAL 和检查点一起限制。先在专用备份容器控制客户端，再评估数据库侧兜底上限；记录回收 IO 和业务延迟，不在生产边压测边试猜值。

## 接入和回退顺序

1. 记录当前命令、调度、资源、备份链和恢复流程，保留原入口。
2. 用 `doctor` 与小任务核对 Socket、实例标识、PGDATA、UID、挂载、Summary 新鲜度、预算和锁。
3. 完成全量、连续增量、校验、合并及独立恢复启动，验证失败退出码确实被 DBS 处理。
4. 在与业务有代表性的负载下，执行相同限速的缓存对照和故障矩阵，确认节点安全余量与查询延迟。
5. 再安排受控时间窗切换调用路径；保留原工具入口和最后一条已验证备份链，出现异常先停新任务再回退。

如果只是设置 PATH，需确认 DBS 实际调用的是裸工具名，还是写死的绝对路径。兼容别名需要 `PGCACHEGUARD_CONFIG` 和正确的子命令名，不能把控制器二进制直接覆盖原生工具后仍让配置指向自身，造成递归执行。

现有 DBS 在恢复前可能移动旧 PGDATA 并异步压缩。这个 Python 线程不在本工具的子进程控制范围。启用 `allow_stopped_pgdata_restore` 只能保护工具自身的空目标 staging 发布，不能自动修复外部 Python 的删除、压缩、调度或错误处理；接入验收必须单独覆盖这些步骤。
