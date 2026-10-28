# 原执行闭合诊断

`runner-admin diagnose-execution` 消费一个已封印的 [Go 原执行回执](execution.md)，分别报告准确 job 的结论和当前资源事实。它可以在服务仍活跃时运行。返回码 0 仅表示诊断报告完整输出，不表示任务通过、资源闭合或获准清理。

当前回执缺少原服务 job、执行凭据闭合和全部外部操作回执。即使当前资源清单为空，报告也保留 `closure: "unknown"`。本入口不签维护许可，不停止服务，不删 VM、记录或凭据，不释放容量，不恢复接单。历史 Python 记录不由本入口读取或迁移。

## 原件与调用输入

入口需要 root 身份。请求、操作目录及凭据沿用采集入口的路径保护、owner、mode、单链接和大小限制。`--request` 指向原请求的准确字节；`--operations` 指向包含该 operation 的原目录。

```sh
/opt/nucleus-halo/bin/runner-admin diagnose-execution \
  --request /root/runner-operations/trusted-request.json \
  --operations /root/runner-operations \
  --capture-sha256 "$EXPECTED_CAPTURE_SHA256" \
  --github-token-file /root/runner-credentials/github-token \
  --garm-token-file /root/runner-credentials/garm-session
```

`EXPECTED_CAPTURE_SHA256` 由 root 依据原采集成品提供。它核对原回执中的 producer，不要求等于当前诊断程序摘要。报告分别给出 `capture_producer_sha256`、`diagnostic_producer_sha256` 和观察时间；不从环境虚构 Git 提交。

采集与诊断共享同一完整 loader：核原请求、执行记录、完成封印、摘要及重建后的值不变量。缺失、半写、格式不支持、错误 producer 或错绑定均拒绝；不调用平台补造原件。重复诊断不改写三个原文件，只在原结果回读时确认它们及目录的持久化。诊断输出交由调用者保存，不在原目录追加可被误当成许可的文件。

两类凭据均只消费已有 root `0600` raw token 文件。GARM 沿既有 session 文件合同；GitHub 文件是仍待授权供应的部署输入。程序不读取或导出已有 `gh` OAuth，不登录、mint、刷新或撤销凭据。调试传输、代理、URL 和凭据环境覆盖仍拒绝，错误只给固定阶段或错误码。

## 各维度的含义

| 维度 | 实际观察 | 不能据此推出的结论 |
| --- | --- | --- |
| `job` | GitHub repository、原 run attempt、job、commit 和 runner 身份；保留真实 conclusion，并另报 run 状态 | 已完成 job 不要求整个 run 已结束；任务 failure、cancelled、timed_out 不变成 success |
| `registration` | 固定 repository 的完整分页 runner 清单，核总数、页链和重复身份 | 目标 GET 的 404、缺页或 null 不能证明注销 |
| `garm` | controller、repository、GitHub endpoint、准确 ScaleSet，以及全局和目标 ScaleSet 实例清单 | GARM 空清单不证明原删除操作已终结；disabled 不构成维护许可 |
| `machine.instances` | 全 project 的完整 LXD 实例、快照和备份；核原 VM UUID、名称、控制器、池、镜像、profile、位置及可写附件 | 同名替换、未知附件、快照或备份残留不等于原 VM 已干净回收 |
| `machine.volumes` | 专用 `halo-ci` pool 的全 project 卷及卷快照，保留原根卷的独立 UUID 和类型、位置、附件关系 | 其他 project 的非 image 卷仍是专用 pool 残留；不能用 VM UUID 替代卷 UUID |
| `machine.operations` | LXD 当前 operation 的完整清单及状态 | 当前未发现 operation 不补足原操作回执；迟到、变化或未知状态保留未知 |
| `host` | 原 boot、PID/starttime、QEMU 身份和 cgroup 设备/inode、`populated` | boot 改变、PID/cgroup 复用或读取失败不等于资源闭合；进程消失与 cgroup 空分别报告 |
| `guard` | provider 拥有的永久锁、保护文件和状态校验；当前 active、intent/latch、policy 摘要与绑定语义 | pending、maintenance 或锁忙只形成读失败；不清 intent/latch，不放宽正常准入 |
| `services` | 固定 controller、watchdog service/timer 的当前 invocation、PID 和 job ID | 当前 inactive 或没有 job 不替代原 JobRemoved 回执 |
| `database` | 固定 GARM SQLite 的同一事务快照、准入状态、目标实例及额外残留 | 数据库快照不是 controller 停接或原外部操作终结的证明 |

`absent_verified` 表示该次完整且身份稳定的清单未找到对应资源。`present`、`residual`、`mismatch` 和 `unknown` 各自保留；不能把错误当空清单。各来源分别有时限，一个来源失败后继续收集其他事实；不声称跨 GitHub、GARM、LXD、宿主和数据库存在全局事务。

LXD 使用固定 SDK 的 `instance_all_projects`、`container_full`、`storage_volumes_all_projects` 扩展。缺扩展拒绝该维度，不降级为单 project 或目标 GET。实例清单使用无字段过滤的 `recursion=2`；固定 qemu producer 在无快照或备份时明确输出 null，按其空集合合同接纳。缺字段仍拒绝。两轮比较准确身份事实，而非只比较清单数量。

