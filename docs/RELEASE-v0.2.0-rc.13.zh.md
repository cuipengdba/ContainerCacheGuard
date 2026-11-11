# ContainerCacheGuard 0.2.0-rc.13

本版补齐缺PSI环境的显式实验策略和可信报告基础。仍为候选版本，不能宣称解决所有Kubernetes驱逐。

- 默认strict保持，新增受限`limited-output`只允许cgroup v2和声明的低复用备份产物；替代保护缺失/过期/来源不符或风险超限暂停新建议，恢复需连续健康。
- 接入来源绑定的kubelet Summary实际memory.available、configz有效内存驱逐阈值和MemoryPressure；附独立只读采集脚本，核心静态程序无Python/Kubernetes客户端依赖，不创建RBAC。
- 可选instance_id、每次启动随机run_id及可见cgroup设备/inode贯穿状态、日志、预检和报告；计数不跨运行/身份变化衔接。
- 多实例来源清单和离线SVG图片，继续支持Markdown/JSON，无Web、不监听端口；缺样、observe和暂停不伪造健康。
- 两备份角色保守示例、四容器接入、信号格式和回退说明；保留每日一文件、30天/1GB清理。

[详细使用说明](RC13-USAGE.zh.md)和[交付证据/未验收项](RC13-DELIVERY.zh.md)。原生双架构race/CLI及静态附件以本次发布附TEST-RESULTS.json为准。

94本轮只读确认Kubelet1.26.12、cgroup v2、约30GiB且PSI仍不可用；实际hard memory.available=1Gi。没有修改171，没有在94替换程序/启用PSI/重启数据库/恢复旧压力。完整匹配的真实PG14输出端对照、两库完整备份校验恢复和48小时仍未通过。能力发布与业务验收分开，实验策略不覆盖源PGDATA、匿名内存尖峰及磁盘/PID压力，不自动停止外部备份。
