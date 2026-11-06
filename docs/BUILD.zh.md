# 怎样从源码构建

建议在专用构建机完成，运行备份的服务器直接使用 Release 二进制。构建会下载上游源码、校验 SHA256，并生成私有 musl、压缩库、OpenSSL 和五个 PG18 客户端，不安装到宿主机的 `/usr`。

## 环境

当前原生构建入口面向 Linux x86_64 或 aarch64。需要 Python 3.9 以上、Go 1.24 以上、GCC、make、binutils/readelf、Perl 完整构建模块、bison、flex、pkg-config、Linux 开发头文件以及下载权限。构建根目录至少留出 3 GiB，首次构建会花较长时间。

例如可在专用 Debian/Ubuntu 构建机安装 `build-essential binutils perl bison flex pkg-config linux-libc-dev python3`；Go 单独按官方发行包安装。这个安装示例不是要求在生产数据库宿主机安装开发工具。

```sh
python3 scripts/build-release.py --build-root /var/tmp/pgcacheguard-build-amd64
```

在 ARM64 Linux 构建机上运行相同命令，选择一个新的构建根目录，即生成 ARM64 产物。这个入口按本机架构构建，不负责模拟另一种架构。

上游下载地址和 SHA256 固定在 `scripts/sources.json`。已有离线源包时：

```sh
python3 scripts/build-release.py \
  --build-root /var/tmp/pgcacheguard-build-offline \
  --source-cache /srv/upstream-source-cache --offline
```

离线目录放原始归档文件，文件名需与清单一致。缺失或 SHA256 不一致会失败。构建根目录必须为空，失败目录保留排查，脚本不会递归删除旧目录。下载代理使用标准 `HTTP_PROXY`/`HTTPS_PROXY` 环境变量，不要把认证信息写进源码。

## 产物与检查

- `dist/pgcacheguard-linux-amd64` 或 `dist/pgcacheguard-linux-arm64`：单文件程序。
- 同名 `.sha256`：校验和。
- `dist/native-build-架构.json`：五个原生客户端的版本、静态检查和 SHA256。

工具链和 Go 程序都会检查没有 ELF `INTERP` 和动态 `NEEDED`。这证明没有动态用户态库依赖，不代表文件系统权限、内核版本或容器安全策略已经通过验收。

```sh
go test ./...
readelf -l dist/pgcacheguard-linux-amd64
readelf -d dist/pgcacheguard-linux-amd64
dist/pgcacheguard-linux-amd64 basebackup --version
dist/pgcacheguard-linux-amd64 basebackup --help
```

Go 编译前必须先生成对应架构的内嵌客户端资源，所以全新源码不能跳过工具链构建直接运行 `go build`。源码可复建不等于不同时间、路径和 Go 版本生成的二进制逐字节相同；当前未承诺可重复构建的字节级一致性。

已完成的 amd64 实验构建和 ARM64 交叉构建有独立证据。这里新增的通用构建入口还需要在干净构建机回归；不能把既有私有目录构建成功当作所有发行版都已通过。

## GitHub 上的双架构原生验证

仓库的 `Native static build and recovery smoke` 工作流分别使用 `ubuntu-24.04` 和 `ubuntu-24.04-arm`。GitHub 的[运行器说明](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)列出这两个架构；ARM 任务在 ARM CPU 上运行，不依靠本项目的 QEMU 配置。工作流记录实际架构和系统页大小，不能把普通 ARM 机器通过写成真实 64 KiB 页内核也通过。

每个任务从固定 SHA256 的上游源码构建静态工具，运行 Go 测试，再独立构建一个 PG18 源数据库，测试全量、连续两代增量、三份合并结果的实际启动与数据核对，以及缺父链、已有目标和损坏拒绝。测试数据是临时生成的小数据；它补充双架构兼容性证据，不能代替迁移真实数据上的性能、缓存或 Kubernetes 驱逐验收。

Actions 中保留二进制、SHA256、客户端构建报告、环境和测试日志 30 天。构建产物不自动标成正式 Release。判断是否通过应查看对应提交的工作流结果，不能仅凭存在工作流文件。源码 checkout 不持久化凭据，工作流只申请仓库读取权限。
