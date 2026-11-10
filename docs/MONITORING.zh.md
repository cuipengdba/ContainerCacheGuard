# 无Web服务的监控：日志与状态文件

从0.2.0-rc.11开始，按用户要求取消内置Web页面、HTTP API、Prometheus和健康HTTP端点。程序没有HTTP监听，不需要端口、认证令牌或网页历史缓冲。rc.9/rc.10的旧HTTP接入方式不适用于本版；已安装的旧进程不会因源码发布自动停止，需要单独升级CCG。

## 每天的运行情况看日志

默认每天一个JSON行文件，控制台同时输出，最多30个日历日、总量1GB。查看命令、清理方式与配置见[日志手册](LOGGING.zh.md)。每条状态记录包含中文`message`、`level`、便于阅读的MiB摘要、完整原始指标和保护状态；`since_previous_log`表示距上次记录的累计计数变化。

默认每10秒记录一次完整状态；采样捕获的决策变化会提前记录，但状态记录最多每秒一条。短于采样/记录间隔的瞬态可能合并，日志不是每次文件操作的审计流水。清理和生命周期事件独立记录，不受此状态限频限制。

## 当前状态看JSON文件

```sh
containercacheguard status --config /opt/ccg/config/dbs.json
# 直接读取需要自行检查下表中的时间、采集错误和可用性。
cat /opt/ccg/state/dbs/status.json
```

以上路径是示例，应替换成实际配置。`status`不触发目录扫描，不修改业务；对过期状态报错，默认过期判断为3倍采样周期。读取到`STOPPED`不表示仍在运行；检查PID时还需核对可执行文件和启动标记，防止PID重用。

| 想确认什么 | 要一起看哪些字段 |
| --- | --- |
| 是否有新鲜采样 | `time`、`metrics.time`、`sample_interval_ms`、`metrics.error`；外部采集拒绝过期或明显未来时间 |
| 数值是否可用 | `metrics.available_metrics`；日志摘要未知显示null，不把零当作未知或反过来 |
| 保护条件是否齐全 | `protection.ready/missing/disabled/unsupported`；齐全不等于允许回收 |
| 当前是否允许新建议 | `decision.allow/state/reason`、`paused`、`recovery` |
| 实际做了什么 | `counters`、日志的`since_previous_log`；建议、驻留复查和重新驻留是不同口径 |
| CCG自身成本及日志占用 | `metrics.self_rss_bytes/self_cpu_percent`、`logging` |

普通文件页近似为max(file-shmem,0)，必须两个字段都可用；匿名、shmem和slab只监测，不通过文件接口清理。没有OOM、缓存下降或进程仍在都不能单独证明业务正常。

## 四容器持久采集

每个实例使用独立的配置、状态文件和日志子目录，详见[四容器接入](FOUR-CONTAINERS.zh.md)。外部采集器每10至15秒读取现有状态文件，持久保存时间戳、可用性、缓存、IO、保护和工作计数，再生成小时图表。按每个实例自己的PID/启动标记划分计数窗口，重启后计数重建基线，不直接跨重启作差。

只需低频读取小文件，不要周期执行全目录`scan`、遍历PGDATA或新启一个CCG来当监控。外部采集器不属于本程序内置服务；本版没有自动安装新的采集、日志平台或Prometheus exporter。已有采集若依赖旧HTTP地址，升级前必须切换到文件源，否则图表会断流。旧HTTP版若要回退，需同时恢复旧配置，不能把新增字段直接交给旧程序。

## Kubernetes和业务健康分开判断

不要把已取消的HTTP端点继续放在Pod探针中，也不要用CCG状态故障触发数据库业务容器重启。需要监控CCG时，使用其独立进程看护和新鲜状态文件；不要把业务Pod的ready/liveness绑定到缓存治理成功与否。

数据库仍需独立采集SQL错误和p95/p99、启动时间、备份/校验结果、Pod重启、cgroup OOM、节点可用内存和Kubelet Evicted/MemoryPressure事件。文件日志和本地状态不能替代这些业务验收。源服务器保持只读；取消HTTP代码不表示已在任何现场容器升级。
