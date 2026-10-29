# GARM 管理会话窗口

本文保留自有管理入口及其原凭据、记录和 unknown 合同。新的[正常运维路径](architecture.md)使用标准 GARM 配置、API/CLI，不依赖该自有窗口或状态机。

`runner-admin acquire-garm-session` 在停接窗口轮换 GARM JWT 密钥，固定 24 小时 TTL，再登录一次。成功后，现有 Go 管理入口消费同一份受管 token。`retire-garm-session` 在到期后保存服务端拒绝证据，再移除派生 token 文件。两个入口都保留原操作身份和 unknown，不自动恢复接单。

本能力只覆盖 GARM 管理会话。GitHub 的其他凭据由各自供应者负责，包括 installation、registration 和 Actions admin 凭据。源码合并不执行服务切换、密钥轮换或登录。现场操作按准确运行方案授权，方案绑定前态、影响范围和恢复办法。

## 入口与可信输入

入口固定为 Linux `dev` 宿主的真实 UID/EUID 0。GARM 固定为 `http://127.0.0.1:9997`，版本为 v0.2.1；LXD 使用固定 Unix socket。入口没有任意端点、密码、代理、测试根目录或历史已闭合标志的参数。

以下仅说明调用格式，不是现场运行授权：

```sh
/opt/nucleus-halo/bin/runner-admin acquire-garm-session \
  --request /etc/nucleus-halo/garm-session-request.json

/opt/nucleus-halo/bin/runner-admin retire-garm-session \
  --request /etc/nucleus-halo/garm-session-request.json \
  --issuer-sha256 ORIGINAL_ISSUER_SHA256
```

`ORIGINAL_ISSUER_SHA256` 是原发行成品的准确 SHA256。发行与退役消费相同请求字节。请求为 root 所有、单链接的 `0600` 普通文件，最多 64 KiB。父目录由 root 所有，禁止组及其他用户写入。所有路径分量拒绝符号链接。

请求示例使用合成身份。运行方案必须从准确现场事实取得每个值：

```json
{
  "version": 1,
  "operation_id": "aaaaaaaa-0000-4000-8000-000000000001",
  "controller_id": "bbbbbbbb-0000-4000-8000-000000000002",
  "username": "haloadmin",
  "boot_id": "cccccccc-0000-4000-8000-000000000003",
  "controller_invocation": "11111111111111111111111111111111",
  "controller_sha256": "2222222222222222222222222222222222222222222222222222222222222222",
  "configuration_sha256": "3333333333333333333333333333333333333333333333333333333333333333",
  "ttl_seconds": 86400,
  "expires_at": 1799999999
}
```

`expires_at` 是本次窗口的发送期限，不是 token 的自报期限。配置摘要绑定轮换前的完整 GARM 配置，成品摘要绑定实际 GARM executable。请求拒绝未知字段、重复键、大小写别名、尾随 JSON 和错误数字。入口总期限为十分钟；原请求已发送后的超时仍保留 unknown。

