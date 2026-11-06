# pgcacheguard

pgcacheguard 为 PostgreSQL 18 原生物理备份增加缓存处理、资源监控和完整性检查。现有程序可以把它作为命令行工具调用，继续使用 `pg_basebackup` 的全量及增量格式，并通过 `pg_combinebackup` 合并恢复。

**当前是开发验证版本。** AMD64 小规模全量、两代增量及实际恢复已通过；ARM64 在仿真环境完成对应流程。完整迁移数据的 PG18 验证、ARM 原生硬件与集群驱逐场景尚未完成。正式发布前，以测试报告和发布说明列出的覆盖范围为准。

## 它要解决什么问题

数据库的备份读取发生在 PostgreSQL 服务端。即使备份客户端放在另一个容器，源数据文件的页缓存仍可能计入数据库所在容器。客户端写备份文件，又会产生另一份缓存和脏页。只给 `pg_basebackup` 设置带宽，可以降低增长速度，却不能单独限制缓存最终积累多少。

pgcacheguard 同时关注这两端：备份文件按窗口写回后发出释放建议；普通目录备份还会根据复制进度，对共享 PGDATA 中相应的源文件页发出建议，保留任务开始时已经驻留的热页。控制器持续读取节点和容器内存指标，达到警戒线时降速，达到停止线时终止自己的任务。

Linux 的 `POSIX_FADV_DONTNEED` 是建议接口。它不能保证立即清除脏页、映射页、共享内存或业务正在使用的页。因此本工具的目标是将备份引起的缓存增量控制在经过验证的预算内，而不是保证任何负载下缓存为零。

## 先阅读这些说明

- [设计原理与脚本对比](docs/DESIGN.zh.md)：数据通路、跨容器条件、控制器和已有工具的关系。
- [配置参数](docs/CONFIGURATION.zh.md)：每个配置项的单位、默认值、选择方法和限制。
- [PG18 备份参数](docs/PG18-OPTIONS.zh.md)：全部原生参数索引及缓存控制的适用范围。
- [从源码构建](docs/BUILD.zh.md)：上游源码、静态工具链、离线构建与实际验证范围。

## 交付形式

目标交付为 Linux amd64 和 arm64 的自包含单文件。程序内置相应架构的静态 `pg_basebackup`、`pg_verifybackup`、`pg_combinebackup`、`pg_waldump` 和 `psql`，运行备份无需另行安装 PG18 客户端。

单文件运行时会在受控目录展开客户端，用完删除；目录需要允许执行，且应有约 40 MiB 空间。静态编译仍依赖 Linux 内核、文件系统、挂载权限和配置；它不意味着可以在 Windows 运行 Linux 备份，也不意味着包含数据库服务端及 PostGIS、Timescale、Citus 等服务端扩展。

当前静态客户端构建启用 OpenSSL、gzip、lz4 和 zstd，未启用 GSSAPI、LDAP 和 OAuth/libcurl。复制连接的身份核对目前要求共享的本机 Unix Socket，并核对实际服务端的系统标识与所挂 PGDATA；任意远端数据库不在当前源端缓存控制范围内。

## 准备目录与身份

建议数据库和备份分别放在两个容器，使用相同的数据库 UID。备份容器只读挂载实际 PGDATA，读写挂载备份目录、运行目录和共享锁目录，并共享 PostgreSQL Unix Socket。

PGDATA 和备份目录必须是不同目录，运行目录也应独立。不能通过另一个数据副本的相同路径名来控制数据库缓存；必须访问相同文件系统中的同一 inode。不要把程序以特权容器运行作为默认方案。

从 [config.example.json](config.example.json) 复制配置，根据实际挂载修改路径、限速和内存预算。示例中的数值只是说明，不是已确定的生产安全值。示例要求提供新鲜的 kubelet Summary 数据；没有该数据时会拒绝工作，不能直接删除这一要求后声称具有相同保护能力。

## 全量备份

```sh
./pgcacheguard -config /etc/pgcacheguard.json basebackup \
  -D /backup/pg18-repository/full-001 -Fp -Xstream \
  --checkpoint=spread --max-rate=20M --manifest-checksums=SHA256 -P
```

普通目录、客户端落盘、默认 WAL 位置，是目前同时处理两端缓存的推荐模式。备份完成后自动执行文件与 WAL 校验。程序的资源上限和原生 `--max-rate` 同时生效，取较小值；实际有效值写入结果记录。

原生 `--checkpoint=fast` 也会保留。它可能在任务开始时制造额外的写入峰值，不能认为备份传输限速同时限制了检查点。

## 增量备份

服务端需要启用 PG18 的 WAL 汇总，并保留足以覆盖父备份的汇总文件。父清单必须来自同一数据库实例的正确备份链。

```sh
./pgcacheguard -config /etc/pgcacheguard.json basebackup \
  -D /backup/pg18-repository/incr-002 -Fp -Xstream \
  --incremental=/backup/pg18-repository/full-001/backup_manifest \
  --max-rate=20M --manifest-checksums=SHA256
```

