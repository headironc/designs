# 原执行绑定的 gateway 停止

`runner-admin stop-gateway` 为已封印的 [Go 原执行](execution.md)停止固定 `nucleiron-gateway.service`，保存唯一 systemd job 和原终态事件，再确认原进程与 cgroup 排空。它消费已核验的 [watchdog 暂停](pause.md)原件；未知请求不重发。

本结果只覆盖 gateway。原 unit 的 `Requires=nucleiron-firewall.service nucleiron.service` 使 controller 停止可能连带停止 gateway，因此先独立取得 gateway 原结果。GARM 的固定 loopback 读取不经过 gateway。本入口不停止 controller、firewall 或 watchdog worker，不恢复服务，不处理凭据、许可、guard、VM、记录清理或容量释放。

## 第一次请求

入口要求 root、受控环境、原采集请求，以及 root 提供的原 capture 和 pause producer 摘要。共享 loader 严格核验采集完成封印、准确执行字节和暂停四份原件。原采集与暂停文件保持不变；当前 timer 安静不能代替原 pause job/event/drain。

```sh
/opt/nucleus-halo/bin/runner-admin stop-gateway \
  --request /root/runner-operations/trusted-request.json \
  --operations /root/runner-operations \
  --capture-sha256 "$EXPECTED_CAPTURE_SHA256" \
  --pause-sha256 "$EXPECTED_PAUSE_SHA256" \
  --github-token-file /root/runner-credentials/github-token \
  --garm-token-file /root/runner-credentials/garm-session
```