GARM v0.2.1 的 `ScaleSet.Enabled` 使用 `omitempty`，合法省略表示 false，与现有 watchdog 相同。显式 null、错误类型仍拒绝；该合同不适用于必需的身份字段。数据库中的 enabled 则必须逐行是整数 0 或 1，NULL、字符串和其他整数均拒绝。

GARM 实例的 provider/agent 分配值也使用 `omitempty`，pending 实例可以合法省略尚未取得的分配值。额外 pending 记录仍计入残留；原目标记录缺少分配绑定时报告 unknown 并保留实际状态，避免把未分配解释成错误身份。全局与 ScaleSet 清单中的必需记录身份缺失、显式 null 或错误类型仍拒绝；实际观察到的不同身份继续报告 mismatch。

guard 的原 policy 摘要及 ScaleSet、池、镜像语义改变会形成 mismatch。原 state 摘要与当前 state 摘要不同可以来自正常后续状态，单独保留当前事实，不自动认定为身份漂移。

## SQLite 与服务身份

首次真实 SQLite 消费使用 `modernc.org/sqlite v1.61.0`，匹配其要求的 `modernc.org/libc v1.79.0`。TOML 消费采用固定 GARM 已使用的 `BurntSushi/toml v1.6.0`，仅解码当前数据库 backend 和文件路径，不调用 GARM 配置初始化、校验或数据库构造。

数据库生产路径固定为 `/var/lib/nucleiron/garm.db`。父 root 只检查元数据和受保护配置，不让 SQLite driver 打开数据库。它通过 `/proc/self/exe` 启动当前准确 image 的受控子进程；子进程使用实际 NSS 中 `nucleiron` 的 UID、主 GID，并清空附加组。子进程读回身份及自身摘要，父进程再核；部署 pathname 被替换不改变这一 image 绑定。内部 worker 不接受任意 SQL 或数据库路径。

子进程用 `mode=ro`、`query_only` 和一个有界读事务读取 WAL。它不使用 immutable，不复制裸 DB，不做 migration、checkpoint 或 SQL 写入。[SQLite WAL 合同](https://www.sqlite.org/wal.html)允许只读连接维护 SHM，因此由服务身份承担这部分系统行为。现存 DB/WAL/SHM 必须是该 UID/GID 的单链接普通文件，拒绝符号链接及组或其他用户权限；完整父路径也受保护。观察前后核对象身份，新增 SHM 同样核服务所有权，不 chown 修复。正常 WAL 内容变化不否定事务快照。

诊断只读取固定六表所需列：`controller_infos`、`repositories`、`github_endpoints`、`scale_sets`、`pools`、`instances`。声明类型与主键按固定 GARM 模型核对，不发明 NOT NULL 要求。实例外键是 `scale_set_fk_id`，不同于 GitHub ScaleSet ID；包含软删除和全部额外实例。目标 mismatch、soft_deleted 与额外实例数量分别保留，不因行顺序互相覆盖。

当前 `/etc/nucleus-halo/garm.toml` 要求 root owner、服务主 GID、`0640`、单链接、受保护路径和有界内容；读取前后核内容与对象。只确认 backend 为 sqlite3、db_file 为固定路径，不公开原配置、秘密字段或整份敏感配置的摘要。

服务进程事实最多确认它的准确 PID/starttime、UID、exe 和 `-config /etc/nucleus-halo/garm.toml` 路径。即使该进程与参数稳定，root 也可能在服务启动后改过配置；当前文件不证明进程已加载的内容。因此报告始终保留 `runtime_database_connection_unknown`。当前文件/进程路径的组合观察与原控制器配置 provenance 缺口是不同事实，均不冒充运行连接已经绑定。

## 验证与后续边界

普通 lifecycle/provider 测试使用真实 SDK 对私有 HTTP/Unix fixture、私有 proc/cgroup、合成 SQLite WAL 和私有 D-Bus。覆盖四种 job 终态、错 attempt/agent/VM/卷、null/缺页/重复、同名替换、相同数量身份变化、快照/备份/其他 project 残留、旧 boot/PID/cgroup 复用、原 seal/producer 拒绝，以及重复诊断不改原件。

SQLite fixture 的 DDL 独立来自固定 GARM 模型。测试证明仅存在于 WAL 的提交可见、同事务保持旧快照、新事务看到新提交，且 query_only 拒绝 SQL 写。普通测试会跳过需要 root 降权的容器用例，不能把该 skip 当作身份已验证。

实际 UID/GID/groups、相同 image、错误身份、取消和输出上限另在一次性无网络 Docker 容器验证。只挂载该片测试成品，不挂宿主业务 socket、DB 或凭据；容器中创建 UID 24567/GID 24568 的私有 NSS fixture。`RUNNER_DIAGNOSTIC_PRIVATE_CONTAINER=1` 只用于该环境。用例还要求 Docker 文件系统、固定 `/tmp/lifecycle.test`、事先不存在的私有数据库目录，拒绝在已有业务目录上运行。

构建、race、vet、秘密检查和文档检查按 [runner 检查](README.md#构建与检查)执行。以上离线验证不代表真实生产 API、当前数据库或凭据已验收。本片只交付诊断；原服务/凭据/外部操作证据的 producer、records-only 维护、清理准入及部署仍是后续独立责任。
