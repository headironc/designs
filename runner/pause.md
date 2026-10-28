# 原执行绑定的 watchdog 暂停

`runner-admin pause-watchdog` 为一个已封印的 [Go 原执行](execution.md)暂停固定 watchdog timer，并确认原 worker 自然排空。它只发送一次 `StopUnit(nucleiron-provider-watchdog.timer, "fail")`。程序保留原 systemd job 和实际 `JobRemoved` 事件，恢复时观察同一请求。

这个结果只覆盖 timer 与原 worker。controller、gateway、凭据、guard 和外部操作仍由各自 owner 处理。worker 可能在退出前完成自己的禁用或 controller 停止操作；排空不证明那些操作成功，也不证明整个执行闭合。

## 第一次请求

入口要求 root、受控环境和原采集请求。采集的 producer 摘要由 root 提供；共享 loader 严格核验原请求、执行记录和完成封印，并取得准确执行字节的摘要。原采集目录保持三个文件，不在其中追加暂停材料。

```sh
/opt/nucleus-halo/bin/runner-admin pause-watchdog \
  --request /root/runner-operations/trusted-request.json \
  --operations /root/runner-operations \
  --capture-sha256 "$EXPECTED_CAPTURE_SHA256" \
  --github-token-file /root/runner-credentials/github-token \
  --garm-token-file /root/runner-credentials/garm-session
```

凭据沿用已有 root `0600` raw token 文件。GitHub 文件仍是待授权供应的部署输入；程序不读取或导出 `gh` OAuth，不登录、mint 或刷新凭据。生产端点固定为 public GitHub、GARM loopback 和宿主 system bus，不接受代理、调试传输或 URL 环境覆盖。

第一次请求核验原 boot、准确 GitHub repository/run attempt/job/commit/runner，要求该 job 已终结，并保留真实 conclusion。任务 failure、cancelled 或 timed_out 均可进入这个检查；暂停不会把任务结果改成 success。

GARM 检查同时覆盖准确 controller、repository、原 ScaleSet、全局实例及目标实例清单。当前实例只能是原绑定实例或完整清单证明不存在。额外、未知或错绑定实例均拒绝新请求。

准入还读取固定 GARM v0.2.1 的全局 `ListScalesets` 和 `ListPools`。两个 admin 接口均无分页，按完整数组处理。每项核唯一内部 ID、准确实体作用域、provider 和 endpoint；任何 enabled、null、缺必需身份或未知响应均拒绝。`Enabled` 的合法省略按固定 producer 的 `omitempty` 合同解释为 false。

原 ScaleSet 必须准确出现一次，并匹配内部 ID、GitHub ScaleSet ID、repository、名称和 `halo-lxd` provider。同一原 repository 与 ScaleSet 名称的另一个 ID 形成冲突。其他 disabled 配置可属于不同 repository/org/enterprise/provider；它们仍须完整通过 public GitHub endpoint 检查。私有 GitHub 或其他 forge 不在支持面，不能过滤掉再声称全部关闭。其他尚未创建的 disabled ScaleSet 可省略 GitHub ID。

重复观察的摘要只编码本入口消费的非秘密身份事实，使用 JSON 保留准确整数与字符串边界。全局 disabled 事实、目标身份和 unit 身份在发送前再次核验。这些观察不是跨 GitHub、GARM 和 systemd 的事务。专用 controller 与独占操作窗口仍是部署前提。

SQLite 的当前配置路径和快照不参与这次写入准入。它们不能证明运行 controller 正连接该数据库，不能替代完整 live GARM 检查。

## 固定原件与一次发送

状态父目录固定为已有的 `/var/lib/nucleiron-watchdog`，要求 root `0700`。永久 `pause.lock` 独立于 worker 的 `operation.lock`，不会挡住原 worker 完成。入口没有另一个 pause 目录或 operation 名称的参数。已有 slot 丢失永久锁时拒绝，不重建锁后继续。

`/var/lib/nucleiron-watchdog/pause` 按顺序保存四份不可覆盖的原件：

| 原件          | 证明的事实                                                                                                                |
| ------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `intent.json` | 版本、原 operation、准确执行摘要、capture/pause producer、原 boot、bus/manager 身份、固定 units、准入摘要和原 worker 身份 |
| `job.json`    | 唯一返回的实际 job ID/path，绑定准确 intent 字节摘要                                                                      |
| `event.json`  | 同一 manager 发出的准确 job/unit/path/result，绑定原 intent                                                               |
| `drain.json`  | 原 job 已 done 后首次核验的 timer/worker 排空事实                                                                         |

目录使用 `0700`，文件使用 `0600`；完整路径、owner、链接数、普通文件类型、大小与对象身份均受检查。文件以 `O_EXCL` 创建，确认文件与目录同步并读回。半写、未知文件、替换、链接或碰撞均保留原状并拒绝，不覆写或迁移。恢复 loader 要求 producer 发出的全部字段，包括零 PID 和空 invocation；缺失或 null 不补成已观察事实。

