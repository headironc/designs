# Guard watchdog 检查

`guarded-provider --watchdog-check` 由现有 [Go watchdog](watchdog.md) 以 `nucleiron` 身份调用。它观察运行条件，并持久化必要的 guard 状态。检查成功退出 `0`，同一 provider 操作锁忙退出 `75`，拒绝或失败退出 `1`。忙不表示健康。root watchdog 继续负责准确 ScaleSet 的停接和原停止请求；它不直接写 guard 状态。

## 观察与状态转换

入口复用正常启动的身份、文件保护、摘要和永久锁校验。正常模式下，任何维护标记或读取错误都拒绝，且不改维护许可。旧 watchdog 本身没有这项维护标记检查；新入口明确仅提供正常模式检查。维护和安装窗口仍须确认 timer 与原 worker 已停止，不能只停 timer。

| 观察 | 状态结果 |
| --- | --- |
| 有合法未结 intent | 保留原 intent 与 active，写阻断删除的 latch；必要时升级已有非阻断 latch |
| 已有 latch、无 intent | 原字节保留并拒绝，不自动清除或降级 |
| 缺少挂载、空间储备不足或依赖服务不可用 | 写 `blocks_delete:false` 的 latch 并拒绝；它仍阻止新建，删除由自己的完整规则判断 |
| 实例归属或身份不符，或合法 JSON 中的 ready 回执内容无效 | 写阻断删除的 latch 并拒绝 |
| 没有 active，或 active 已 ready | 条件正常则通过；不清除容量，也不对已 ready 实例追加持续健康检查 |
| 未 ready 的 active 有准确回执 | 原子写入 `ready:true` 与正 `int64` agent ID，保留实例身份、原时间及容量 |
| ready 文件不可用、超限或不是合法 JSON，且尚未满 600 秒 | 保持等待状态，通过本次检查，不宣称已 ready |
| 等待达到 600 秒 | 写阻断删除的 bootstrap 超时 latch 并拒绝 |
| 其他观察失败 | 写非阻断删除的观察失败 latch 并拒绝；取消或写入失败时不宣称 latch 已落盘 |

已有 latch 保留首次原因和时间。只有从允许后续删除升级为阻断删除时增加 `operation_failure`。任何分支都不执行 native provider，不创建或删除 VM，不清除 intent、active 或 latch，也不解释 records-only 许可。

挂载观察要求 `/var/snap/lxd/common/halostorage` 至少保留 116 GiB，`/var/log/nucleiron/guests` 至少保留 256 MiB。使用固定目录描述符的 mount ID 区别真实挂载和普通目录，再读取该文件系统的可用空间。要求 Linux 5.8+ 的 `STATX_MNT_ID`；内核不支持就拒绝，没有宽松回退。

通过 systemd D-Bus 读取 `nucleiron-logs`、`nucleiron-gateway`、`nucleiron-firewall`、`nucleiron-dns` 和 `nucleiron-egress` 的 `ActiveState`。五者均须为 `active`，连接和这一组读取共用最长 10 秒窗口。不发送服务状态变更。

LXD 客户端固定连接本机 Unix socket，读取 `halo-ci` project 的准确实例和 bootstrap ready 文件。核 controller、pool、镜像摘要、唯一 profile、VM 类型、实例名和 UUID。实例或回执中的缺失、空值、错误类型不能被 SDK 的零值当成有效身份。agent ID 不经浮点转换。

## 锁、持久化与失败保全

同一个永久 `operation.lock` 覆盖状态读取、观察、转换和落盘。状态目录及文件保护沿用 [正常启动](startup.md)。正常启动只判断是否准入，watchdog 可以读取合法 intent/latch 并执行对应转换；二者不能共用“必须为空”的状态判定作为加载器。

每次写入先独占创建 `state.pending`，记录原状态和下一状态的 SHA256，文件和目录同步后才创建 `state.new`。同步新文件后原子替换 `state.json`，同步目录并读回准确内容，最后删除 pending 标记并再次同步目录。标记与临时文件均为服务用户所有的 `0600` 文件。

中断、磁盘或权限故障可能留下旧状态、已替换的新状态、pending 标记或临时文件。存在任一事务文件时，正常启动和 watchdog 都拒绝，不删除、不覆盖、不自动重放。需在已停止原进程的维护窗口核对原材料与当前状态；本片不提供自动恢复或清除命令。替换后的同步错误不能解释为“原件未变”。最后一次目录同步失败时，新状态已确认持久化；标记可能在重启后重现，仍拒绝放行。

SIGTERM/SIGINT 取消观察，最外层检查限时 85 秒。现有 root watchdog 的 90 秒限制与进程组终止保持。LXD 每次请求最多 15 秒、响应最多 2 MiB，固定 socket 拨号支持取消。只允许 GET，不走代理、不接受重定向、不重试业务写入。日志只有固定阶段，不输出输入、上游响应或秘密。

## 包、验收与消费

本片沿用 `golang.org/x/sys v0.49.0`，接入 `github.com/coreos/go-systemd/v22 v22.7.0`。LXD 客户端固定为 6.10.0 标签对应的 `v0.0.0-20261012123619-3c94d4fae7ff`，对应保全来源中实际观察过的服务版本；连接要求 `virtual-machines` API extension。实际安装仍需核宿主版本及配置，离线测试不证明部署兼容性。

`govulncheck v1.9.0` 对 LXD 报告 5 项模块范围告警，扫描不是零告警通过。公告中的自定义版本不能自动映射为 Go 版本：[3999](https://pkg.go.dev/vuln/GO-2025-3999) 和 [4003](https://pkg.go.dev/vuln/GO-2025-4003) 修复于 6.5，[4121](https://pkg.go.dev/vuln/GO-2025-4121) 于 6.6，[4595](https://pkg.go.dev/vuln/GO-2026-4595) 于 6.7，[5306](https://pkg.go.dev/vuln/GO-2026-5306) 于 6.8。本片绑定的标签是 6.10.0；只构建固定 Unix socket 的 GET 客户端，不包含 LXD daemon、UI、证书管理或存储操作端点。因此这些报告不构成本候选的已知阻断，也不证明运行宿主安全；安装时仍核实际 daemon。

Linux 验收运行 `go test -race -count=1 ./...`、`go vet ./...`、`go mod verify`，并静态构建 `guarded-provider` 和 `provider-watchdog`。SDK 测试使用本地 Unix HTTP 服务及私有 `dbus-daemon`，不访问宿主 LXD 或 systemd。覆盖状态转换、599/600 秒边界、准确整数、绑定错误、响应上限、取消、锁争用、进程中断、替换前后故障和再次进入。普通挂载观察测试仅只读 `/proc`；不创建或修改挂载。

现有 `provider-watchdog` 的子调用路径改为 `/opt/nucleus-halo/bin/guarded-provider --watchdog-check`。正常启动也消费这个二进制，后续增加了[单次 VM 创建入口](create.md)。GARM external provider 路径保持独立，完整切换等待其余命令及部署验收。成品配套包含两个 Go 二进制与相应文档；watchdog 切片不安装、不启动 timer，不恢复 Python，不启用 ScaleSet。旧未结操作仍由原恢复责任处理。
