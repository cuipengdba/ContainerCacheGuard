# DBS/HISDBS 与独立备份容器：部署和 Web 使用

本页给运维人员一条可检查的接入路线。ContainerCacheGuard（下称 CCG）是常驻进程，不替代 PostgreSQL、DBS 或 `pg_basebackup`。示例为 PostgreSQL 18；把路径、端口、UID、资源限额和存储卷换成自己集群的实际值后使用。

## 为什么是四个容器

| 容器 | 业务进程 | 读写的数据 | CCG 管的文件范围 | CCG 读取的内存账本 |
| --- | --- | --- | --- | --- |
| DBS 业务 | PostgreSQL、现有 DBS 程序 | DBS 的 PGDATA | DBS 的 `PGDATA/base` | DBS 容器 cgroup |
| DBS 备份 | PG18 `pg_basebackup`、`pg_verifybackup` 等客户端 | 只读连接 DBS 的 Unix Socket，写独立备份卷 | DBS 备份输出目录 | DBS 备份容器 cgroup |
| HISDBS 业务 | PostgreSQL、现有 HISDBS 程序 | HISDBS 的 PGDATA | HISDBS 的 `PGDATA/base` | HISDBS 容器 cgroup |
| HISDBS 备份 | 同上 | 只读连接 HISDBS 的 Unix Socket，写另一备份卷 | HISDBS 备份输出目录 | HISDBS 备份容器 cgroup |

备份容器不启动数据库、不运行 DBS 业务程序；CCG 作为备份目录的资源控制辅助进程。两个备份输出卷应分别有容量预算和保留规则。它们即使是不同分区，也可能共用一块物理盘，不能把分卷当成磁盘 IO 隔离。

**源端读盘发生在 PostgreSQL 服务端，输出写盘发生在 `pg_basebackup` 客户端。** 两端各运行一个 CCG 后，自动探测得到各自容器的 cgroup，不必从备份容器跨 cgroup 猜测数据库内存。业务容器 CCG 对本容器 PGDATA 发 `POSIX_FADV_DONTNEED` 建议；备份容器 CCG 处理其输出卷。若把源端 CCG 改放到备份容器，必须只读挂载同一实际 PGDATA 文件并显式监控业务 cgroup；核对设备号/inode/UID，不能用复制副本冒充同一页缓存。

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
  "cgroup_path": "",
  "state_file": "/run/ccg-output/status.json",
  "cache_target_bytes": 1073741824,
  "web_listen": "127.0.0.1:9898",
  "web_history_samples": 3600
}
```

配置只写出需要覆盖的字段，其余使用 `init-config` 所示默认值。2 GiB/1 GiB 是实验软目标，**不是推荐生产阈值或硬上限**。根据容器 limit、节点余量、活跃页、业务 p99 和备份时长定标。`PGDATA/base` 中也可能有热点；建议从只观察和低复用备份产物开始，排除已识别热点后才对数据库端启用 enforce。`global`、WAL 和事务目录默认排除，不要为追求更大回收数字解除这些保护。

## 在容器里启动

将对应架构的静态二进制装入镜像或只读挂载。运行 UID 应能读目标文件并写自己的状态目录。每个容器先执行：

```sh
containercacheguard doctor --config /etc/ccg/source.json
containercacheguard run --config /etc/ccg/source.json

# 备份容器将 source.json 换为 output.json
```

`run` 在前台常驻。用原有 supervisor 管理它，保持 PostgreSQL/DBS 的退出和信号处理。若现有镜像由 `/root/start.sh` 启动，应在独立实验环境检查该脚本何时创建目录、切换 UID 和启动 DBS，再加入 CCG 子进程；不能简单把第二个永久命令接在 `start.sh` 末尾。就绪探针应同时确认数据库/DBS 接口和 `containercacheguard status` 的采样新鲜度，否则 Pod `Running` 可能掩盖控制进程已退出。

旧 DBS 代码有多个硬编码 `vmtouch` 路径，涉及备份、同步和源端 `releaseCache`。在 observe 阶段可并存以看现状，但无法把下降归因于 CCG；**enforce 阶段同一范围只能有一个回收执行者**。若现有 DBS 发行包没有关闭旧清理的配置开关，需要做明确的 DBS 接入改造，并验证备份/恢复退出码和异常路径。把 `vmtouch` 换成静默返回成功的空命令会掩盖问题，不应采用。`nocache` 若仍包装某个实际命令，也要核对其进程生命周期与缓存范围。

## 备份容器怎样连接 PostgreSQL

将数据库的 Unix Socket **目录**共享给对应备份容器；备份输出使用独立 PVC。备份容器无需写 PGDATA，也无需共享数据库网络命名空间。Socket 目录权限及 PostgreSQL `pg_hba.conf` 的 `local replication` 规则应经过实际验证。示例：

```sh
pg_basebackup -F p -P -v --checkpoint=fast -X s \
  -h /run/pg-socket -p 5432 -U replica \
  -D /backup/20261107-full --max-rate=60M