下一次可使用 `incr-002/backup_manifest`，但恢复时需要完整的有序父链。增量目录不能直接作为完整 PGDATA 启动。定期合成已经校验的完整恢复副本，可以缩短事故发生后的合并等待。

## 校验与合并恢复

```sh
./pgcacheguard -config /etc/pgcacheguard.json verifybackup \
  /backup/pg18-repository/full-001

./pgcacheguard -config /etc/pgcacheguard.json combinebackup \
  -o /restore/staging-002 \
  /backup/pg18-repository/full-001 \
  /backup/pg18-repository/incr-002
```

合并前校验各个输入，合并后再次校验结果。目标必须是新的独立目录或空目录，不能覆盖任何输入备份或已有数据。默认拒绝写当前配置的 PGDATA；仓库内独立的新目标可用于 DBS 的 synthetic 输出。当前合并入口支持 `-o/--output`、`--clone`、`-n/--dry-run` 和有序输入目录；不要把这个范围误写成已支持合并工具的全部参数。

已有 DBS 调用中的 `--skip-checksums` 和 `--no-parse-wal` 会被接受并记录，但包装器会加强为完整校验。缺少所需 WAL 的备份可能因此被拒绝；这是接入前必须核对的行为差异。`combinebackup --dry-run` 保留上游结构检查含义，不为试运行额外扫描全库数据，也不将其标为完整验证；仓库内新建的 synthetic 目标可用于现有 DBS 的链检查。

恢复流程应先在新目录校验并启动演练，确认业务数据和扩展可用后，再由运维流程切换。不要先删除唯一的 PGDATA，再开始判断备份能否恢复。默认不写配置的 PGDATA；现有 DBS 必须输出到该路径时，可显式启用 `allow_stopped_pgdata_restore`，在确认目标为空且已停库的条件下，先合并到相邻 staging、完整校验，再禁止覆盖地原子改名发布。该适配已通过小规模实际启动恢复回归；完整 DBS 服务流程仍需验收。工具不会自动停止业务数据库或切换服务地址。

## 现有程序怎样调用

推荐直接调用 `pgcacheguard -config ... basebackup`，后面放原来的 PG18 参数。也可以设置环境变量后使用接近原生的形式：

```sh
export PGCACHEGUARD_CONFIG=/etc/pgcacheguard.json
./pgcacheguard -D /backup/pg18-repository/full-003 -Fp -Xstream -r20M
```

另一个接入方法是在专用 PATH 目录把程序链接为 `pg_basebackup`、`pg_verifybackup` 和 `pg_combinebackup`。如果现有程序写死客户端绝对路径，仅修改 PATH 不会生效，必须通过挂载或安装布局使那个实际路径指向兼容入口。不要覆盖数据库服务端程序。

退出码 0 表示该命令成功，失败为非零。还要检查运行目录中的 `result.json`：`success=true` 与 `verified=true` 表示不同事情，tar、无清单及不含所需 WAL 等原生模式可能成功产出文件但没有完成完整可恢复性校验。

包装器只控制它自己运行期间的任务。DBS 随后在 Python 中执行的整文件压缩、解压、同步迁移和删除操作，不会因为前一个备份子进程退出就继续受到包装器保护。完整接入演练必须覆盖这些后续步骤。

## 日志和结果

原生数据输出保留在标准输出，结构化事件输出到标准错误，避免污染 `-D -` 的 tar 流。安全采样按配置频率进行，常规采样日志最多约每 10 秒一条，压力状态变化时及时输出。每次任务在 `runtime_dir` 创建独立目录，保存原子写入的 `result.json` 和控制状态。

记录中的 `*_advice_requested_bytes` 是系统调用请求处理的字节量，不是测得的缓存释放量。实际效果应结合驻留页测量、容器 `memory.stat`、节点可用内存、业务延迟和恢复结果评价。

## 当前边界

- 推荐模式支持默认表空间。外部表空间还需完整、可核对的共享挂载与进度映射；当前 `preserve-hot` 会明确拒绝该场景。
- `destination-only` 可用于只处理客户端输出缓存的其他原生格式；`off` 用于限速基线对照。二者都不提供源 PGDATA 缓存保护。
- 全部 `pg_basebackup` 参数按 PG18.6 上游选项表识别并传递，参数组合仍由上游校验；缓存策略、目录保护和本地实例身份检查会限制具体可执行模式。完整功能矩阵尚在回归。
- `memory.high` 不是文件缓存的独立上限；`io.max` 也不是程序逻辑吞吐限制的同义词。把它们加在整个数据库容器上会影响业务，必须单独验证。
- 容器运行测试不能替代 Kubernetes/OpenShift 的真实驱逐、权限、SELinux/SCC 和挂载验收。

## 上游资料

本项目保留 PostgreSQL 的参数解析、备份协议和文件格式，IO 适配及控制器由本项目实现。基础缓存接口来自 Linux，已有的 nocache 和 vmtouch 也使用相关机制。详细出处及区别见设计说明。

[PostgreSQL 18 pg_basebackup](https://www.postgresql.org/docs/18/app-pgbasebackup.html) · [增量合并](https://www.postgresql.org/docs/18/app-pgcombinebackup.html) · [Kubernetes 节点压力驱逐](https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/)