首次发送复用暂停的完整 live 准入：准确 GitHub repository/run attempt/job/commit/runner，job 已终结且保留真实 conclusion；准确 GARM controller/repository/ScaleSet、全实例及目标实例清单，以及全局 ScaleSet 和普通 pool 均 disabled。额外身份、同名冲突、enabled、显式 null、缺必需证据或未知结果均拒绝。固定 producer 合法省略的 disabled 字段和额外未创建 ScaleSet 的 GitHub ID 沿 [暂停合同](pause.md#第一次请求)解释。本片只支持 public GitHub。

凭据只在首次 live 准入时惰性读取已有 root `0600` raw token 文件。GitHub 文件仍是待授权供应的部署输入；不读取或导出 `gh` OAuth，不登录、mint 或刷新。恢复已有 slot 不读取这些文件。SQLite 当前配置路径与快照不参与发送准入，也不证明 controller 的运行数据库连接。

## 准确服务与发送窗口

首次 gateway 必须 settled active/running、MainPID 非零、ControlPID 为零且无 job。程序从实际加载属性核验固定 nginx 命令、DynamicUser、用户、KillMode、信号、超时和停止行为，拒绝额外停止 hooks、反向停止传播及宿主动作。`SuccessAction`、`FailureAction`、`JobTimeoutAction` 和 `StartLimitAction` 必须明确为 `none`；`SuccessExitStatus` 必须为空。

固定 controller 和 firewall 必须 active，其 `StopWhenUnneeded` 必须为 false。gateway 的直接 Requires/Wants/Requisite 清单中的其他单元也须明确不因失去依赖自动停止。只检查本入口消费的直接关系，不遍历或操作通用依赖图。systemd 的 [依赖与 StopWhenUnneeded 合同](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#StopWhenUnneeded=)及[完成/失败动作](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#FailureAction=)分别决定这些检查。

原身份绑定 boot、bus/manager、invocation、启动时间戳、PID/starttime、实际 UID/GID、可执行文件对象和摘要，以及 cgroup v2 device/inode。nginx 会改进程标题，程序不把其 `/proc` argv 当作不变命令；加载的固定 ExecStart 与实际 nginx image 分别核验。DynamicUser 的身份来自实际进程，不调用 NSS 查找，也不要求恢复时已释放的账户仍存在。

准确 controller 的 invocation、PID/starttime、实际进程和必要加载配置在发送前后保持同一实例，仅保护此次发送窗口。这个核验不证明运行进程加载了当前配置文件的内容，也不证明其数据库连接。最终 drain 保存 `controller_after`，严格绑定 intent 中原 controller 的这些身份事实。

## 永久锁与固定原件

父目录固定为 root `0700` 的 `/var/lib/nucleiron-watchdog`。锁序为已有 `pause.lock`，然后已有 worker `operation.lock`，持有至本次结果持久化；缺失 operation 锁拒绝，不新建或替换。持锁暂停消费者共享原严格 loader，不重复获取同一把锁。

任何已有 `stop-request.json` 都阻止新发送，包括目录或链接。本入口不解释、覆盖或迁移 watchdog 的历史原件。持锁和发送前复查用于排除正常 watchdog 的竞争；部署仍须提供独占受控窗口，root 的并行管理操作不是跨系统事务的一部分。

固定 `/var/lib/nucleiron-watchdog/gateway-stop` 保存四份 root `0600` 不可覆盖原件，目录为 `0700`：

| 原件          | 证明的事实                                                                                                                                                |
| ------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `intent.json` | 版本、operation、准确 execution/capture/pause/gateway producer、原 pause intent/drain 摘要、boot/manager、固定 unit、准入摘要及原 gateway/controller 身份 |
| `job.json`    | 唯一返回的实际 job ID/path，绑定准确 intent 字节摘要                                                                                                      |
| `event.json`  | 原 manager 发出的准确 job/path/unit/result，绑定原 intent                                                                                                 |
| `drain.json`  | 原事件 done 后的 gateway 排空事实与已核验的 controller 发送窗口身份                                                                                       |

文件使用 `O_EXCL` 创建，核 owner、完整路径、普通文件类型、链接数、模式、大小与对象身份，完成文件及目录同步和读回。缺字段、null、半写、未知文件、错 producer、替换或碰撞均拒绝并保留。没有另一个 gateway slot、unit 或锁路径参数。

先订阅，再持久化 intent，然后做最终准入和身份复查，只调用一次 `StopUnit(nucleiron-gateway.service, "fail")`。intent 后的复查失败也占用原 slot：崩溃后的无 job intent 无法证明从未发送。调用失败、丢返回或取消都不能授予重发；已返回的 job 和原事件分别持久化，不以 unit inactive 或 job 消失代替原事件。

## 原请求恢复与排空

重复相同入口只恢复原请求，不做新的平台准入，不发送 StopUnit、CancelJob 或替代操作。它要求准确 execution、生产者、原 pause、boot 和 manager 身份。原 job 非 done 时保留 `gateway_job_failed` 和真实 result；原事件缺失时保持 unknown。

原事件 done 后，程序等待原 Main/Control PID 消失和原 cgroup 无 live 后代。原 PID 的 Z/X/x 回收态仍须匹配原 starttime、UID/GID 和原 cgroup；原路径的 `(deleted)` 只在已知原身份回收时允许。回收态仍待 PID 消失。`populated=0` 不证明所有 zombie 均已 reaped，也不声称枚举了其他 zombie。

首次 stat 仍为 live，而后续出现原 deleted cgroup 或 exe 消失时，程序沿同一 PID descriptor 再核 UID/GID 和 membership，随后确认准确 starttime 与最终回收态。准确原进程只继续等待；已观察的不同 image、身份或 cgroup 对象仍拒绝。

同名 cgroup 替换、PID 复用、invocation/配置改变、gateway 重激活或迟到 job 均拒绝。已观察的身份冲突先于暂态等待；原 PID 回收不能掩盖 cgroup inode 替换。两次 unit 快照保留后一次事实，不用旧快照覆盖已观察的冲突。

如果事件存在但 drain 尚未封存，恢复必须从仍存活的原 controller 核回发送窗口；controller 已停止或换实例时，保留原 job/event 并报告 `controller_window_unknown`，不补造历史。完整有效 drain 已保存 `controller_after` 后，恢复和诊断不再要求 controller 当前 active，允许后续独立授权的 controller 停止。

三分钟是本次等待上限。取消、到期、写入或输出失败不证明请求没有发生；原件可能已有 job、事件或完整 drain。回读确认原文件耐久性后继续原观察，不覆盖或清理 slot。

## 输出与诊断

成功 JSON 的 state 为 `gateway_stopped_drained`，包含 `gateway_job`、`job_result` 和 `service_result`。失败使用固定 stage/code，保留已验证的 job/result；不回显原 error、原响应、凭据或配置。返回码 0 只表示这个 gateway 结果已确认。

[闭合诊断](closure.md)使用可选 `--gateway-sha256` 及 `--pause-sha256`，核可信原 producer 后共享严格 loader，再观察当前 gateway 身份与排空。它不创建锁、slot 或事件，不追加或覆写原件；现有文件和目录同步仅确认耐久性。gateway 窄结果不能把整体 `closure`、原服务 jobs、执行凭据、全部外部操作或原 controller 配置 provenance 缺口升级为 closed 或清理许可。

## 验证与交付边界

测试使用实际 go-systemd/godbus 对私有 D-Bus、固定 GitHub/GARM SDK 对私有 HTTP，以及合成 proc/cgroup/文件系统。覆盖早到事件、丢返回/事件、原事件恢复、持久化与取消边界、双锁并发、历史 stop 阻挡、传播/宿主动作拒绝、动态身份、PID/cgroup 复用与回收同现、controller 窗口中断及后续停服、严格原件和真正诊断 consumer 的整体 unknown。原采集三文件与 pause 四文件逐字保持。

按 [runner 检查](README.md#构建与检查)完成全 module test/vet、lifecycle race 与静态 admin 构建；本片不新增依赖。未改的 provider、数据库降权容器和 nft 集成沿用已核输入的证据。测试不连接真实服务、API、数据库或凭据。源码发布不安装 unit、不执行真实停止、不恢复服务、不部署，也不迁移 Python 历史原件。
