# 171 DBS/HISDBS：CPU不限、内存30/15 GiB调整实测

2026-11-11按用户最新确认调整：DBS内存目标为30 GiB（原消息230GB已明确更正），HISDBS为15 GiB，两者CPU不限制，IO原值保持。本记录补充上午的组件更新记录，不覆盖其中已发生的OOM故障。

## 最终配置与实际回读

服务器标称64GB；Linux/CRG实测`MemTotal=66414546944`字节，即61.85336685180664 GiB。继续按此前GiB口径计算，不能用标称64直接当组件分母。

| 项目 | DBS | HISDBS |
|---|---|---|
| CPU硬配额 | 不限制，`cpu.max=max 100000` | 相同 |
| 内存上限 | 30 GiB，32212254720字节 | 15 GiB，16106127360字节 |
| 等效宿主内存占比 | 48.50180600819428% | 24.25090300409714% |
| IO | 保持原40%标定结果 | 相同 |
| 253:0 rbps / wbps | 1538183604 / 1809852654 | 相同 |
| 253:0 riops / wiops | 13400 / 13114 | 相同 |

实际CPU祖先配额均为max，未修改原cpuset、CPU权重或其他控制项。CPU不限表示没有CPU时间硬配额，仍与宿主机其他进程共享CPU。

当前CRG独立百分比不接受0，也不能用100冒充取消配额。因此通过已安装`v1.0.0-preview.1`内嵌`v1.0.0`的热重载，切换为`resource_mode=manual`；省略`limits.cpu_cores`，由CRG使用同一私有journal恢复仍自有的CPU原值`max 100000`。内存与IO明确按绝对值维护，上表百分比是等效占比，不能宣称manual配置仍设置了memory_percent/io_percent字段。

DBS资源字段如下，HISDBS仅将memory_max改为15GiB；原mode、state_dir、io_profile、interval_seconds、data_paths、log_dir等全部保留：

```json
{
  "resource_mode": "manual",
  "limits": {
    "memory_max": "30GiB",
    "io_read_bps": "1538183604B",
    "io_write_bps": "1809852654B",
    "io_read_iops": 13400,
    "io_write_iops": 13114
  }
}
```

## 方法与安全边界

1. 保存原CRG配置、候选配置、实际控制值和同一身份journal的私有副本；不修改或删除原journal，不创建额外备份容器。
2. 在原容器运行候选doctor，确认只有memory.max和同一设备io.max两项；CPU移除由热重载协调。独立doctor不持有运行Agent的原值日志，DBS增额预览因此仍按当前旧限额截断；实际Agent使用原journal，最终以30GiB内核回读确认成功。
3. HISDBS下调前约17.29GiB。先在其自身cgroup写memory.reclaim，每次请求256MiB，共14次、累计请求3.5GiB；每次检查SQL/API、OOM和进程身份，降至约13.76GiB并留出CRG安全余量后再下调。该值是请求量，不能当精确归因的净回收量；这是显式管理操作，不是CCG工作计数，也不是全局drop_caches。接口行为参考[Linux官方cgroup v2说明](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-interface-files)。
4. 原子替换同一路径CRG配置，调用`crg reload --state-dir /tmp/container-guards/crg --config /opt/guards/config/crg.json`。需要显式使用运行Agent的state_dir；requested本身不代表生效，必须检查accepted、generation、items和实际cgroup文件。最初DBS显式reload遗漏state_dir返回1，自动协调周期已接受新策略；补充正确state_dir后完成全部检查，没有重启Agent。
5. CPU、memory.max和IO回读符合表中值，两个Agent均为managed、配置generation=2。原cpu.weight=1、memory.high=max、memory.swap.max=0、pids.max=405067、io.weight=default 100均未变；CCG配置、IO profile、程序摘要、Deployment spec、容器ID、数据挂载、postmaster启动时间及两组件进程启动标识均未变。

## 实测时间与结果

以下均为服务器北京时间，服务器比客户端慢约100秒，未调整时钟：

| 事件 | DBS | HISDBS |
|---|---|---|
| 原子替换配置 | 16:22:58 | 16:26:12 |
| 每分钟监控首次采到新限额 | 16:23:19 | 16:26:19 |
| 本轮短检查结束 | 16:25:20 | 16:26:49 |
| SQL/API检查组数 | 9 | 25 |
| SQL/API错误 | 0 / 0 | 0 / 0 |
| SQL平均/最长 | 100.90 / 108.34 ms | 100.74 / 110.91 ms |
| API平均/最长 | 12.28 / 15.48 ms | 12.56 / 16.03 ms |
| 新增OOM / kill | 0 / 0 | 0 / 0 |

16:29:28组合复查：DBS约14.46GiB，HISDBS约13.76GiB；两库SQL/API成功，实际配额仍符合要求。此为短期现场验证，不能代替持续业务验收、全量数据校验或备份恢复验收。

## 保留的历史与监控

CCG仍为0.2.0-rc.12、auto/enforce、文件缓存软目标2GiB，没有更新程序或重启CCG。DBS CCG仍为CRITICAL/new_oom_kill保护；本次不通过重启清除该状态。HISDBS为CONTROL/cache_budget_exceeded。PSI未知，可选max_io_psi_avg10=0维持禁用，其他已有配置不变。

DBS当前容器累计oom=731、oom_kill=3：01:29附近1次kill、10:58两次kill；另有旧DBS容器历史1次kill。HISDBS当前容器累计0/0。本次没有清零统计，也不能将上午的非计划数据库重启归为本次计划维护。

原一分钟采样、每小时只读汇报继续，更新监控基准到本次CPU不限和30/15GiB。容器ID分段计算累计量、CCG按PID分段；读取resource-events.json识别本次显式配置及管理回收，不将其作为SQL/API失败豁免。后续不自动调参、重启或压测。
