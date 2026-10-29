# 首次 disabled 连接

本文保留自有管理入口及其原凭据、记录和 unknown 合同。新的[正常运维路径](architecture.md)使用标准 GARM 配置、API/CLI，不依赖该自有窗口或状态机。

`runner-admin connect-disabled` 核对 `headironc/nucleus` 的首次连接。结果绑定 GARM repository、GARM ScaleSet 和 GitHub ScaleSet。新 ScaleSet 保持 disabled，容量固定为 1，空闲数为 0。结果供现有执行身份采集入口核对。启用、创建 VM 和执行 job 使用独立准入。源码验证使用合成凭据和隔离服务；现场操作另行授权。

## 输入与来源

入口固定宿主 `dev`、root、受控环境及禁止 dump 的进程。可信请求文件由 root 所有，模式为 `0600`，最大 64 KiB。请求包含 `version: 1`、`credential_source`、`repository_mode`、原仓库接受事实、模板摘要、私有镜像 fingerprint、bootstrap 摘要与原生 GARM `scaleset` 参数。

`credential_source` 使用[观察凭据](github-observer.md)的可信来源格式。入口消费原 [GARM 会话](session.md)，保留历史 unknown 阻断、watchdog 和 provider 空闲租约、原认证域与 controller 身份。它不再次 Login。数据库观察在服务 UID/GID、空附加组的只读 WAL 快照内进行；资源清单允许新增 disabled 对象，凭据、endpoint、用户及 generation 的稳定摘要保持不变。观察凭据入口仍要求整个清单不变。

`repository_mode: create` 要求没有已接受仓库 ID 与摘要。`existing` 则要求可信输入明确提供原 UUID 和不可变身份投影摘要；同名查询结果不能成为接受事实。投影覆盖 owner、name、credential ID、endpoint、创建时间、agent mode 与 balancer。运行中的 manager 状态另外核对。

ScaleSet 固定使用 `halo-lxd`、Linux amd64、`halo-ci` flavor、`Default` runner group、`disable_update: true`、`enable_shell: false`、`min_idle_runners: 0`、`max_runners: 1` 及 `enabled: false`。额外参数仅接受 `disable_updates` 和原 Go bootstrap 模板字节。系统模板由 GARM GET 读取并核摘要；LXD 通过固定 Unix socket 在 `halo-ci` project 内读取私有 VM 镜像及原 alias，核 fingerprint 和类型。

创建 repository 时另传 `--webhook-secret-file`。该文件由 root 所有、模式为 `0600`，原字节与请求摘要相符。controller 配置关闭自动 webhook 管理。CLI 不接受任意 API endpoint 或 token 参数。

## 凭据与单次写入

入口只签一次 App JWT。先签发 metadata read 审计 installation token，核完整 installation 和唯一 Nucleus 仓库范围，撤销并实际观察拒绝。随后只签发一次单仓库管理 token，权限恰为 metadata read 与 administration write。它取得一次 registration token，再通过原 RemoteAuth 协议取得 Actions admin 会话，随后立即退役管理 token。

所有 POST 和 DELETE 在发送前持久化原意图。响应先保存，之后才由固定 SDK 解码和业务规则判断。意图存在而原响应丢失时不重发写入，不补签 JWT，也不换 operation。GET 使用独立观察身份，以免重启后将创建前的空清单当作当前事实。GitHub 请求禁代理和重定向；GARM 固定使用 `127.0.0.1:9997`，发送 bearer 前重新核 controller 与监听来源。

入口只创建一次 repository 和一次 repository ScaleSet。每次创建前核全局 repository、ScaleSet、pool、instance 清单，以及准确名称的远端 ScaleSet；已有实例、enabled 资源、重复目标或身份漂移均阻断。创建后用原回执固定对象 ID，另取当前两端事实并再次核本地 ID。后台重建产生的新远端 ID 不能替代原创建结果。

## unknown 与恢复

固定状态位于 `/var/lib/nucleus-halo-credentials/connection`，沿用永久 `session.lock`。写入与退役持独占租约，执行采集持共享租约。目录、文件、权限、原内容和 inode 均参与检查；原请求和 producer 摘要保持不变。

repository 或 ScaleSet 创建没有可接受原回执时，结果保持 unknown。入口最多对原 controller 发送一次 StopUnit，保存原 job、event 及排空事实，不调用 StartUnit。丢失 StopUnit 回执时不重发；已有 job 则只恢复其原事件和排空。controller、boot 或 manager 更换会阻止停止替代实例。当前对象列表为空不构成重建许可。

原创建回执已知、后续 GET 失败时，恢复只读核验并继续原事务。最终 `verified.json` 绑定原请求、凭据原件、创建回执及两端观察；部分发布只恢复派生 `connection.json` 与完成封印。已经发布的原结果可以离线读回，不获取新凭据。开始显式退役后，未完成的创建事务不能继续。

`capture-execution --disabled-connection-sha256=<producer>` 消费固定连接槽。它核原封印，再将 controller、Nucleus repository、ScaleSet、pool 和镜像绑定到既有 guard 与平台观察。当前两端实际状态仍由执行采集核对，历史 disabled 事实不替代执行准入。

## 分开的退役结果

`retire-connection-credentials --request=<original-plan>` 只处理本次连接产生的临时凭据，不依赖 GARM 会话仍有效。它不获取新 token，不调用 GARM 或 LXD，不结束未知创建。已知 token 在错误路径进入有界退役；原响应丢失仍保留 unknown。

| 凭据                          | 证据与结果                                                                                               |
| ----------------------------- | -------------------------------------------------------------------------------------------------------- |
| 审计和管理 installation token | 原 DELETE 及实际 `401`；删除回执未知时，等原到期后观察拒绝，保持与撤销的区别                             |
| registration token            | 原 `expires_at` 和官方一小时到期合同；到期后报告 `documented_expiry_elapsed_rejection_unverified`        |
| 本入口的 Actions admin 会话   | 原 HTTPS 响应 JWT 的 `exp` 投影，保护至原到期；随后固定 GET 实际 `401`，报告 `expiry_rejection_observed` |
| GARM manager 内部缓存会话     | 仍由 controller 的原运行及停止责任处理；本入口无法枚举或证明全部退役                                     |

Actions 的期限解析不是本地签名验证。registration 没有已核实的无副作用拒绝观察接口，因此到期结果不称“已撤销”或“全部凭据闭合”。所有私钥、token、数据库、运行配置和真实回执均留在受保护运行目录，不进入 Git 或诊断输出。

## 验证与边界

受影响检查覆盖真实 go-github/GARM SDK 请求形状、固定 HTTP transport、受保护文件、共享与独占租约、SQLite/WAL 来源、原 controller job 恢复、创建响应丢失、两端身份漂移、部分封印与分别退役。LXD 测试连接一次性私有 Unix server；原生检查的私有 D-Bus、降权容器及 nft namespace 沿用[完整 Go 验收](../README.md#go-检查)。

这些检查证明源码及隔离恢复行为。首次真实连接、原 controller 停止、到期拒绝、VM 单任务闭合、连续排队及 Nucleus/Kiln 消费仍分别需要现场证据。
