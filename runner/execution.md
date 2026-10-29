# 原执行身份采集

`runner-admin capture-execution` 为一个已经分配且正在运行的准确 job 保存原身份。入口消费受保护的可信请求，再读取 GitHub、GARM、LXD 和宿主事实。回执只证明采集时的对应关系，不证明 job 成功、资源闭合或清理资格。尚未分配、bootstrap 失败和历史缺失身份不在本入口的接纳范围。

采用[首次 disabled 连接](connection.md)时，传入 `--disabled-connection-sha256=<producer>`。入口在共享租约内核固定连接槽的原封印。原 controller、仓库、ScaleSet、pool 和镜像与当前 guard 及两端实际观察相符，才接受采集。该参数不授予启用许可，也不替代新执行的凭据和准入检查。

`internal/lifecycle` 拥有可信请求、观察记录、完成封印和原结果回读。`internal/provider` 继续拥有 policy、active、永久锁与正常模式准入。采集不创建 VM、不注册 runner、不派发或取消 job，也不修改 guard、启停服务、迁移记录或释放容量。

## 固定入口与输入

入口只接受 Linux root 的实际 UID 和 EUID。GitHub 固定为 `https://api.github.com`，GARM 固定为 `http://127.0.0.1:9997`，LXD 固定为 `/var/snap/lxd/common/lxd/unix.socket`。project、存储池和 profile 均为 `halo-ci`。入口没有端点、代理、命令或测试根目录参数。

下面是调用格式，不是部署或现场执行授权：

```sh
/opt/nucleus-halo/bin/runner-admin capture-execution \
  --request /etc/nucleus-halo/execution-request.json \
  --operations /var/lib/nucleus-halo/executions \
  --github-token-file /etc/nucleus-halo/github-observer.token \
  --garm-token-file /etc/nucleus-halo/garm-observer.token
```

请求和两个凭据文件都要求 root 所有、普通文件、单链接及 `0600`。入口只读消费原文件，不更改模式。每份 raw token 最多 64 KiB，文件可附带一个换行；入口不登录、签发、刷新或导出凭据。请求最多 64 KiB。父目录均由 root 所有且禁止组和其他用户写入；所有路径分量拒绝符号链接。operation 根目录预先配置为 root 所有的 `0700`，不能放在服务用户可写目录之下。

GARM 的既有 producer `acquire-session.py publish_token` 使用 root `0600`、单链接及 raw token 加换行。原 `lifecycle.py API.garm` 按同一合同 strip 后读取，本入口消费相同形状。原 GitHub 路径通过 Human 的受控 CLI profile 调用 `gh api`，不导出 token。本 Go 入口的 GitHub token 文件是尚待运行授权和供应的部署输入；当前只定义并测试消费，不读取或导出既有 CLI OAuth，也不新建凭据。历史 receipt 不迁移。

受管 GARM 凭据由[管理会话窗口](session.md)供应。使用固定受管路径时，另传 `--garm-session-sha256` 绑定原发行成品。会话来源、共享租约、期限和进程秘密保护的合同由该页维护；采集入口仍不自动登录或刷新。

非空的 `GH_*`、`GITHUB_*`、`GARM_*`、`LXD_*` 和代理环境变量均拒绝。TLS 信任文件的 `SSL_CERT_FILE`、`SSL_CERT_DIR` 覆盖也拒绝。入口拒绝 `DEBUG`、`SWAGGER_DEBUG` 和 `GODEBUG` 中的 `http2debug`，并独立关闭 SDK debug。SDK transport 只允许固定服务的 GET，禁用代理和重定向。每次 HTTP 请求限时 15 秒，每份解码后的响应最多 2 MiB。整个入口限时 90 秒。错误只输出固定事件和阶段，不输出 token、路径、API 原文或 SDK 错误正文。

可信请求采用显式版本，不接受未知字段、重复键、大小写别名、尾随 JSON、分数或越界 ID：

