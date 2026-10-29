# 历史执行与恢复责任

本文集中维护已退役自研 runner 的历史恢复责任。新正常路径采用[上游架构](architecture.md)，不消费本协议作为逐 job 或 PR 的门禁。源码删除不结束旧执行，不产生重试、清理、服务启停、凭据变更或迁移许可。

## 原版本与现场原件

原恢复 owner 先绑定实际宿主、原操作、准确源码与成品摘要、安装版本及原记录，再解释历史结果。主线曾有的源码模板不证明当前安装版本；同名目录、程序或对象也不建立身份连续性。保留原请求、原响应、意图、锁、发布与服务回执及脱敏报告。含秘密的配置、凭据、数据库及操作原件继续留在原受保护位置，不进入 Git 或 PR。

退役前的仓库基线为 `23eb693a797fae054571fbb691df259b61cb153e`。下表只用于解释旧格式和定位原合同；恢复仍选择与原操作实际绑定的版本，不默认采用该基线，也不执行历史说明中的命令。

| 历史材料                | 准确版本的追溯入口                                                                                                                                                                                                                                                                                                                                                                                                                                                            | 保留用途                                                       |
| ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------- |
| Go 运行程序、模板与依赖 | [源码树](https://github.com/headironc/designs/tree/23eb693a797fae054571fbb691df259b61cb153e/runner)                                                                                                                                                                                                                                                                                                                                                                           | 解释原字段、字节、版本和程序行为；不是当前部署清单             |
| 原执行与资源            | [采集](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/execution.md)、[闭合诊断](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/closure.md)                                                                                                                                                                                                                                              | 原执行身份、producer 和观察缺口                                |
| 原写入与服务            | [provider](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/delete.md)、[firewall](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/firewall.md)、[watchdog 暂停](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/pause.md)、[gateway 停止](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/gateway.md) | 原事务、systemd job/event、进程与排空结果                      |
| 原凭据与连接            | [会话](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/session.md)、[观察凭据](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/github-observer.md)、[连接](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/connection.md)                                                                                                                       | Login、mint、registration、撤销与到期的独立事实                |
| 历史安装观察            | [inspect-prepare](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/prepare-evidence.md)、[inspect-installed](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/installed-evidence.md)                                                                                                                                                                                                        | 五份 prepare 原件与 25 个安装文件的不同观察范围                |
| 旧维护窗口设计与来源    | [历史恢复协议](https://github.com/headironc/designs/blob/23eb693a797fae054571fbb691df259b61cb153e/runner/records-recovery.md)                                                                                                                                                                                                                                                                                                                                                 | 原 Python 来源、旧许可与尚未实现的 Go 窗口设计；不继续该路线图 |

历史 Python 来源属于原 owner 保全的 `halo-20261026` 包，包含 lifecycle、records-only、session、first-connection 及 credential-rebind 等不同版本。原来源清单和摘要决定解释范围。不要 import 或执行旧 Python，不将不同版本的记录或许可混用，不复制归档源码或新增兼容解析器。

## 历史执行闭合与占用

旧执行绑定实际 commit/tree、目标基线、原检查与矩阵、Kiln digest、VM 镜像、程序版本和资源规格。从可信平台事实建立以下关联；guest 自述和当前空列表不能补造原身份：

`run / attempt / job / runner → GARM instance → VM UUID → host boot_id / QEMU PID+starttime / cgroup / writable volumes`

GARM controller、repository、ScaleSet 内部 ID、GitHub ScaleSet ID、instance/agent 和 VM UUID 分别保留其原含义。名称相同、数量相同或重启后的 PID 消失不证明原对象连续。未预先采到的身份和来源保持缺口；新尝试另有身份及与原尝试的关系。

历史完整闭合同时要求以下事实：

- 原 job 已终结，runner 已注销且不能接单。
- GARM 原实例已结束。
- VM、可写卷、快照及执行临时凭据已清理。
- 原 QEMU 已退出，cgroup 无残留。
- 创建、停止、删除等原外部操作全部终结，没有可能迟到创建资源的请求。

未全部闭合时保持原占用。业务结果和资源结果分别判断。成功 job 不补造 closed，失败 job 不因资源闭合改成成功。

原 guard 的 active/intent/latch、pending/new 事务及永久锁保留原字节和原恢复责任。未知、残留或未结操作继续占用原容量，不能手写空状态、删除标记或以 max=1、空数据库、空 LXD 列表代替完整闭合。当前 inactive、job 消失、boot 变化或等待者退出也不证明原 systemd job、进程及资源全部结束。

## 未知结果与恢复边界

旧 Login、mint、registration、repository/ScaleSet 创建、rebind、安装、服务请求及资源写入的 unknown 沿原身份、原意图和原回执恢复。先确认原执行结束，再核原平台对象和结果。不换 operation、目录、实例名或程序语言重发，不以输出失败推定远端未写入，不重置数据库或补造完成封印。新正常 provider 的幂等删除合同只适用于新路径，不能覆盖这些旧操作。

固定历史恢复集合包含 controller-start-verification、first-connection acquisition 和 credential-rebind amendment。保留它们各自的来源、锁、原 Login 和其他未结判断；新会话、密钥轮换或新 controller 不关闭旧 unknown。令牌到期、服务端拒绝和撤销是不同事实。原到期后拒绝报告保留其准确范围，不能改写成即时撤销，也不删除丢失签发回执的 unknown。

旧维护或 records-only 许可只解释原格式、原字节、boot、job/invocation、程序摘要和目标。许可不迁用于新版本、新域或新正常执行，不延长过期许可，不重新编码旧 JSON 来冒称同一摘要。部分签发、发布、配置交换或退役失败继续保留原件。Go records-only 窗口未交付，源码退役后不再作为实施计划；存在旧设计不授权签发新许可。

恢复如需读取 GARM 数据库，先确认准确 schema、实际服务身份及 DB/WAL/SHM。保持只读事务和已提交 WAL 可见性，所有权、链接或来源异常交回原 owner；不通过 chown、复制裸 DB、忽略 WAL 或直接写库制造完成。HTTP 接纳、API 或 DB 单侧缺行不证明原后台处理和资源闭合。恢复不能扩大为任意 SQL、凭据导出或新维护动作。

## 安装观察及保全成品

保留原 `inspect-prepare`、`inspect-installed` 报告和准确 observer 成品，连同请求、原锁、绑定及摘要。保留成品不要求主线继续构建相同 digest；新构建也不能冒充原 observer。若缺原报告或准确成品，向原恢复 owner 取得，不默认再次观察受保护现场。

`inspect-prepare` 的原范围是 `session-20261027-first-connection` / `credential-rebind-175824179` 的五份 prepare 原件和 staging。内部一致性不证明 producer 执行、外部 preflight、服务排空或恢复完成。没有 repair intent 也不证明没有 staging。

`inspect-installed` 的原范围是固定历史 host-plan 的 25 个文件及原锁、计划的复核。文件匹配不证明当前服务、boot、API、数据库、资源、额外 watchdog 或完整 Host.facts。两个报告均不签启动、重试、清理或迁移许可；退出码 0 只表示报告输出成功。读取拒绝、缺锁、来源漂移和观察期间变化保持 unknown，不能解释为不存在或绕过文件保护。

源码退役不取得现场读取许可，也不判定既有报告已运行或历史 unknown 已结束。迁移由原恢复 owner 按准确授权核明旧写者、队列、凭据和原请求不会影响新域；未解决的历史责任保持可追溯。