订阅先于发送，intent 持久化先于一次 `StopUnit`。返回 job 和原终态事件分别持久化。调用失败、取消、超时、丢失返回或写入失败都不会重发。没有持久化 job 的 intent 保持 unknown，即使当前 timer 已 inactive。缺原事件也不能用 unit 当前状态或 job 消失补成成功。

## 恢复与自然排空

重复调用相同入口恢复原请求。恢复要求相同 pause producer、原 execution、boot 和 bus/manager 身份，不要求重新取得平台凭据；已有 slot 不走首次平台准入。未知结果只继续观察原 job，不能发送第二个 StopUnit、CancelJob 或替代请求。占用 slot 的退役和恢复 timer 属于后续独立操作，本入口没有 reset 或 archive。

原事件为非 done 时保留 `timer_job_failed` 和真实 result。原事件为 done 后，程序等待原 worker 自然结束。worker 可初始闲置或执行自己的 oneshot；入口不停止、kill 或重启 worker。

当前检查核 loaded unit、固定 fragment、待 reload、timer/worker 唯一关联、额外触发与停止传播 hooks、固定 timer 配置、oneshot 与准确 `ExecStart`。`SuccessExitStatus` 的 exit-code 与 signal 集合必须均为空，避免 override 将非零退出改报 success；缺失或错误 wire 类型也拒绝。它还核 unit jobs、worker invocation、启动时间和 main/control PID/starttime，并观察固定 cgroup 的设备/inode 与 `populated`。systemd 的动态计时与命令运行列不作为静态配置摘要。

成功要求 timer inactive/dead、两 unit 无 job、worker inactive 或 failed、原 main/control PID 消失且整个 cgroup 无 live 后代。`populated=0` 不证明所有 zombie 都已回收，本入口也不声称枚举其他 zombie。worker 失败但已排空时仍可给暂停结果，同时保留真实 `worker_result`。这不证明 worker 健康。

进程退出、跨状态快照和 unit 已 dead 但 live 后代仍在时，可继续同一次有界观察。原 property job 在后续清单消失可表示正常结束；清单明确出现另一个正 job ID 则拒绝，timer 与 worker 均不把它当退出暂态。原进程处于 `Z`/`X`/`x` 回收态时，仍核准确 starttime 与 cgroup，等待该原进程消失。内核可在 cgroup 删除后保留原路径加 ` (deleted)`；只对已匹配原 starttime 的回收态接纳这个准确路径，活进程或其他路径仍拒绝。替换的回收态进程或畸形 stat 继续拒绝，不能仅凭状态字母推定为原 worker。采集入口仍要求存活进程，沿用原 cgroup 路径规则。PID/cgroup 复用、invocation 替换、迟到 start 或 timer 重新激活拒绝当前成功。末尾重读 unit、boot 和 manager；重入也重新核当前排空，不信任陈旧 drain 标志。root 手动启动属于独占窗口之外的活动，程序只检测其可见影响，不控制其他操作方。

三分钟是本次等待上限。到期或取消结束等待，不结束原外部请求。发送后的任意失败都可能已留下原 job、事件或完整 drain 文件；调用者应保留原材料并回读，不能把非零退出当成没有发生作用。

## 输出与只读诊断

成功输出固定 JSON，包含 `state: "watchdog_paused_worker_drained"`、`timer_job`、`job_result` 和 `worker_result`。失败输出固定 stage/code，并保留已经验证的窄观察字段。错误不包含原响应、配置或凭据。返回码 0 只证明本次暂停与排空检查完成，不签发清理或容量释放许可。

[闭合诊断](closure.md)可通过可选 `--pause-sha256` 接收 root 提供的原暂停 producer 摘要。它共享严格暂停 loader，核原请求和事件后再观察当前 unit/进程/cgroup。它不创建锁、slot、事件或其他原件；原文件与目录的同步只确认原结果耐久性，沿用采集 loader 的恢复合同。

没有可信 pause producer、原件不全、不同 boot/manager 或当前变化均保留 unknown。即使窄观察成功，整体 `closure` 和既有原服务 jobs、凭据、外部操作及原 controller 配置 provenance 缺口仍保持 unknown。此结果不能升级成维护许可。

## 检查与交付边界

测试使用固定 SDK 对私有 HTTP、实际 go-systemd/godbus 对私有 D-Bus，以及合成 proc/cgroup/文件系统。覆盖全局 disabled 与额外 enabled/null/错身份、早到事件、丢返回/事件、失败 job、并发锁、持久化失败、半写与替换、无重发恢复、自然退出与后代残留、PID/cgroup 复用、timer 重激活、原采集三文件不变及真正诊断 consumer 的整体 unknown。

按 [runner 检查](README.md#构建与检查)运行全 module test/vet、lifecycle race 和静态 admin 构建。测试不连接真实 system bus、GARM、GitHub、LXD、数据库或凭据。源码合入不安装 unit，不执行真实暂停，不恢复服务，不清 guard/记录，不撤销凭据，不释放容量，也不迁移历史 Python 原件。