```json
{
  "version": 1,
  "operation_id": "aaaaaaaa-0000-4000-8000-000000000001",
  "repository_id": 9007199254740993,
  "owner": "example",
  "repository": "consumer",
  "run_id": 9007199254740995,
  "attempt": 2,
  "job_id": 9007199254740997,
  "commit": "0123456789012345678901234567890123456789",
  "controller_id": "bbbbbbbb-0000-4000-8000-000000000002",
  "garm_repository_id": "cccccccc-0000-4000-8000-000000000003",
  "garm_scaleset_id": 27,
  "garm_instance_id": "dddddddd-0000-4000-8000-000000000004"
}
```

operation、controller、GARM repository 和 instance 使用准确的小写 UUID。GitHub repository/run/job 使用正 `int64`；attempt 使用正整数且不超过 `2147483647`。GARM ScaleSet 使用正 `uint32`，符合 v0.2.1 路由的整数边界。GitHub ScaleSet ID 另存，不替代 GARM 内部 ID。可信编排负责提供本次执行的原 operation；本入口不生成另一个 operation 来绕过原记录。

## 观察链与拒绝规则

provider 的 root 只读入口按实际 `nucleiron` UID 校验已有 guard 文件，持有同一永久 `operation.lock`。正常 provider 入口仍要求服务身份。只有 `active.ready=true` 且已有正 agent ID 的正常状态可以采集；intent、latch、维护标记、`state.pending` 和 `state.new` 均拒绝。policy 身份和 native/config 摘要仍由 guard 现有规则核对。采集失败不改变这些文件或容量。

GitHub repository 的 ID、owner/name 和 full name 匹配请求。准确 run attempt 的 run ID、attempt、commit、repository 和状态也匹配。准确 job 的 run、attempt、commit 及 `in_progress` 状态匹配后，正 runner ID 和 runner name 才与 guard 的 agent/name 建立关系。所有必需 SDK 指针字段缺失或 `null` 都拒绝。

GARM 的 controller、repository、ScaleSet 和 instance 与请求及 guard 对应。实例 UUID、名称、provider ID、`halo-lxd` provider、ScaleSet 外键及 agent ID 均匹配；实例为 `running`，runner 为 `active`。provider pool 始终使用 ScaleSet name 加 `-` 加 repository UUID。GARM repository 和 ScaleSet 的 forge endpoint 均要求 `github` 类型及公共 GitHub 的 HTTPS base/API URL，允许 URL 的空路径或单个尾斜线。v0.2.1 producer 要求并原样返回这两个 URL，缺失值不能解释成默认端点。

LXD 读取准确 VM 的 UUID、controller、pool、镜像、类型及唯一 profile，并核 running PID。任何进行中或未知状态的 LXD operation 均拒绝。expanded devices 只接受一个专用 root disk、受控桥接 NIC 和 none 设备。NIC 沿 `network.py` 的 profile producer 绑定 `network=nucleiron0`，不带与 managed network 冲突的 `nictype` 或 `parent`。入口通过 SDK GET 核实该网络仍为已创建的 managed bridge。固定 LXD 的 `ExpandedDevices` 保留原网络引用，缺少 `nictype` 不能证明普通虚拟 NIC。physical、SR-IOV、未知网络类型、其他磁盘、宿主路径和 raw QEMU 配置均拒绝。实例快照和 root 卷快照均要求明确为空。

root 卷由实际存储 API 读取，保存名称、独立的 `volatile.uuid`、类型、pool、project、content type、location 和 used_by。卷 UUID 由 LXD 独立生成，不以 VM UUID 代替；缺失 UUID、未知附件或额外可写卷均拒绝。专用池允许 image 卷，除此之外只有准确 root 卷。字段来源是固定 LXD 的 `shared/api.StorageVolume` 及 `lxd/storage/backend_lxd.go` 的 `GetNewVolume`，不从 guest 自报数据补造身份。

