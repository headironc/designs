# Provider 异常停接

`provider-watchdog` 供专用 root oneshot 服务消费。它调用现有 guard 的 `--watchdog-check` 接口。返回 `0` 表示本次正常，`75` 表示 guard 正忙；没有未结请求时，两者均直接结束。其他退出、启动失败或超时进入停接流程。Go 入口不改写 guard 状态，也不承担正常 provider 准入或 records-only 启动校验。

已交付异常停接程序、service 和 timer 模板。[Go guard watchdog](guard-watchdog.md) 提供本入口消费的 `--watchdog-check`，包含必要的 guard 状态持久化；[正常启动准入](startup.md)为另一入口。VM 写操作与 records-only 恢复分别交付。当前只构建和离线验证，不安装模板、不启动 timer，不恢复已停止的 Python 流程。完整 Go 运行版本仍依赖后续 provider 实现及实际部署验收。

GARM client v0.2.1 已负责登录、controller 身份查询、ScaleSet 查询和禁用。本片用 go-systemd v22.7.0 替换 systemctl 停止调用，并交付原 job 的观察与恢复。写前记录和恢复判断仍由 watchdog 持有，SDK 不负责重试或决定原请求是否完成。

## 目标与调用

入口固定使用 `/usr/sbin/runuser -u nucleiron -g nucleiron -G lxd -- /opt/nucleus-halo/bin/guarded-provider --watchdog-check`。检查最长 90 秒，取消或超时终止整个子进程组。标准输入和输出接空设备，子进程错误不进入日志。

程序从 root 所有、单硬链接的普通文件读取配置。`/etc/nucleus-halo/provider-policy.json` 的模式为 `0644`，`/etc/nucleus-halo/admin-password` 为 `0600`。符号链接、FIFO、目录、错误权限和超限输入均拒绝。policy 继续由部署能力生成，watchdog 只消费 `controller_id`、`repository_id`、`scaleset_name`；其余字段仍由 guard 解释。标识符必须有效且显式提供，没有历史 repository 默认值。

GARM API 固定为 `http://127.0.0.1:9997/api/v1`，禁用代理和重定向，并关闭 SDK 的请求调试输出。使用现有 `haloadmin` 登录接口，随后核对 controller 身份。列表中只选取名称匹配的唯一 ScaleSet。repository 和 provider `halo-lxd` 必须匹配。发送前再次读取准确 ID，并验证全部身份。GARM v0.2.1 的 `enabled` 使用 `omitempty`，缺省表示禁用；响应 consumer 在 SDK 解码前拒绝显式 `null` 或错误类型。只有已启用的目标才收到一次禁用 PUT：`Enabled` 指针明确提供 `false`；SDK 同时编码的 `enable_shell`、`state`、`extended_state` 为 `null`，按 v0.2.1 更新逻辑均不变更。成功响应为 HTTP 200 和 ScaleSet JSON，随后仍独立读回准确目标为禁用，才报告 `disabled`。合法空列表报告 `no-scaleset`，缺失、`null` 或损坏列表是失败。

每次 GARM 请求最长 15 秒，响应上限为 2 MiB。ID 使用 `int64` 精确解析，并受 GARM 路由的正 `uint32` 范围约束。整个观察流程最长 125 秒；失败停服务另有 15 秒额度。后续 tick 恢复原停止请求时，也有独立的 15 秒观察额度。超时只结束本次观察，不取消或重发原 job。service 的 170 秒上限为退出和清理留出余量。

## 请求记录与恢复

watchdog 在独立的 `/var/lib/nucleiron-watchdog` 保存记录。目录由 root 所有，模式 `0700`；记录和永久锁文件为 `0600`。`flock` 排他锁覆盖检查、网络请求、持久化及失败处置。另一个 watchdog 正在运行时退出 `75`，不替它请求停止。unit 的 `StateDirectory` 创建该目录；直接调用时也验证目录并同步新目录的父目录。

`state.json` 版本为 `1`，保存准确 policy 绑定、`login_pending` 和 `disable_pending`。后者为原请求的 ScaleSet ID，`0` 表示没有未结禁用请求。临时文件写完、同步后原子替换记录，再同步目录。每次外部写入之前先持久化意图；存储失败时不发送该写入。记录不保存密码或 session token。

登录响应丢失时，`login_pending` 保持为真；新调用拒绝再次登录。成功收到 token 后，程序持久化 `login_pending=false`。token 仅驻留内存，用于后续请求。禁用结果不明时保留原 ID，新调用只能读取该 ID。看到仍启用、对象缺失或身份变化均不重发 PUT。准确读回禁用后才清除未结意图。policy 变化和不支持的状态记录均拒绝继续。

