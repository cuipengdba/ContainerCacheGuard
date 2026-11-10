# 两个候选版本交付与测试记录

2026-11-10。以下是代码功能回归与公开发布结果，未将真实业务长期验收标成通过。

| 版本 | 主要变化 | 两种架构分别通过 | 发布 |
| --- | --- | --- | --- |
| 0.2.0-rc.8 | 风险解除后的连续健康采样恢复；业务信号严格校验；doctor错误退出 | 40顶层race单测、12项CLI、7项网页，0跳过 | [程序和测试JSON](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.8)、[CI](https://github.com/cuipengdba/ContainerCacheGuard/actions/runs/42735032258) |
| 0.2.0-rc.9 | 只读Prometheus、healthz和readyz；字段可用性与过期处理；四目标接入说明 | 44顶层race单测、15项CLI、12项网页，0跳过 | [程序和测试JSON](https://github.com/cuipengdba/ContainerCacheGuard/releases/tag/v0.2.0-rc.9)、[CI](https://github.com/cuipengdba/ContainerCacheGuard/actions/runs/42738942714) |

两架构程序均取自对应版本原生Linux CI，核验ELF的架构、无INTERP/DYNAMIC段、构建提交及干净源码标记。每版6个附件的服务端大小、SHA256和源码标签已独立核对。附件含amd64/arm64程序、SHA256SUMS、TEST-RESULTS.json、中文版本说明和LICENSE。

## 发布提交与回退

- `0.2.0-rc.8`：`cd2e350b0dad8ae8328459cfb64308a956de1835`。
- `0.2.0-rc.9`：`e5f24595f60c8f0969cbe7824396cbf60357a966`。

保留rc.8程序和原配置即可回退；停止CCG不会代管或停止PostgreSQL。新增HTTP接口复用原Web监听和认证，默认Web仍关闭。不要把CCG的健康/监测就绪绑定为同业务容器的数据库liveness/readiness条件；详见[监控接入](MONITORING.zh.md)。

## 编码微基准

| 原生架构 | 固定快照编码耗时 | 分配量 | 分配次数 | 测量次数 |
| --- | ---: | ---: | ---: | ---: |
| amd64 | 21.690微秒/次 | 17333 B/次 | 325次/操作 | 1000 |
| arm64 | 24.318微秒/次 | 17333 B/次 | 325次/操作 | 1000 |

仅测固定内存快照编码，不含HTTP传输、认证、采样、目录扫描或数据库业务。不能据此宣称整个守护进程的资源成本或业务SLO通过。

## 失败与边界

rc.8首轮CI `42734662425`在VERSION格式检查处因CRLF失败；修正为LF并增加.gitattributes后，`42735032258`完整通过。保留首轮记录，不计作全套成功。

本轮没有修改171、没有替换94运行程序。两架构本次内核页大小均4096B，未测试ARM64 64KiB页。真实PG14数据、业务SLO、等强度持续压力、YsP正式接入和48小时/零驱逐验收仍未完成。新版本不消除容量不足、业务匿名内存尖峰或回收热点导致重新读盘的风险。

首页版本徽章由公开Releases自动生成，包含预发布版；远端图片可能有短暂缓存。
