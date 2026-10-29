# Runner 精简架构与迁移边界

正常路径采用 GARM v0.2.1 与上游 LXD provider v0.1.5。GitHub 原生 job container 消费固定 Kiln digest。GARM、provider 和 LXD 承担实例生命周期；本项目维护必要配置、镜像和消费适配，不再建设第二套正常容量或资源闭合状态机。

这是已采用的目标架构，部署切换尚未实施。仓库已有 Go 程序继续保全；代码合入不表示现场已经切换。旧 guard/gate 退出目标正常路径，历史 unknown 及原程序合同由[历史恢复协议](records-recovery.md)维护。未合入的四 unit 隔离 WIP 仍暂停，不是当前源码的已交付能力，也不是新路线的前置。

## 正常目标流与事实所有者

GitHub 接收仓库原生 workflow，负责排队、分配、job 结论、取消及超时。GARM ScaleSet 使用 ephemeral/JIT 为新 runner 创建唯一实例，直接调用上游 provider。LXD 从无实例身份的固定基础镜像创建 VM。VM 内官方 runner 使用原生 job container 拉取固定 Kiln，执行实际业务检查。

完成消息进入 GARM 的清理路径。provider 停止原实例、请求删除并等待 LXD operation。GARM 处理删除结果后，更新其管理的实例和容量。正常链路不经过自有 guarded-provider，也不生成每个 PR 必须消费的 closed seal。上游行为见 [ScaleSet](https://github.com/cloudbase/garm/blob/v0.2.1/workers/scaleset/scaleset.go)、[完成消息](https://github.com/cloudbase/garm/blob/v0.2.1/workers/scaleset/scaleset_helper.go)、[实例状态机](https://github.com/cloudbase/garm/blob/v0.2.1/workers/provider/instance_manager.go)和 [LXD provider](https://github.com/cloudbase/garm-provider-lxd/blob/v0.1.5/provider/lxd.go)。

| 事实                                | 正常 owner                         | 项目消费边界                                                |
| ----------------------------------- | ---------------------------------- | ----------------------------------------------------------- |
| commit、run/attempt/job、结论       | GitHub Actions 与消费仓库 workflow | 使用原生 checks、artifacts 和 API；guest 自述不授予管理权限 |
| runner、JIT、ScaleSet、数据库状态   | GARM 与上游 worker                 | 使用支持的配置、API/CLI；不直接写库或维护镜像状态库         |
| VM、根盘、operation 与 project 配额 | provider 与 LXD                    | 接受服务成功合同，异常沿原对象调查                          |
| VM 基础镜像与 bootstrap             | 版本化镜像配方、上游模板及必要配置 | 无长期凭据和既有 runner 身份；提供 Docker、CA 与日志环境    |
| Kiln 及业务检查                     | Kiln 发布、Nucleus 原生 CI         | 固定 registry digest，核工具、权限和环境兼容                |
| 旧 unknown 与迁移资格               | 原恢复 owner                       | 保留原件和原判断，不复制成新正常状态机                      |

初始采用一个仓库级 ScaleSet，min-idle=0、max-runners=1。专用 LXD project/profile 设置 VM 数量、CPU、内存和根盘上限，并隔离其他控制器及设备。配额约束 LXD 管理的资源，不独立证明内核孤儿消失。磁盘 quota 也不保证物理空闲；存储池、缓存容量和空闲告警须另行配置。

## 凭据与不可信执行边界

每个 job 使用新 VM，VM 承担与可信控制面的隔离。guest Docker 只提供任务容器环境。App 私钥、LXD socket、管理 token、宿主目录与发布凭据不进入 guest。guest 只取得本实例的 bootstrap/JIT 和任务所需短期 token。镜像不得带已有 runner 身份。

GitHub App 安装范围、workflow permissions、Secret/Environment 和私有 Kiln 授权使用平台标准配置。可信发布任务与仓库级不可信池隔离。所有能进入该池的 workflow 都须满足边界，不能靠任务名称或执行后拒绝报告保护秘密。网络和包权限按[跨仓库边界](../ci-architecture.md#runner-与-kiln)验收。

独立 bridge/project 与标准 ACL/防火墙阻断宿主、其他项目、内网及未授权 IPv6。若确需按域名出口或限制 metadata/callback 路径，复用 Squid/nginx 配置，GARM 管理 API 不对 guest 直通。DNS、IPv4/IPv6、重定向和管理面拒绝均待真实验证。[上游 provider](https://github.com/cloudbase/garm-provider-lxd/blob/v0.1.5/README.md) 不自动配置 ACL，这属于部署职责。

## 正常删除与异常停接

正常回收信任 LXD 原 operation 的成功合同，以及上游支持的资源不存在幂等语义。provider 本身会停止实例、删除并等待 operation。接受该合同不表示把 HTTP 202 或一次查询 HTTP 200 当成删除成功，也不表示物理介质擦除。独立 custom 卷与共享镜像有各自生命周期，部署配置不能让它们成为未管理的 job 可写盘。

上游正常删除可针对原唯一实例重试，不要求每次另核 QEMU/cgroup、全部远端回执并签发统一封印。该取舍仅适用于新正常路径；旧 Go/Python 操作的 unknown、不重发、身份和容量合同保持。

| 情形                           | 处理 owner 与行为                                  | 保留的限制                                               |
| ------------------------------ | -------------------------------------------------- | -------------------------------------------------------- |
| job 成功、失败、取消或超时     | GitHub 保存结论；GARM 完成消息、超时及对账推进清理 | 取消回执不等于 job 终态，启用前验证真实失败场景          |
| bootstrap 或创建失败           | GARM 既有状态机处理 error、pending_delete 和清理   | 不叠加阻止上游推进的自有 intent 状态机                   |
| 删除失败或通信中断             | 上游沿原唯一实例按幂等语义恢复；运行侧停接、告警   | 不用 force-delete 忽略 IaaS 错误来伪装回收成功           |
| controller 重启                | GARM 使用持久化状态和启动恢复                      | 不重置数据库或换实例名绕开错误                           |
| LXD 不可用、状态矛盾或疑似孤儿 | 停止新分配，保留对象和日志，进入人工异常处置       | QEMU/cgroup、卷及 operation 的额外取证只在准确授权下使用 |
| 旧现场 unknown                 | 原恢复 owner 单独解释或证明隔离                    | 新路径重试规则不能覆盖旧 Login、连接及资源请求           |

停接优先使用 GARM 标准 disabled 或控制器运维路径。禁止新建与允许现有待删资源继续回收分别验证，不假定 disabled 天然同时满足两者。告警及人工处置链须在启用前演练；不预建新 watchdog 平台，也不承诺网络分区下的固定恢复时限。

原生 CI 按候选、业务覆盖和仓库保护接纳检查。清理异常触发运行侧停接和告警，保留独立基础设施状态，不改写 job 结论，不再为每个 PR 增加自有资源闭合门禁。

## 必要定制与旧模块去向

| 能力                                                       | 目标去向                         | 仍保留的责任                                                 |
| ---------------------------------------------------------- | -------------------------------- | ------------------------------------------------------------ |
| guarded-provider 创建、删除、读取及 active/intent 容量状态 | 从新正常路径退役，源码保全       | GARM worker 和上游 provider 拥有正常生命周期                 |
| 自有 session、观察 token 供应及首次连接状态机              | 从新正常运维退役，保留原记录解释 | 最小凭据、到期及权限边界仍由标准管理配置落实                 |
| 四 unit mask、永久 gate、原 job 封印                       | 暂停 WIP，不列为部署依赖         | 迁移停接使用准确授权的标准运维计划                           |
| capture、closure、records-only 与旧 Python 恢复            | 仅保历史恢复使用                 | 不关闭旧 unknown，不自动继续未实现的恢复框架                 |
| bootstrap、网络与日志                                      | 优先上游模板、配置和镜像配方     | 必要 CA、出口和日志边界仍须验收；旧 Go firewall 不在本片删除 |
| Nucleus workflow 与 Kiln                                   | 保留最小消费适配                 | 业务矩阵、私有包授权及工具兼容仍由消费仓库负责               |
| 验收工具                                                   | 随真实场景交付薄用例和报告       | 不拥有运行状态，也不签发常驻运行许可                         |

当前未证实还需新的常驻自研 runner 管理程序。先完成配置、镜像、消费适配与真实验收。若上游确实不能表达某项必要规则，再为该消费者选择最小代码。下面的 Go 依赖和协议只描述保留程序，不是另一份正常部署路线图。

## 旧运行链与来源

下列路径均相对已保全的 `halo-20261026/` 源码快照。它们是代码分析依据，不表示当前宿主部署版本。版本和来源摘要由本地 `source-manifest.json` 记录。原目录名不承担运行版本选择。

| 实际消费者及调用链                                                                                 | 自有职责                                                          | 现有事实与边界                                                                                          |
| -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| GARM external provider → `guarded-lxd-provider.py` → native LXD provider                           | provider 准入、单 VM 容量、写前意图、实例归属、创建与删除后的核验 | `GARM_*` 环境、stdin、stdout 和退出码构成接口；原生程序响应不是资源终态的唯一证据                       |
| watchdog timer → `provider-watchdog.py` → guard 检查 → GARM ScaleSet                               | 异常后停止接入                                                    | 只定位绑定的 ScaleSet；先读状态再决定禁用，失败时请求停止 controller；root watchdog 不直接改 guard 状态 |
| controller 的 ExecStartPre → guard → `records_only.py`                                             | 正常启动与 records-only 启动资格                                  | maintenance 存在时禁止全部 provider 写入；例外启动绑定原 permit、boot、job、invocation 和 guard 字节    |
| native cloudconfig → `vm-lifecycle-candidate-fa6b5782-20261026/runner-install.tmpl` → guest runner | JIT 凭据安装、服务安装和 bootstrap 完成回执                       | 使用固定 metadata/callback TLS 网关；已含身份的镜像拒绝复用；ready 标记包含实例名和 agent ID            |
| `network.py` 生成的 firewall unit → `network-firewall.py` → nft                                    | 检查并原子替换自有防火墙表                                        | 只删除已存在的 `inet nucleiron` 与 `bridge nucleiron` 表；检查与应用使用同一规则输入，保留其他表        |
| `diagnostics.py` → rsyslog `omfile` 与 logrotate                                                   | TLS 日志采集、过滤与轮换配置                                      | 准确模板没有调用 `guest-log-store.py`；旧 omprog 程序仅是历史材料，不纳入 Go 运行组件                   |
| `vm-first-connection-20261027/first-connection.py` → GARM/GitHub 读取和单次创建                    | 首次 repository / disabled ScaleSet 连接                          | 对齐两端对象与原成功回执；观察到对象缺失不授权重试；导出连接不代表已运行任务                            |
| `vm-session-receipt-recovery-20261027/acquire-session.py` → `sign-app.go` → API                    | 凭据消费证明、有限会话取得和清理                                  | 凭据留在受保护记录；mint、登录和撤销各有原意图；丢回执后保留 unknown，不重新签发                        |
| `vm-restore-guard-pin-20261026/install-services.py` → D-Bus adapter → 文件安装事务                 | 安装服务窗口、停止与恢复                                          | 先订阅再发送；持久化原 job 路径及 JobRemoved 事件；终态后再核 unit、进程和文件前态                      |
| `vm-restore-guard-pin-20261026/lifecycle.py` → GitHub/GARM/LXD/宿主事实                            | 一次 VM 执行的验收与故障恢复                                      | 绑定 run/job/attempt、agent、VM UUID、QEMU 身份及 cgroup；回收成功与任务用例通过是两个结果              |
| staging / collect / diagnosis 入口 → 固定源码、受保护原件和公开输出                                | 程序发布绑定、只读诊断与脱敏                                      | 发布清单不授予执行；公开文件在特权操作前独占创建；历史未知操作仍由原程序解释                            |

这些保全版本中的初始 guard 固定旧 repository URL，与后续 records-only/rebind 版本并不等价。恢复既有程序时，原绑定、policy、源码版本和 unknown 仍由原 owner 核实，不能混用副本。[历史恢复协议](records-recovery.md)保留原窗口的事实所有者与退出条件。未实现的 Go 维护窗口不再列为正常路线的必做项。

网络、镜像和诊断安装脚本还承担配置生成与宿主部署。模板来源与部署选择分开维护，不把现有 `/etc` 配置直接入库。已按来源清单核对日志模板：`guest-logs-server.conf` 的 SHA256 为 `4b93a348815e1079c31eb4c67996d7d5248efdf080ad462d4ef43f0a0541b0ad`。它用 `omfile` 写入 `/var/log/nucleiron/guests/current.log`，安装脚本配套 logrotate。旧测试中的 omprog 路径替换不证明当前存在该消费者。运行部署状态仍以原 owner 的宿主验收为准。

## 已采用的 Go 包

本节记录仓库已有 Go 程序的依赖与协议，版本以 `go.mod`、`go.sum` 为准。它们继续服务现有程序与历史恢复，不是上游正常路径的另一套必装控制面。下列原写前记录、unknown 和不重发规则只约束这些程序，不能用来禁止上游正常删除的幂等恢复。

| 包与上游                                                                                                                  | 承担的能力与消费者                                                  | 自有代码保留的责任                                                                                      |
| ------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| Go 标准库：`os`、`crypto/sha256`、`encoding/json`、`net/http`、`os/exec`、`database/sql`                                  | 文件、摘要、编码、传输、原生 provider 进程及数据库接口              | 输入来源、准确字节、权限、时限、错误处理与秘密脱敏                                                      |
| [`golang.org/x/sys/unix`](https://pkg.go.dev/golang.org/x/sys/unix)                                                       | guard、watchdog 和 bootstrap 的 Linux 文件锁及受保护文件操作        | 锁的生命周期、owner/mode、链接限制与原子持久化；不能以一次 syscall 成功代替完整文件校验                 |
| [`github.com/cloudbase/garm/client`](https://github.com/cloudbase/garm/tree/v0.2.1/client)                                | watchdog、连接与生命周期能力访问 GARM API                           | 绑定 controller/repository/ScaleSet、发送前意图、准确目标读回及 unknown 处置                            |
| [`github.com/cloudbase/garm-provider-common/params`](https://pkg.go.dev/github.com/cloudbase/garm-provider-common/params) | guard 消费 external provider 请求与响应；bootstrap 使用原生模板参数 | 命令白名单、必填字段、身份、容量及准入校验；协议类型不代表已经获得许可                                  |
| [`github.com/canonical/lxd/client`](https://pkg.go.dev/github.com/canonical/lxd/client)                                   | guard 和生命周期能力读取 VM、卷与原 LXD operation                   | 资源归属、原操作终态和释放容量条件；VM 写操作仍调用现有 native provider                                 |
| [`github.com/google/go-github`](https://github.com/google/go-github) 的版本化 `github` 包                                 | 生命周期与连接能力读取 GitHub run、job、runner 和注册状态           | run/attempt/job 对应关系、权限边界与闭合判断                                                            |
| [`github.com/coreos/go-systemd/v22/dbus`](https://pkg.go.dev/github.com/coreos/go-systemd/v22/dbus)                       | guard 依赖单元查询、watchdog 停服与恢复及原服务请求恢复校验         | 只读查询校验属性类型与状态；发送变更时先订阅、持久化原 job，并分别确认 job 终态与 unit/进程/cgroup 状态 |
| [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)，通过 `database/sql` 使用                                   | 原执行闭合诊断读取 GARM SQLite 事实；已接入 v1.61.0 / libc v1.79.0  | 原服务身份、只读连接、WAL 可见性与 schema 绑定；禁止通过直接写库制造完成状态                            |
| [`github.com/BurntSushi/toml`](https://pkg.go.dev/github.com/BurntSushi/toml)，固定 GARM 使用的 v1.6.0                    | 闭合诊断只解码当前 controller 的 database backend/db_file           | 不初始化配置或数据库，不公开秘密配置；当前文件不证明运行连接已加载该内容                                |
| [`github.com/google/nftables`](https://github.com/google/nftables)                                                        | firewall 通过 netlink 构造并原子提交自有表规则                      | 部署输入校验、自有表边界、规则语义、事务结果及未知发送恢复                                              |

基础库有明确的维护与实际依赖来源。[runc 使用 go-systemd](https://github.com/opencontainers/runc/blob/main/go.mod)，[GoReleaser 使用 go-github](https://github.com/goreleaser/goreleaser/blob/main/go.mod)，[Tailscale 使用 nftables](https://github.com/tailscale/tailscale/blob/main/go.mod)。这些是选型依据，不证明本项目已经集成验收。`google/nftables` 位于 Google 组织。README 同时声明它不是 Google 官方产品，两者不冲突。LXD client 随 LXD 维护。GARM client 与 provider-common 随 GARM 生态维护。产品维护不等同于 SDK 独立的长期 API 稳定承诺。

已有 Go 程序在 `go.mod` 和 `go.sum` 固定实际导入版本。GARM client 以当前服务基线 v0.2.1 核对 API。provider-common 以 native provider v0.1.5 使用的 v0.1.9 核对协议。LXD client 绑定部署服务版本及使用的 API extensions。维护这些程序时，新增依赖仍按真实消费者核验，不据原路线图预建能力。`modernc.org/libc` 遵循 sqlite 所要求的配套版本。维护动态从上游发布与安全公告核对，不把本文的来源链接当成可浮动构建依赖。

SDK 接入保留现有超时、代理、重定向、响应上限和精确整数要求。逐项测试缺省、`null` 与错误类型的区别，不让生成类型的零值掩盖非法响应。写操作只由业务 owner 决定是否发送；关闭会重发业务写入的 SDK 或 transport 重试。SDK 返回错误或 context 结束后，仍沿原请求核验结果。依赖升级检查实际调用面、传递依赖、漏洞和协议变化，不自行增加兼容层。

### 防火墙接入 nftables 包

现有 Go firewall 使用 `google/nftables v0.3.0` 和 `mdlayher/netlink`，具体入口与恢复见 [原生防火墙](firewall.md)，不保留 CLI fallback。该包不承担 nft 文本解析；纯配置生产入口输出仅覆盖当前受管规则的 JSON，firewall 校验配置并生成规则。既有配置生成、消费与 unit 合同保留；新目标是否停用该入口须经过等价配置验收。

既有 Go firewall 合同保持端口、地址、域名策略及路由边界。存在性读取完成后，将自有表删除与重建放在一次 netlink batch 中。仅替换 `inet nucleiron` 与 `bridge nucleiron`，其他表保持。配置错误在发送前拒绝；内核拒绝 batch 时原规则保持。发送后超时或回执丢失保留未知结果，先核实际表状态，不重发。

该 Go 入口在独立网络 namespace 验证允许、拒绝及转发行为，覆盖首次加载、替换、非法配置、内核事务拒绝、并发和回执丢失。发送/恢复替身与真实内核检查分别报告。程序先校验配置，再提交一次内核事务，不将检查和应用变成两次 netlink 写入。实际切换固定配置与二进制版本，旧未知操作按原身份处理。

## 已有 Go 程序的协议与故障语义

### ScaleSet 的 provider 池身份

已有 guard 的消费者是 repository ScaleSet。GARM `v0.2.1` 的创建和列表调用都生成 `scaleset_name + "-" + repository_id`，作为 provider 的 `PoolID`。这是 ScaleSet 的派生标识，不是普通 pool 的 UUID。创建调用链见上游 [instance manager](https://github.com/cloudbase/garm/blob/v0.2.1/workers/provider/instance_manager.go)，列表调用链见 [ScaleSet worker](https://github.com/cloudbase/garm/blob/v0.2.1/workers/scaleset/scaleset.go)。保全来源 `provider.py` 按同一规则生成 policy。

guard 的共享 policy 拥有 `controller_id`、`repository_id`、`scaleset_name`、`pool_id` 和 `image_fingerprint`。controller 与 repository 使用小写 UUID；ScaleSet 名称非空且不含环境变量不能承载的 NUL。`pool_id` 必须与名称和 repository ID 的准确拼接一致，镜像使用小写 SHA256。身份字段缺失、null、错误类型或相互不匹配时，watchdog、创建、删除和正常读取均在观察或写入前拒绝。

请求环境、创建正文和 LXD 的 `user.runner-pool-id` 都与这一个 policy 值逐字比较。不裁剪、改名或接受第二种 UUID 池格式，也不据此迁移原记录。正常启动入口仍只承担文件、摘要和 guard 状态检查；完整身份校验由这些实际消费者执行。

测试从上游派生规则独立给出预期字符串，覆盖六个入口的真实格式准入和错绑定拒绝。native 进程参数及 LXD SDK 回归也使用该格式；同名 VM 的替换测试继续使用另一个合法 VM UUID，避免把身份错配测试变成格式错误测试。未知结果、写前意图和容量释放规则保持。

### 其他协议边界

| 边界                   | Go 实现的要求                                                                             | 验收重点                                                                    |
| ---------------------- | ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| GARM external provider | 保留接口 `v0.1.0`、命令白名单、环境绑定及 native 输出；忙时退出 75                        | 合法只读调用、错误身份、超限输入、并发锁、秘密不进入错误输出                |
| guard 状态             | 保留 `version/active/intent/latch` 和 `blocks_delete` 含义；写前持久化，核验后清除 intent | 写前崩溃、发送后超时、回执丢失、残留 VM/卷、bootstrap 失败、维护模式拒写    |
| 单次外部写入           | 同一目标不能靠更换目录或 operation 名重复发送                                             | 并发、超时、非成功响应、结果写失败均保留原请求及 unknown                    |
| systemd                | 订阅先于请求；原 job 的终态和当前 unit 状态分别核验                                       | 事件早于调用返回、丢回执、重启、失败终态、另一个 job、PID 复用和残留 cgroup |
| credentials            | 输入结构、解封字节、签名 claims、有效期和清理含义保持                                     | 不导出私钥；公开诊断不含 token、密码、原始响应或低熵秘密摘要                |
| guest bootstrap        | 固定端点、JIT 模式、runner 身份与 ready 回执保持                                          | 原生模板渲染、部分写入、callback 失败、服务失败、禁止重用已配置 guest       |
| 记录编码               | 按具体记录保留字段、整数含义、时间单位、缺省语义及摘要输入字节                            | Python 排序/转义、Go HTML 转义、数值及换行差异使用固定向量逐项比较          |
| 受保护文件与公开回执   | 保留 owner/mode、链接限制、容量界限、原子持久化和 O_EXCL 防覆盖                           | 符号链接、硬链接、FIFO、目录、同名文件、半写入及目录替换                    |

不能把 Python JSON 解析后重新编码的摘要，默认等同于 Go 编码摘要。协议采用哪一种规范化由原消费者确定；读取历史记录时优先核原字节和原判断证据。Go `int64` 与精确数值解析承担标识符，避免经浮点数中转。

历史恢复不通过批量改写路径或重新生成意图升级。新版本只接管明确完成前态核验的新操作。旧操作仍保留原身份、程序摘要、回执与恢复入口，直至原 owner 确认终态。新实现遇到不支持的记录格式明确拒绝，不猜测升级。

## 迁移顺序与下一片

先独立验证标准路径，再补取消、超时、重启、删除失败及两任务排队场景。随后处理旧域的准确补证、停接和凭据隔离，最后选择生产池切换及原生 CI 消费。每片单独授权和验收，架构选择不授予整条路线的执行权。

推荐首片只有一个可观察结果：在另行批准的可销毁测试域，不接自有 guard/gate，以固定上游版本完成一个 job 的新 VM、固定 Kiln、真实检查和标准删除。必要变更限标准配置、无身份基础镜像/必要 bootstrap、无生产秘密的 probe workflow 和相应用例。优先复用既有合适对象，不默认新建仓库、宿主或长期凭据。

验收核 job/runner/VM 对应、基础镜像与 Kiln digest、私有包有权成功及无权拒绝、guest 无管理凭据或宿主挂载、必要网络拒绝、根盘上限，以及标准删除 operation 成功和实例/根盘消失。这是部署配置的场景验收，不是每个 job 的常驻多平台证明服务。故障和连续排队在后续片补齐后，才选择生产启用。

首片目前仍待选择，不执行 GitHub 连接、凭据签发、VM 或现场操作。它排除旧 unknown 恢复、旧服务/密钥切换、生产接单、扩容及旧代码删除。源码与配置切换另片交付。

新生产池接单前，原恢复 owner 须确认旧写者、队列和凭据不会影响新域。换 controller/project 名称不能隔离同仓库的旧 token 或原 LXD 请求。按准确现场计划保留必要恢复入口、退役能力并保持失败后停接；不自动恢复旧 controller，也不回滚数据库。旧程序继续按[历史恢复合同](records-recovery.md)解释。

已有 Go 程序的构建和检查仍由 [runner README](README.md#构建与检查) 维护，依赖与命令不因本次文档改变。已合入源码、当前安装版本、目标架构和真实运行验收是不同事实。文档发布不安装服务，不切换 external provider，也不证明标准路径已经可用。
