# rc.13：受限输出策略和可信报表怎么用

这版继续提供同一个静态程序，支持Linux amd64/arm64；核心无Python、PostgreSQL客户端、Web或Kubernetes网络依赖。可选的节点采集脚本在已有kubectl/Python3的管理机或宿主机运行，不放进每个业务容器。**自动范围模式不变；新受限策略只适用于人为确认低复用的备份输出，不用于源PGDATA。**

## 先分清两种“可用内存”

原来的`host_memavailable_bytes`来自宿主可见/proc/meminfo，保留诊断和原保护，不能冒充kubelet驱逐指标。新`metrics.node_signal.memory_available_bytes`来自Summary节点memory.availableBytes；节点UID、采样时间、有效内存hard/soft驱逐阈值和MemoryPressure一起进入状态。节点信号JSON均以字节、秒为单位。[Kubernetes官方说明](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/)解释工作集与inactive_file口径；94的实际Kubelet为1.26.12，不能照抄新版本默认值。

## 两种保护策略

`protection_profile`省略或`strict`：保持旧行为，默认要求有效IO PSI；原配置没有被静默弱化。历史明确设置某个可选阈值为0的语义保留，医生报告仍列出禁用保护。

`limited-output`：实验功能，只允许cgroup v2、`mode=paths`、显式`output_low_reuse=true`、绝对缓存预算和`trigger=budget`。只有IO PSI不可见时才允许这一项缺口，状态明确标记experimental；如果PSI可见仍按原阈值保护。须同时具备连续IO、文件refault、内存分类、脏页/写回、OOM、自身CPU/RSS及来源绑定的节点和真实业务p99。IO吞吐不等于等待时间，不能称为PSI替代品。[Linux PSI接口说明](https://docs.kernel.org/accounting/psi.html)定义了资源等待口径。

任何风险只会停止CCG新释放建议，**不会自动暂停pg_basebackup**；外部备份止损和业务容量仍需管理。节点available小于等于`max(hard,soft)+安全余量`或MemoryPressure为true就停止；soft宽限秒数用于展示，CCG提前停止，不等宽限耗尽。它不覆盖磁盘/inode/PID压力、所有分配峰值或其它节点服务，不能保证永不驱逐。

## 新参数逐项说明

| 参数 | 默认 / 含义 | 受限策略要求 |
| --- | --- | --- |
| instance_id | 空；人为指定稳定部署身份，1..128字节，字母数字开头，可含_.:/- | 必须；不同主机/命名空间/容器唯一，如lab94/puc/dbsbackup |
| protection_profile | 空等同strict | 显式limited-output才开放实验能力 |
| output_low_reuse | false；运营者对输出用途的声明，不是程序自动识别冷热 | true；只能挂载备份产物，勿把源PGDATA声明为产物 |
| node_signal | 空；来源绑定的节点JSON绝对路径 | 必须，可见真实普通文件，限制16KiB，禁止符号链接/FIFO |
| node_id | 空；实际Kubernetes Node UID，不是IP/节点名 | 必须与信号一致 |
| node_source_id | 空；可信采集器的稳定标识 | 必须与信号一致；这是配置绑定，非密码学认证 |
| node_safety_margin_bytes | 0；提前停止的额外余量 | 至少256MiB；示例1GiB不代表94已验收 |
| business_source_id | 空；已有business_signal扩展来源绑定 | 必须，对应被备份数据库的真实业务探针 |
| business_signal | 既有参数，业务JSON路径 | 必须，不能用简单SELECT 1延迟假装完整业务尾延迟 |
| signal_max_age_seconds | 10 | 1..30；Summary有缓存，应按真实采样周期设定，示例30 |
| max_business_p99_ms | 0；业务绝对SLO阈值 | 必须正数；100ms仅示例 |
| max_io_bytes_per_second | 0 | 必须正数；cgroup可见读写合计，非磁盘延迟 |
| max_file_refaults_per_second | 0 | 必须正数，需连续基线；不代表全部业务读盘 |

其余原有参数见[完整配置](CONFIGURATION.zh.md)和[rc.12手册](RC12-USAGE.zh.md)。受限策略还强制：扫描<=64MiB/s、建议<=8MiB/s、初始<=1MiB/s、区域<=1MiB、<=64操作/s、<=200目录项/s、工作片<=10ms、CPU预算<=5%单核、RSS<=128MiB；采样<=2秒、连续健康恢复与预热>=10秒、文件年龄和访问保护>=30秒、冷却>=60秒。脏页阈值>0，临界策略必须pause，不允许always触发。超限配置拒绝启动。速率是建议覆盖预算，不保证实际回收同样多；遍历操作也受预算限制。

## 准备节点来源

管理员在已获授权的宿主/管理机执行；本项目不创建RBAC，不建议给业务容器集群管理员凭据。脚本固定使用已有kubectl，读取Node对象、节点代理Summary和configz；现有账号必须具备相应读取权限（nodes/proxy权限本身很强，应由集群管理员评审）。失败不会刷新旧文件，过期后CCG暂停。

```sh
mkdir -p /opt/ccg-signals
python3 scripts/kubelet_signal.py --node k8smaster-3294 \
  --source-id lab94-kubelet --output /opt/ccg-signals/node.json --once
# 查看结果里的node_id，再写进两个备份配置；不是复制本手册的示例UID。
cat /opt/ccg-signals/node.json
# 核对后前台持续采集；需要守护运行时由现有服务管理器接管，不自动安装服务。
python3 scripts/kubelet_signal.py --node k8smaster-3294 \
  --source-id lab94-kubelet --output /opt/ccg-signals/node.json --interval 2 --max-age 30
```

configz中缺evictionHard时拒绝采集，不猜内核/版本默认值。KubeletConfiguration的soft映射为omitempty且无默认soft策略，实时configz省略soft时规范化为0；若提供soft memory.available却没有宽限值，则拒绝采集。[1.26.12配置定义](https://github.com/kubernetes/kubelet/blob/v0.26.12/config/v1beta1/types.go)与[驱逐阈值解析](https://github.com/kubernetes/kubernetes/blob/v1.26.12/pkg/kubelet/eviction/helpers.go)为依据。百分比基于Node内存capacity，向上取整；非有限数、负数和溢出拒绝。

节点JSON格式（值为示例，不能长期写死时间）：

```json
{"schema_version":1,"source_id":"lab94-kubelet","node_id":"actual-node-uid","time":"2026-11-11T03:00:00Z","memory_available_bytes":6442450944,"hard_available_bytes":1073741824,"soft_available_bytes":0,"soft_grace_seconds":0,"memory_pressure":false}
```

业务信号由你们现有监控采集，原子替换JSON，不要原地覆盖半个文件。格式为`{"time":"实时RFC3339时间","source_id":"dbs-business-probe","p99_ms":真实窗口p99,"pause":false}`。时间必须反映业务指标窗口的新鲜程度；信号重复时间只在有效期内接受，倒退暂停，未来超过1秒拒绝。修改身份需同步可信配置并受控重启CCG，不接受运行中悄悄换来源。不要用虚构数字填齐保护。

## 四容器怎么挂

DBS/HISDBS继续使用strict，先observe；加各自唯一instance_id。不要因备份输出实验成功就打开源PGDATA。dbsbackup、hisdbsbackup分别使用[DBS模板](../config/rc13-dbsbackup-limited-output.json)和[HISDBS模板](../config/rc13-hisdbsbackup-limited-output.json)，初始也是observe。改Node UID、实例、实际输出和业务信号路径，阈值按本机标定评审。

每个备份容器只挂自己的输出卷为`/backup`；把可信采集目录以**整个目录只读挂载**为`/signals`，不要用subPath绑定单文件（原子替换会换inode）。容器无需Kubernetes token。两个备份容器分别读对应业务信号；节点信号可以共用。四份状态、日志目录必须独立，日志仍每日一文件、30天/1GB。信号目录权限由管理员限制，业务/备份容器只读。

原有同Pod/跨Pod目录共享与Socket说明见[四容器接入](FOUR-CONTAINERS.zh.md)。当前94两个备份Pod是临时实验部署，**没有在本轮改成同Pod或注册YsP**；新模板不是已应用的Kubernetes清单。跨容器只针对同一底层inode的可见共享文件发释放建议；输出端不挂源PGDATA也不释放源页。CCG不把缓存转移到自己的cgroup，也不能强制清理PG shared_buffers或匿名内存。

```sh
./containercacheguard doctor --config /etc/ccg/output.json --require-ready
./containercacheguard run --config /etc/ccg/output.json
# observe确认输入和范围后，受控实验才改action=enforce并重启该CCG进程。
```

doctor就绪只代表输入齐全，可能仍因水位、预热或SLO暂停，不等于允许回收。doctor自己的run_id是本次检查的随机标识，不能当成常驻进程的run_id。

## 多容器小时图片

新日志自带run_id，cgroup_device/cgroup_inode只在当前实例上下文解释。配置instance_id应稳定且唯一；每次启动的随机run_id用于断开累计计数。旧日志继续支持单实例Markdown/JSON/SVG，但没有足够身份时拒绝混入多实例清单。

参考[来源清单](../config/rc13-report-sources.json)，将各角色每日日志拷到独立目录，修改绝对文件路径；一天多文件可以列多个，不能重复文件/实例。清单最多16实例、总1024文件和4096小时分组，清单自身<=1MiB；符号链接/FIFO等输入拒绝。清单与--log-directory/--log-file/--config不能混用。它是一次性离线处理，**不会自动启动小时定时任务**。

```sh
./containercacheguard report --manifest /reports/sources.json \
  --format svg --output /reports/2026-11-11.svg --timezone Asia/Shanghai
./containercacheguard report --manifest /reports/sources.json --format json > /reports/detail.json
```

SVG没有脚本、外链或服务，可直接浏览器打开/插入汇报。展示普通文件、匿名、共享内存采样峰值，节点余量采样最低、自身RSS/CPU峰值、模式/状态和缺口；柱长按每个小时归一，按数值比较，**不是按小时缺口补零的连续曲线**。匿名不等于缓存；共享页与file可能交叉，三项不可直接相加。未知、observe、暂停、重启/缺样均不能称为安全。详细建议字节、净下降、再次驻留和IO见JSON；不能把建议量当实际收益。

## 升级与回退

备份原程序、JSON和每日证据；先用新程序doctor，保留strict/observe、独立日志目录。停止旧CCG后启动新CCG，不能两个实例写同一个日志目录。只读文件挂载需要替换inode时由部署平台受控重建，别假定宿主换文件就已更新进程。回退先停止新CCG，恢复旧程序和旧JSON，rc.12不识别新字段，**不能直接拿rc.13配置启动旧程序**。数据库无须因此重启。当前版本没有自动修改94运行实例。