密码只从 `/etc/nucleus-halo/admin-password` 的 root `0600` 文件读取。发行、退役及四个管理消费者在读取秘密前禁用 core dump。数据库子进程也执行相同保护。实现设置 `RLIMIT_CORE=0` 和 `PR_SET_DUMPABLE=0`，并读回进程状态。前者不足以覆盖管道式 core handler，见 [Linux core 合同](https://man7.org/linux/man-pages/man5/core.5.html)。保护只改变调用进程，不修改宿主 sysctl。

非空凭据、代理和 SDK debug 环境沿管理入口的[环境合同](execution.md#固定入口与输入)拒绝。SDK transport 只允许固定 Login POST 和 controller-info GET，关闭代理、重定向及业务重试。输出只含固定状态、operation 和期限；错误只含固定事件及阶段。

## 历史与当前准入

固定历史集合包含三组原件：

- controller-start-verification；
- first-connection acquisition；
- credential-rebind amendment。

观察器消费原格式、原摘要和原 owner.lock。旧 Python 保持停止，App 凭据保持加密。观察不改历史数据库或回执。

原 service job 的路径、unit 和终态事件必须相符。旧 observed-running/stopped 回读不替代原事件，restored 阶段也不证明旧 Login 成功。acquisition 只接纳在外部签发前停止的原目录形状；source、本地 app-sign 意图及响应须交叉绑定，原 App JWT 须已到期。该判断不等于 GitHub 撤销证明。后续 mint、registration、Actions admin、GARM Login、rebind、pending 和未知条目均阻挡窗口。

锁按以下顺序取得：

1. 全局 session；
2. 原 acquisition/rebind owner；
3. 原 controller-start owner；
4. watchdog pause；
5. watchdog operation；
6. provider operation。

所有锁非阻塞。历史与 guard 锁保持原身份，不重建丢失的历史锁。watchdog 只在没有原记录时建立初始永久锁。已有 state 或停止记录时，丢锁仍拒绝。

watchdog 拥有自己的状态解释。未结 Login、禁用意图、原停止请求和部分写入均阻挡窗口。另一原暂停操作也阻挡窗口。持有 watchdog operation 锁期间，新 tick 不能登录。provider 必须为正常 CLEAN。active、intent、latch、维护标记和部分事务均拒绝。启动检查也使用 provider 锁。只有原 controller StartUnit job 的发送与完成观察可以释放一次该锁。随后非阻塞重取，并核原文件未变。

降权数据库子进程使用实际服务 UID/GID 和空附加组，只读核 controller、唯一启用的管理账号及 generation。所有 instance 行均阻挡；pool 和 ScaleSet 必须 disabled。LXD 核所有 project 的 instance、专用池可写卷和 operation。未知或进行中的 operation 阻挡窗口，空列表不单独证明历史执行闭合。

当前进程观察核 procfs、boot 和 PID/starttime。UID、executable 和参数也须核对，并复查完整 PID 清单。旧脚本、特权解释器、其他管理入口、watchdog 和 QEMU 阻挡窗口。当前管理进程与准确 controller 单独绑定。不可读、复用、替换和仍待回收的用户进程拒绝。内核线程按 [Linux PF_KTHREAD 定义](https://kernel.googlesource.com/pub/scm/linux/kernel/git/torvalds/linux.git/+/63bceed808c5cafbac4e20b5a40012a0ec6c6529/include/linux/sched.h)及其他事实核验。观察不重构已经结束进程的结果。其他可信 root 写者仍须遵守维护窗口与原锁。

gateway 须已停止、无 job 且原 cgroup 为空。本入口只观察 gateway，不发送另一个停止请求。观察允许 controller 正处于本窗口的停止阶段，同时继续核加载的依赖和服务合同。ScaleSet、pool、gateway 和 watchdog 均不自动恢复。

## 轮换、单次登录与恢复

窗口按原意图发送三个 systemd 请求：停止 watchdog timer、停止 controller、启动 controller。每次发送前保存意图，保留原 job 路径及实际事件。丢失方法回执或事件时，只沿原记录恢复；inactive、空 cgroup 或另一个新 job 不能替代缺失回执。

配置候选只改变 `jwt_auth.secret` 与 `time_to_live=24h`。GARM 原生验证器和完整语义对比确认其他字段不变。配置交换绑定原 inode、候选、意图和封印；中断后核同一交换，不生成第二把 key，也不自动回滚旧 key。JWT 密钥轮换会影响该 controller 的管理、instance 和 agent token，所以运行资源及其他消费者均须先排空。

启动来源绑定原 StartUnit、新 invocation、实际成品、配置、监听 inode、进程 FD 和网络 namespace。TCP 建连后、发送任何凭据字节前，再核来源。服务 PID 存活不能代替实际 listener 归属。

本地签名探针先证明旧域接受，再证明新域接受新签名并拒绝相同 claims 的旧签名。探针只调用 controller-info GET；它不是找回的历史令牌，也不证明旧 Login 的结果。账号或 generation 变化、探针过期及错误响应均拒绝。缺失的旧令牌继续保持缺失，不补造拒绝样本。

管理 Login 只发送一次。完整原响应先落盘，再核管理身份、controller、generation 和精确的 24 小时期限。原响应丢失、损坏或超限时，保留已发送的事实并拒绝发布。GARM v0.2.1 没有本入口可用的逐 token 提前撤销接口，token_id 不能用来找回丢失响应。

固定父目录 `/var/lib/nucleus-halo-credentials` 预先配置为 root `0700`。`session.lock` 为全局永久锁；`window/` 保存配置、探针和服务原件，`session/` 保存准确请求、登录意图、原响应、验证封印和派生 `garm-token`。文件为 root `0600`、单链接。配置及响应仍含秘密，必须持续留在私有位置，不进入 Git、PR 或公共日志。

`window/` 或 `session/` 任一存在时，都必须取得既有 `session.lock`。锁丢失时拒绝，不为部分窗口重建锁身份。

先保存 verified 封印，再发布 token，最后保存 complete 封印。已有完整原件时，发行入口只读回同一结果；部分槽位不触发第二次 Login。换 operation、请求或目录不会获得重发资格。输出失败不证明发行失败，沿同一请求回读。

## 消费与到期退役

[采集](execution.md)、[暂停](pause.md)、[停止 gateway](gateway.md)、[诊断](closure.md)支持固定受管文件 `/var/lib/nucleus-halo-credentials/session/garm-token`。`--garm-session-sha256` 绑定原发行成品。受管消费核原 window、请求、响应和 complete 封印，并核 controller 和期限。既有外部 raw token 合同继续保留。受管路径缺少 producer 时拒绝。

消费者持有共享 session 租约至 API 使用结束。退役取得独占租约，先核 token 到期及原服务来源。`retirement.response.json` 在同一次持久化中保存实际规范 401 正文、原意图摘要和原观察时间，再据此生成 retired 封印，最后删除原 inode 的派生 token。已有退役意图立即阻挡新消费者，时钟回退也不重新开放。替换文件不被删除。

本地同步回执丢失时，只恢复原封印和删除，不重复已保留的拒绝 GET，不重新登录。原请求、原响应和判断原件继续保留。成功状态 `expiry_rejection_observed_token_removed` 只证明观察到到期后拒绝及派生副本移除，不代表即时撤销、历史 unknown 已闭合或所有秘密原件已销毁。

## 验证边界

检查沿用[原生 Go 入口](../README.md#go-检查)。私有 HTTP、D-Bus、proc/cgroup、文件和 SQLite 夹具覆盖请求丢回执、事件乱序、原件漂移、锁冲突、配置交换、认证域、消费者租约和到期退役。数据库降权与进程保护另在独立子进程和无网络容器验证。

真实历史报告、安装版本、服务窗口、密钥轮换、一次新登录和到期拒绝仍按准确现场计划验收。这里的隔离结果不证明 Nucleus 已接单，也不授予部署、启用 ScaleSet、恢复旧 Python 或清理历史 unknown 的权限。
