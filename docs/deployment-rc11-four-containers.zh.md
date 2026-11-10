# 四容器替换为rc.11：部署记录与日志位置

时间：2026-11-10，UTC+8。本页记录94测试机的实际替换；171没有修改。四处运行程序均核验为已发布的0.2.0-rc.11，SHA256为`7ab4ae462235c4419ab967189a1d27bb4c515928b63e93502df841882f44617f`。源码标签及原生回归见[本版交付](RC11-DELIVERY.zh.md)。

## 实际结果

| 实例 | 当前动作与状态 | 启动和持久化方式 |
| --- | --- | --- |
| dbs | observe / OBSERVE | 只替换CCG，由原业务看护脚本重新拉起；数据库Pod未重建 |
| hisdbs | observe / OBSERVE | 同上 |
| dbsbackup | observe / OBSERVE | 备份Pod重建一次，挂载固定rc.11程序和独立日志目录 |
| hisdbsbackup | enforce / SUSPENDED | 同上；因缺IO PSI数据暂停新回收，详见下文 |

四个旧端口9898、9899、9900、9901均不再监听；逐一检查了新进程的实际TCP套接字，不只检查预期端口。四处均生成合法每日JSON日志，含启动、中文说明、摘要及周期工作量。原监控采集器通过`status`命令继续取得四个新鲜rc.11快照，不依赖HTTP。

18:20:11–18:32:08的替换窗口内，已有15秒SQL探针两库各48次、失败0，最大采样间隔约15.12秒；成功探针中的数据库启动时间各自保持一致，PostgreSQL主进程PID/启动标记未变化。DBS/HISDBS的Pod UID未变化、重启数仍0；两个备份Pod的UID因受控重建变化，新Pod Ready、重启数0。没有启动备份、校验或模拟压力，也没有把这次短窗写成完整业务SLO或48小时验收通过。

## 具体怎样挂载与启动

业务容器沿用原只读`/opt/ccg`目录挂载，宿主机原子替换`bin/containercacheguard`。只向核对过的CCG子进程发送TERM，原脚本独立重启CCG；不向Pod的PID1或数据库发送信号。旧进程退出与新进程启动之间有短暂CCG监控空窗，SQL探针独立运行。

备份镜像标签没有改变。两个备份Deployment增加固定版本文件的只读hostPath挂载，目标仍是`/opt/ccg/containercacheguard`，因此原看护脚本和采集命令无需换路径。源文件名为`containercacheguard-v0.2.0-rc.11`，后续升级更换版本文件及挂载来源，再受控重建备份Pod；不要覆盖一个已经被单文件bind mount固定的旧inode并期待它自动更新。

两个备份Deployment采用`Recreate`：先停止旧实例，再启动新实例，避免同一备份范围出现两套enforce同时运行。实际执行前确认没有`pg_basebackup`或`pg_verifybackup`进程，先做Kubernetes服务端预检，再逐个更新。现有备份数据和输出挂载未改变。

配置文件保留原模式、路径、目标及资源预算，移除Web字段，并新增日志字段。备份配置是单文件bind mount，因此更新时保留原inode；改宿主文件名不会让既有bind mount自动切换。配置更新后才重启CCG或重建相应备份Pod。

这里仍是原来的两个业务Pod和两个**临时独立备份Pod**，没有改成同Pod sidecar，也未借本次更新完成YsP正式服务注册或备份调度移交。

## 四处日志怎么找

容器内统一为`/var/log/containercacheguard/`；各实例对应不同宿主目录。业务容器使用原有可写`/var/log`磁盘挂载里的专用子目录；备份容器增加独立日志目录挂载，日志不混入PGDATA或某次物理备份输出。业务CCG沿用原UID0，备份CCG沿用UID999/GID998；日志目录0700、文件0600，按实际UID准备权限。

```json
{
  "log_directory": "/var/log/containercacheguard",
  "log_keep_days": 30,
  "log_max_total_bytes": 1000000000,
  "log_timezone": "Asia/Shanghai"
}
```

```sh
# 在对应容器里执行，配置路径按下表选择。
tail -n 20 /var/log/containercacheguard/containercacheguard-2026-11-10.log
containercacheguard status --config /path/to/instance.json
```

| 实例 | 实际程序路径 | 实际配置路径 | 状态文件 |
| --- | --- | --- | --- |
| dbs | `/opt/ccg/bin/containercacheguard` | `/opt/ccg/config/dbs.json` | `/tmp/ccg-dbs/status.json` |
| hisdbs | `/opt/ccg/bin/containercacheguard` | `/opt/ccg/config/hisdbs.json` | `/tmp/ccg-hisdbs/status.json` |
| dbsbackup | `/opt/ccg/containercacheguard` | `/etc/ccg/output.json` | `/tmp/ccg-dbsbackup/status.json` |
| hisdbsbackup | `/opt/ccg/containercacheguard` | `/etc/ccg/output.json` | `/tmp/ccg-hisdbsbackup/status.json` |

每天一文件，每实例最多30个日历日和1GB，容量先清最旧历史，当天独自触顶会截断当天旧内容。四实例并不是共用1GB；运行时另存的控制台日志也不在该上限内。详细含义见[日志手册](LOGGING.zh.md)。持久化由挂载设计和运行配置核对，本轮未为验证持久性再主动重启一遍业务Pod。

## 必须保留的限制：94尚未启用PSI

只读实测宿主机内核为5.14.0-611.54.3.el9_7.x86_64，内核配置`CONFIG_PSI=y`、`CONFIG_PSI_DEFAULT_DISABLED=y`，当前启动参数没有启用PSI。宿主`/proc/pressure`和四个实际cgroup中的`io.pressure`、`memory.pressure`均不存在，说明不是备份容器里读取文件的权限问题。

四个实例的`protection.missing_metrics`均包含`io_psi_avg10`。三个observe实例继续观察；hisdbsbackup虽保留enforce配置，实际为`SUSPENDED / protection_metrics_unavailable:io_psi_avg10`，不发出新的回收建议。**部署完成不等于主动回收已就绪，更不能认定缓存问题或驱逐风险已解决。**

内核文档说明PSI通过`/proc/pressure`和cgroup压力文件提供，`psi=`启动参数控制启用。后续可在维护窗口评审启用`psi=1`并受控重启后重新核验；本次没有修改启动参数、重启服务器或把PSI阈值设0来绕过保护。[PSI接口](https://docs.kernel.org/accounting/psi.html)、[内核启动参数](https://docs.kernel.org/admin-guide/kernel-parameters.html)是该机制的原始说明。

## 回退与证据

94受限操作目录已保存四份旧配置、四份旧二进制、两个原Deployment、应用补丁和部署结果；本地交付目录保留前后盘点、日志/版本核验、SQL窗口和内核PSI证据。没有把业务数据、凭据或完整原始Deployment上传到公开仓库。

若需要回退，先确认没有活动备份。两个备份按保存的旧Pod模板受控重建；两业务共用宿主二进制，因此应成对恢复旧程序和旧版可解析的配置，只重启CCG子进程。回退配置仍去掉Web字段以保持页面关闭，不把rc.11新增日志字段交给旧版。回退脚本与资料已准备，本轮没有主动做会再次切换版本的回退演练。

最终验收仍包括保护输入齐全、缓存收益与重新读盘代价、业务p95/p99、完整备份/校验/恢复、YsP正式接入及持续等压48小时。这些未完成项没有因本次替换而标为通过。