宿主读取实际 procfs 和 cgroup v2。记录 host boot ID、QEMU PID、`/proc/PID/stat` 第 22 字段的 starttime ticks、实际 executable 路径和设备/inode、cgroup 路径和设备/inode。QEMU executable 限于 root 所有且不可由组或其他用户写入的 `/snap/lxd/<revision>/bin/qemu-system-x86_64`。准确 `-uuid` 参数只出现一次，值与 VM UUID 相同。根 cgroup、错误路径、符号链接或不包含该 PID 的组均拒绝。

采集做两轮完整观察并比较。每轮宿主读取复核 boot、PID/starttime、命令行、实际 executable 和 cgroup。provider 在同一永久锁内完成最后准入检查，再调用 lifecycle 的发布闭包。完成封印前，闭包再核 guard 原字节、policy、native/config 与进程身份。封印后只释放锁和文件，不以新的观察撤销已记录的原身份。永久锁阻止受管删除，但不制造跨 GitHub、GARM、LXD 和宿主的全局原子快照。其他合法写者遵循 guard 锁，部署修改遵循停接维护边界。这些仍是现场前提。

## 独占持久化与原结果

入口先在受保护根目录独占创建 `<operation_id>/`，同步父目录，再用 `O_EXCL` 保存准确请求字节为 `request.json`。原请求一经保留，同一 operation 就不能绑定另一请求或 VM。观察失败保留部分目录，下一次调用拒绝重发采集。

成功记录为 `execution.json`，包含格式版本、实际运行的 runner-admin 成品 SHA256、原请求 SHA256、预期身份、实际身份，以及 guard state、policy 和 native/config 的 SHA256 摘要。记录另存 UTC 采集时间，未保存 guard 原字节。入口从 `/proc/self/exe` 计算实际成品摘要，不从环境猜测 Git commit。文件最多 128 KiB。记录中没有 API 原文、凭据或 guest 自报的身份授权。

入口独占创建文件，同步并读回后，再核受影响身份。只有这些检查完成，才独占创建并同步 `complete.json`，封印版本和准确 `execution.json` SHA256。目录及文件均核 owner/mode、路径身份、普通文件和单链接。所有文件使用 `0600`；所有目录使用 `0700`。准入或封印前检查失败不会发布完成封印。缺少完成封印的 `execution.json` 只是部分材料，不能作为完成回执。封印写入、同步或输出失败则可能留下完整原件，其结果沿原 operation 回读确认，不能把所有错误解释成未完成写入。

同一 operation、准确相同请求字节和受支持的 producer 成品，回读完整原件后返回 `execution_identity_original`。该路径先同步原文件、operation 及父目录，再读回字节和身份；原完成写入的同步结果未知时，这一步确认原件耐久性。同步失败继续拒绝，不重写、不重新观察平台，也不把旧观察解释成当前闭合。新采集返回 `execution_identity_captured`，两种结果均带 `version:1`。不同输入、producer 摘要或版本、损坏原件、部分目录、链接和碰撞全部保留并拒绝。输出丢失时先沿原 operation 读回；没有完成封印的材料交原操作恢复，不换 operation 跳过。

## 检查与后续边界

离线检查使用实际 go-github v84.0.0、GARM v0.2.1 和固定 LXD SDK。私有 HTTP/Unix socket 夹具覆盖准确整数、错 repo/run/attempt/job/agent、GARM 及 VM/卷错绑定、缺失/null、响应上限、重定向和禁止写入。文件与进程检查覆盖永久锁、替换、链接、PID 复用或退出、boot/cgroup 变化、并发、取消、部分回执和原结果回读。真实子进程测试只启动测试成品，不运行 runner 或真实 VM。

构建和受影响 Go 检查沿用 [README](README.md#构建与检查)。测试不访问真实 GitHub/GARM/LXD，不连接宿主 system bus，不安装服务或部署成品。[原执行闭合诊断](closure.md)通过共享的完整 loader 消费这份原身份。诊断使用可信输入校验原 producer 摘要，另计算并报告当前诊断程序摘要；records-only 维护窗口、正常服务恢复、尚未分配执行和完整生命周期仍需独立交付。
