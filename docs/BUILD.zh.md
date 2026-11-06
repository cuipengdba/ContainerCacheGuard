# 构建与发布

产品只使用 Go 标准库，没有 CGO、动态库和外部备份客户端依赖。支持 Linux amd64/arm64；Windows 可以交叉编译，但程序不在 Windows 上运行缓存治理。

## 构建

Go 1.24 或更高，当前构建和 CI 使用 1.27.3。Python 仅用于可选构建辅助脚本和验收，不是产品运行依赖。

```sh
python3 scripts/build.py --version 0.1.0-rc.1 --commit "$(git rev-parse --short HEAD)"
# 或直接使用 Go：
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o containercacheguard .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o containercacheguard-arm64 .
```

输出目录 dist 包含双架构二进制和 SHA256SUMS。使用 `file` 和 `readelf` 验证无 INTERP/NEEDED，不能仅凭 CGO_ENABLED=0 就省略交付检查。

## 测试

```sh
go vet ./...
go test -race -count=1 -v ./...
python3 scripts/integration.py ./dist/containercacheguard-linux-amd64 --output integration-results.json
```

race 测试需要本机可用的 C 工具链；这不改变最终静态程序的依赖。Linux 文件机制测试需支持的真实磁盘文件系统；tmpfs 不作为支持目标。集成测试只在临时目录造文件，不修改 cgroup 限额、不全机清缓存。持续压力、真实数据、Kubernetes/OpenShift 验收另见测试报告。

CI 在原生 x86_64 和 ARM64 runner 执行，不能以交叉编译成功代替原生执行；页大小随 runner 记录，4KiB 的 ARM64 结果不能证明64KiB内核已通过。

## 容器镜像

```sh
docker build --build-arg TARGETARCH=amd64 -t containercacheguard:local .
```

scratch 镜像仅含程序，默认显示帮助。运行时挂载配置、数据和可写状态目录，指定实际 UID。Dockerfile 不包含 shell，因此不要用 shell 命令作为容器启动参数。

## 发布规则

每个版本附源码标签、双架构二进制、SHA256SUMS、中文发行说明以及当前实测范围。候选版不标成生产通用保证。公开仓库只包含通用源代码、文档及去敏汇总，不包含业务数据、连接信息、代理认证、内部 DBS 源码和原始表名。