pg_verifybackup /backup/20261107-full
```

这与 DBS 当前 PostgreSQL 全量备份调用的关键参数一致；`--max-rate` 负责备份数据传输的限速，**不是全盘 IO 硬上限**，WAL 流、校验和业务写入仍会增加 IO。输出目录创建后才会被路径发现；初始峰值要计入余量。一次完整备份结束后，记录 `backup_manifest` 校验和、`pg_verifybackup` 结果与恢复演练，再按容量规则淘汰较早的实验备份。真实生产保留策略必须满足 RPO/RTO，不可照搬实验清理。

原 DBS 内置的 03:00 备份调度若仍在业务容器调用 `pg_basebackup`，就没有真正分离。要将任务改由备份容器的调度器/Job 执行，并让 DBS 查询任务状态或接受结果回执；DBS 当前二进制是否支持该接入要按发行版本验证。实验中直接从备份容器运行同参数客户端，只证明这条数据通路，**不等于 DBS 自动任务链已经迁移验收**。

## 打开实时页面

每个容器的 `127.0.0.1:9898` 只在自己的网络命名空间可见。具有 Kubernetes 权限的运维人员在自己的终端执行，例如：

```sh
kubectl -n <namespace> port-forward pod/<hisdbs-pod> 9898:9898
# 浏览器打开 http://127.0.0.1:9898/
```

直接打开 `http://<节点IP>:9898/` **不会自动连到 Pod 内页面**：需要显式转发或网关。隔离内网实验若必须从其他电脑直接访问，可临时将 `kubectl port-forward --address <节点内网IP>` 分别绑定到四个不同宿主端口，并确认节点防火墙只放行受信任客户端。例如 `9898:9898` 转 HISDBS 业务Pod、`9899:9898` 转 DBS业务Pod、`9900:9898` 转 HISDBS备份Pod、`9901:9898` 转 DBS备份Pod。这个转发本身没有用户鉴权，仅适合受控实验网络；生产环境应使用本地回环转发或有TLS与鉴权的代理。Pod重建后须重新核对转发及页面状态。

逐个查看四个 Pod 时可以选不同本地端口：`9899:9898`、`9900:9898` 等。页面每 2 秒获取状态，曲线每 15 秒刷新，显示采样时间和过期提示。`file` 包含 `shmem`，页面单列 `max(0,file-shmem)` 估算普通文件缓存；`active_file`/`inactive_file` 是 cgroup 的 LRU 分类。匿名内存、共享内存、slab 只展示，不能被文件范围释放建议清除。页面显示的 `advice_bytes` 是建议范围，净驻留下降另列，两者不能混为一谈。

若必须监听非回环地址，配置 `web_token_file`，权限 0600，用户名 `ccg`，并通过 TLS 反向代理或安全隧道访问。HTTP Basic 自身不加密，不应将开放的 HTTP 端口直接暴露在生产网络。页面与 API 均只读：`/api/v1/status` 返回最新状态，`/api/v1/history` 返回最近有界采样；重启后历史清零。48 小时统计要由持久采集保存，不能依赖浏览器进程内曲线。

## 压力测试与验收

1. 记录四个 Pod 的镜像摘要、DBS 程序校验和、PG 版本、配置差异、requests/limits、UID、PVC 与底层磁盘设备。版本不同就明确写出来。
2. 在迁移数据副本上先跑 12 小时 observe，对齐写入 TPS、WAL、备份时间、同步突发和双份备份重叠。旧清理措施是否运行必须单独标记。
3. 在不改变数据量/限速/资源配置的前提下，对**同一明确范围**启用 enforce；至少记录 PGDATA 与备份端的 `file`、`active_file`、`inactive_file`、`anon`、`shmem`、dirty/writeback、IO、节点可用内存、CCG CPU/RSS 和业务 p95/p99。
4. 每 12 小时生成统计图和说明；48 小时全程保留原始采样、Kubelet `Evicted` 事件、OOM 与容器重启记录。完整备份必须通过 `pg_verifybackup`，并做独立恢复演练。
5. 若 `active_file` 未复现、CCG 没发出建议、业务延迟恶化、读盘反弹或旧 vmtouch 同时运行，不宣称问题已解决。有限测试只能证明明确压力范围内的结果，不能保证所有未来负载下“百分之百不驱逐”。

94 的隔离实验为了 PostgreSQL 18 目标使用升级后的数据库副本。原环境仍是 PostgreSQL 14，且有不同的节点内存、Patroni/HBP/etcd 配置；生产模式的 DBS 二进制在缺少这些外围状态时会停止数据库。实验保留相同的 DBS/HISDBS 二进制但关闭其集群联动，使用外部定时脚本驱动备份。因此该实验能验证备份与缓存通路和真实 Kubelet 压力，不能冒称完整生产功能等价。
