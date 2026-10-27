# Provider 异常停接

`provider-watchdog` 供专用 root oneshot 服务消费。它调用现有 guard 的 `--watchdog-check` 接口。返回 `0` 表示本次正常，`75` 表示 guard 正忙；没有未结请求时，两者均直接结束。其他退出、启动失败或超时进入停接流程。Go 入口不改写 guard 状态，也不承担正常 provider 准入或 records-only 启动校验。

本片交付异常停接程序、service 和 timer 模板。guard 与 records-only 是后续完整实施片。当前只构建和离线验证，不安装模板、不启动 timer，不恢复已停止的 Python 流程。完整 Go 运行版本仍依赖后续 guard 实现及实际部署验收。

## 目标与调用

入口固定使用 `/usr/sbin/runuser -u nucleiron -g nucleiron -G lxd -- /opt/nucleus-halo/bin/guarded-lxd-provider --watchdog-check`。检查最长 90 秒，取消或超时终止整个子进程组。标准输入和输出接空设备，子进程错误不进入日志。

程序从 root 所有、单硬链接的普通文件读取配置。`/etc/nucleus-halo/provider-policy.json` 的模式为 `0644`，`/etc/nucleus-halo/admin-password` 为 `0600`。符号链接、FIFO、目录、错误权限和超限输入均拒绝。policy 继续由部署能力生成，watchdog 只消费 `controller_id`、`repository_id`、`scaleset_name`；其余字段仍由 guard 解释。标识符必须有效且显式提供，没有历史 repository 默认值。

GARM API 固定为 `http://127.0.0.1:9997/api/v1`，禁用代理和重定向。使用现有 `haloadmin` 登录接口，随后核对 controller 身份。列表中只选取名称匹配的唯一 ScaleSet。repository 和 provider `halo-lxd` 必须匹配。发送前再次读取准确 ID，并验证全部身份。GARM v0.2.1 的 `enabled` 使用 `omitempty`，缺省表示禁用；显式 `null` 或错误类型仍拒绝。只有已启用的目标才收到一次 `PUT {"enabled":false}`。读回准确目标为禁用后，才报告 `disabled`。合法空列表报告 `no-scaleset`，缺失、`null` 或损坏列表是失败。

每次请求最长 15 秒，响应上限为 2 MiB。ID 使用 `int64` 精确解析，并受 GARM 路由的正 `uint32` 范围约束。整个观察流程最长 125 秒；失败停服务另有 15 秒额度。service 的 170 秒上限为退出和清理留出余量。

## 请求记录与恢复

watchdog 在独立的 `/var/lib/nucleiron-watchdog` 保存记录。目录由 root 所有，模式 `0700`；记录和永久锁文件为 `0600`。`flock` 排他锁覆盖检查、网络请求、持久化及失败处置。另一个 watchdog 正在运行时退出 `75`，不替它请求停止。unit 的 `StateDirectory` 创建该目录；直接调用时也验证目录并同步新目录的父目录。

`state.json` 版本为 `1`，保存准确 policy 绑定、`login_pending` 和 `disable_pending`。后者为原请求的 ScaleSet ID，`0` 表示没有未结禁用请求。临时文件写完、同步后原子替换记录，再同步目录。每次外部写入之前先持久化意图；存储失败时不发送该写入。记录不保存密码或 session token。

登录响应丢失时，`login_pending` 保持为真；新调用拒绝再次登录。成功收到 token 后，程序持久化 `login_pending=false`。token 仅驻留内存，用于后续请求。禁用结果不明时保留原 ID，新调用只能读取该 ID。看到仍启用、对象缺失或身份变化均不重发 PUT。准确读回禁用后才清除未结意图。policy 变化和不支持的状态记录均拒绝继续。

停接失败时，程序先独占写入并同步 `stop-request.json`，再调用一次 `/usr/bin/systemctl stop --no-block nucleiron.service`。成功返回只记为 `requested`，不声称服务已经停止；失败或回执丢失保留 `pending`。日志只报告固定阶段和结果。已有停止记录时，后续 timer 直接报告检查原请求，不重复登录、PUT 或停止请求。状态目录或停止意图无法持久化时，不发送无记录的停止请求，明确报告尚未请求停服。

恢复由后续宿主操作窗口处理。先保全全部记录，再核原 systemd 请求、controller、ScaleSet 和 guard 的实际状态。未结停止请求没有自动重试；原 CLI 不返回 job ID，本片也不宣称提供 job 终态恢复。只有授权的恢复操作确认原请求结束后，才可处理停止标记并恢复观察。清除停止标记不清除 `state.json` 中的未结登录或禁用意图。未知登录结果仍阻塞，新程序不猜测修复；本片不提供自动清记录或重新接入命令。

该目录与旧 guard、records-only permit 及历史恢复目录分开。原操作仍由原恢复能力解释；迁移文件、换目录或删除标记都不是重试授权。

## 验证与消费

沿用 [Go 工具链与构建环境](README.md#构建与检查)。在 `runner/` 执行：

```sh
go test -race -count=1 ./internal/watchdog
go vet ./internal/watchdog ./cmd/provider-watchdog
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o .build/provider-watchdog ./cmd/provider-watchdog
```

测试使用本地 HTTP server、临时受保护文件和受控子进程。覆盖正常与忙碌检查、准确目标、空列表、身份变化、并发锁、精确大整数 ID、登录及 PUT 回执丢失、原目标只读恢复、文件保护、取消和脱敏。不会连接实际 GARM、LXD 或 systemd。实际 CLI 的无权限和非法参数检查也不调用宿主服务。

[service](templates/nucleiron-provider-watchdog.service) 沿用原 guard 所需的 `/var/lib/nucleiron` 写权限；watchdog 自己只写专用目录。原 [timer](templates/nucleiron-provider-watchdog.timer) 的启动及间隔保持。模板消费路径为 `/opt/nucleus-halo/bin/provider-watchdog`。部署时仍需核准确 guard、policy、controller、旧操作与成品；离线通过不证明真实停接。

## 行为来源

已核历史来源包 `halo-20261026` 中的两个文件。`provider.py` 生成当前消费接口、配置形状与 unit；不执行该安装脚本。新 repository 的真实 GARM UUID 尚未交付，测试仅使用合成身份。

| 来源 | SHA256 |
| --- | --- |
| provider-watchdog.py | `ea4bac1419541ca1032491bcbcf5f5c74905d652831f45a400d0e93ed04e766e` |
| provider.py | `de9f034f6413447a511a7ee82caa571f9abe508e09f3c8d5688f7d399fd95584` |
