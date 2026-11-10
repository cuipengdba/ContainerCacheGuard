# 标准监控、健康检查与四容器接入

从0.2.0-rc.9开始，已有只读Web监听同时提供`/metrics`、`/healthz`和`/readyz`。不额外开启监听，不扫描PGDATA来回答请求，不执行SQL，不修改cgroup，不保存长期时间序列。使用默认关闭Web的配置时，这些接口也不会开放。

## 先分清三种状态

| 想知道什么 | 看哪里 | 不能据此得出什么 |
| --- | --- | --- |
| CCG HTTP进程能响应吗 | `/healthz`，可响应即200 | 不能据此判断监控有效、文件缓存已下降或数据库正常 |
| 当前监控数据可用吗 | `/readyz`；有效且新鲜为200，否则503 | 观察模式、人工暂停或正常策略退让也可以监控就绪；不表示允许回收 |
| 当前允许发出建议吗 | `ccg_advice_allowed`或JSON的decision.allow | 允许不等于已找到合格范围，也不表示内核已经释放或业务安全 |

`/metrics`在指标错误或尚无样本时仍返回200，提供`ccg_monitoring_ready 0`等状态；Prometheus的`up=1`因此只表示成功抓取，不能替代数据就绪判断。所有新路由仅接受GET/HEAD，POST等方法不会触发任何控制动作。它们复用原有Basic认证、4个并发处理额度和读写超时。

## 开启及检查

```sh
# 沿用已有配置；先只绑定容器内回环地址。
containercacheguard run --config /etc/ccg/config.json --web-listen 127.0.0.1:9898
curl -f http://127.0.0.1:9898/healthz
curl -f http://127.0.0.1:9898/readyz
curl -f http://127.0.0.1:9898/metrics
```

如果配置了web_token_file，三个接口都要认证。例如`curl --user ccg http://127.0.0.1:9898/metrics`会提示输入密码，避免在命令中写明口令。远程抓取先配置受控网络和TLS反向代理；内置监听本身仍为HTTP，Basic不提供加密。非回环监听仍必须配token文件。端口配置为0时，实际监听地址记录在状态的`web_address`；它是容器监听地址，不是自动配置好的节点访问入口。

## 数据什么时候算新鲜

先有完整状态时间，且指标采样具有时间、无采集错误、当前内存读数可用。分别检查状态与指标时间；超过未来1秒的数据拒绝。取两者中较老时间计算年龄，新鲜窗口为`max(3 × sample_interval_ms, 1000毫秒)`。如默认1秒采样，窗口为3秒；显式60秒采样则窗口为180秒。

尚无样本、采集失败、关键当前内存缺失、时间异常或过期时，readyz返回503及固定分类reason，不把原始错误和路径放进健康响应。Prometheus不再输出过期/错误的资源数值和工作累计量，而是输出就绪与可用性为0。计数在恢复后重新出现，仍是该进程累计值；进程重启后自然重置，分析累计量应使用rate/increase而不是直接相减。

只缺少某个可选字段时，其它已知数据仍可使用；缺少的字段省略，并通过`ccg_metric_available{metric="字段名"} 0`说明。已知的0会正常输出。JSON新增`metrics.available_metrics`，键名与原JSON字段一致，true表示该字段可采集；整个样本若有error，字段存在也不能当成健康数据。首次速率没有连续基线时不可用。v1没有据此补造slab、OOM或refault。页面相应卡片显示未知，缺失字段的历史曲线独立断开。

## 常用指标看懂就够了