停接失败时，程序通过固定的 `/run/dbus/system_bus_socket` 连接 system bus，先读取 bus ID 与 manager 的唯一 owner，再订阅 `JobRemoved`。随后读取 unit、boot 和进程身份。只读准备失败时尚未创建停止记录，也未发送 StopUnit；后续 tick 可以重新准备。准备成功后，程序独占写入并同步版本 `2` 的 `stop-request.json`，保存 boot ID、bus ID 与 manager 的唯一 owner、unit 运行 ID、cgroup 路径，以及主进程和控制进程的 PID/starttime。持久化后再次核对 manager 身份，通过后才调用一次 `StopUnit(nucleiron.service, fail)`。`fail` 模式不替换冲突 job。发送前的停止意图无法持久化时不发送请求；请求后的 job 回执无法持久化时，结果保持未确认且不重发。已经写入的停止记录不会因失败被自动删除。记录不保存凭据。

请求返回后立即持久化原 job ID。只有原 manager 发出的信号，其 job ID、job 路径和 unit 均准确匹配，才作为终态回执保存。job 路径固定为 `/org/freedesktop/systemd1/job/<ID>`。先订阅的顺序能保留早于方法返回的完成信号；[systemd 接口文档](https://www.freedesktop.org/software/systemd/man/latest/org.freedesktop.systemd1.html)也要求按此顺序跟踪请求。未知 job 结果、错误身份或读取失败均拒绝确认。

收到并保存 `done` 后，程序分别核验实际停止状态：没有原 job 或该 unit 的其他 job；unit 为 `inactive` 或 `failed`；MainPID 和 ControlPID 为零；原 PID/starttime 对应的进程已退出；原 cgroup v2 的 `populated` 为零或该组已移除。`populated` 覆盖子 cgroup，PID 复用也不会被当作原进程仍存活。最后再次核 unit、排队 job 和 manager 身份，才保存 `stopped=true`。其他终态保留原结果，不被改写成成功。确认后仍保留停止记录与退出码 `1`，日志报告 `controller_stopped_admission_locked`，不会自动恢复接入。

后续 timer 遇到停止记录时，只进入原请求恢复，不再运行 guard 或 GARM 写入。已保存 job ID、尚未收到终态时，程序重新订阅并观察原 job；已保存 `done` 时，继续核服务和资源状态。job 已从列表消失不证明成功，不能补造丢失的 `JobRemoved`。请求返回前中断、job 回执尚未保存、完成事件丢失、boot 或 manager 改变，均保持未确认，不发送第二次 StopUnit。等待超时后的原请求仍属于原操作。

旧版本 `1` 的 CLI 停止记录没有 job 身份，程序保全并拒绝自动迁移。无法自动确认的记录仍由授权宿主恢复窗口处理：先保全原件，再核原请求及 controller、ScaleSet、guard 的状态。只有恢复操作确认原请求结束后，才可处理停止标记；处理标记不清除 `state.json` 的未结登录或禁用意图。本片不提供自动清记录或重新接入命令。

该目录与旧 guard、records-only permit 及历史恢复目录分开。原操作仍由原恢复能力解释；迁移文件、换目录或删除标记都不是重试授权。

## 验证与消费

沿用 [Go 工具链与构建环境](README.md#构建与检查)。在 `runner/` 执行：

```sh
go test -race -count=1 ./internal/watchdog
go vet ./internal/watchdog ./cmd/provider-watchdog
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o .build/provider-watchdog ./cmd/provider-watchdog
```

测试让真实 SDK 连接本地 HTTP server 和一次性私有 D-Bus，配合临时受保护文件和受控子进程。GARM 测试保留准确目标、写前记录、损坏回执、取消和不重发覆盖。停止测试验证订阅先于发送、快速完成、原 job 恢复、丢失完成回执、boot 改变、失败终态、PID 复用及残留资源。子进程在停止请求已送达、job 回执返回前被 SIGKILL，下一次调用仍拒绝重发。测试不连接宿主 system bus，不操作真实 GARM、LXD 或 systemd 服务。

[service](templates/nucleiron-provider-watchdog.service) 沿用原 guard 所需的 `/var/lib/nucleiron` 写权限；watchdog 自己只写专用目录。原 [timer](templates/nucleiron-provider-watchdog.timer) 的启动及间隔保持。模板消费路径为 `/opt/nucleus-halo/bin/provider-watchdog`。部署时仍需核准确 guard、policy、controller、旧操作与成品；离线通过不证明真实停接。

## 行为来源

已核历史来源包 `halo-20261026` 中的两个文件。`provider.py` 生成当前消费接口、配置形状与 unit；不执行该安装脚本。新 repository 的真实 GARM UUID 尚未交付，测试仅使用合成身份。

| 来源                 | SHA256                                                             |
| -------------------- | ------------------------------------------------------------------ |
| provider-watchdog.py | `ea4bac1419541ca1032491bcbcf5f5c74905d652831f45a400d0e93ed04e766e` |
| provider.py          | `de9f034f6413447a511a7ee82caa571f9abe508e09f3c8d5688f7d399fd95584` |