| 指标 | 类型/单位 | 如何理解 |
| --- | --- | --- |
| ccg_exporter_up | gauge，0/1 | HTTP导出器响应；不代表PG健康 |
| ccg_monitoring_ready | gauge，0/1 | 新鲜且有效的数据是否就绪 |
| ccg_snapshot_fresh / ccg_metrics_valid | gauge，0/1 | 分开看时间与采集有效性 |
| ccg_sample_age_seconds | gauge，秒 | 两个时间中较旧者的年龄；未来时间可能为负 |
| ccg_metric_available | gauge，0/1 | 固定字段的可用性，metric标签不来自路径或错误 |
| ccg_memory_current_bytes | gauge，字节 | 被监控cgroup计费，不是CCG自身RSS |
| ccg_memory_file_bytes | gauge，字节 | 内核file统计，包含shmem |
| ccg_memory_ordinary_file_bytes | gauge，字节 | max(file-shmem,0)，两字段均可用才输出 |
| ccg_memory_anon_bytes / ccg_memory_shmem_bytes / ccg_memory_slab_bytes | gauge，字节 | 这些类型当前只监测，不能由文件建议接口清理 |
| ccg_visible_memory_limit_bytes | gauge，字节 | 可见最严有限额，可能来自父组；不能直接把本容器current/这个值当成父组比例 |
| ccg_pressured_ancestor_memory_current_bytes / ccg_pressured_ancestor_memory_limit_bytes | gauge，字节 | 父组必须使用这对值判断 |
| ccg_host_memavailable_bytes | gauge，字节 | 可见proc MemAvailable，**不是kubelet memory.available** |
| ccg_cgroup_io_bytes_total / ccg_cgroup_io_bytes_per_second | counter字节 / gauge字节每秒 | 可见io.stat合计，堆叠设备可能重复计量，不是物理盘基准测试 |
| ccg_cgroup_file_refaults_total / ccg_cgroup_file_refaults_per_second | counter累计 / gauge每秒 | 文件页再次访问的信号，不能单独归因于CCG |
| ccg_advice_allowed | gauge，0/1 | 当前有效策略允许建议；错误/过期时不输出，不能把缺失解释成停止成功 |
| ccg_policy_state | 固定state标签的gauge | OBSERVE/IDLE/CONTROL/CONTROL_CRITICAL/BACKOFF/CRITICAL/SUSPENDED/STOPPED/UNKNOWN的有界状态 |
| ccg_file_advice_bytes_total | counter，字节 | 内核接受的建议范围累计量，不是实际释放内存 |
| ccg_observed_net_reduction_bytes_total | counter，字节 | 区域复查观察净下降，可能混入自然回收/并发访问 |
| ccg_observed_reappeared_bytes_total | counter，字节 | 区域再次驻留量；与文件页refault不是同一指标 |
| ccg_pending_hint_paths / ccg_hint_queue_dropped_total | gauge路径数 / counter累计 | 判断新文件发现是否积压或提示丢失 |
| ccg_inventory_sweeps_completed_total | counter，次 | 0表示尚无一次完整遍历，不能声称覆盖全部目录 |
| ccg_recovery_enabled / ccg_recovery_pending | gauge，0/1 | 连续健康恢复门是否启用/等待 |
| ccg_recovery_healthy_seconds / ccg_recovery_required_seconds | gauge，秒 | 恢复进度和配置窗口 |
| ccg_guard_stops_total / ccg_recovery_resumes_total | counter，次 | 只在恢复门启用时输出；记录连续风险阶段进入和窗口完成，不按每秒重复算一次 |
| ccg_process_resident_memory_bytes / ccg_process_cpu_seconds_total | gauge字节 / counter秒 | CCG自身资源成本 |

完整导出含HELP/TYPE；JSON状态仍包含详细reason。版本label限定为短构建标识，其它标签只有固定字段与状态枚举，不导出PGDATA、容器ID、原始错误或SQL为标签。当前没有自带Kubelet驱逐事件计数，需结合已有Kubernetes事件、kube-state-metrics、节点监控和真实SQL探针。

## Prometheus抓取四个实例

示意见[配置文件](../deploy/prometheus.example.yml)。四个目标标签分别标为dbs、hisdbs、dbsbackup、hisdbsbackup；域名和TLS代理都需要换成实际环境。此示例不代表已在94部署Prometheus、代理或ServiceMonitor。镜像内/同Pod/跨Pod的挂载及cgroup范围仍按[四容器说明](FOUR-CONTAINERS.zh.md)核对：共享卷不自动共享缓存计费，采到哪个cgroup必须由doctor确认。

Prometheus建议先用15秒抓取间隔，再按实际CPU、内存与数据频率评估；并发抓取不会触发额外目录遍历，但HTTP解析和文本编码仍有成本。CI的编码微基准只测固定快照到文本，不等于整个程序、HTTP负载或真实数据库开销验收。

## 告警示例与适用边界

```promql
# 目标不可抓取；还要设置适合现场的持续时间。
up{job="containercacheguard"} == 0

# 进程可响应但数据失效，不能看成零缓存。
ccg_monitoring_ready{job="containercacheguard"} == 0

# 发现提示丢弃：应检查覆盖与预算，不能直接认定备份损坏。
increase(ccg_hint_queue_dropped_total[5m]) > 0

# 有建议，但窗口内没观察到净下降：只是调查线索。
(rate(ccg_file_advice_bytes_total[5m]) > 0)
and (rate(ccg_observed_net_reduction_bytes_total[5m]) == 0)
```

告警阈值与持续时间需要现场标定。匿名内存升高、缓存重新驻留、设备忙碌和SQL失败要放在同一时间窗分析；不能仅凭建议字节增加或没有OOM就报成功。指标消失应先看monitoring_ready、up和可用性，不能把PromQL空结果当作零。

## Kubernetes探针不要误接到数据库

**CCG与DBS在同一业务容器运行时，不要把CCG healthz失败作为这个业务容器的liveness条件。** 该配置可能因治理程序故障而重启数据库。使用监控告警，或由进程管理器只拉起CCG，不让CCG代管数据库进程。

同Pod独立sidecar时，任一容器readiness失败都可能影响整个Pod的服务就绪。缓存治理不是数据库提供服务的前提，因此不要因为readyz暂时503就把数据库Pod摘流量。只在独立治理Pod中评估使用它作监测就绪；或者由外部监控读取，而不接入业务Pod的readiness。不会为了增加这几个接口自动修改任何Pod探针。

本轮接口功能回归不能代替PG14真实数据、业务SLO、48小时或零驱逐验收。

机制参考：[Prometheus文本格式](https://prometheus.io/docs/instrumenting/exposition_formats/)、[Kubernetes探针](https://kubernetes.io/docs/concepts/workloads/pods/probes/)。
