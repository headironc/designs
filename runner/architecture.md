# Runner 的 Go 结构与实施切片

本设计将 runner 自有实现改为 Go，并按独立业务结果交付。[专用防火墙重载](README.md)、[guest bootstrap](bootstrap.md)、[provider 异常停接](watchdog.md)、[Go guard 正常启动准入](startup.md)、[guard watchdog 检查与状态持久化](guard-watchdog.md)、[单次 VM 创建](create.md)、[VM 删除与容量释放](delete.md)、[provider 正常只读协议](read.md)、[原执行身份采集](execution.md)、[闭合诊断](closure.md)、[watchdog 暂停](pause.md)和 [gateway 停止](gateway.md)已合入。本片新增 [历史 prepare 原件观察](prepare-evidence.md)，保留历史来源与当前前态缺口，不恢复旧请求。异常停接已接入 GARM client 与 go-systemd，包含原停止 job 恢复。已修正的 [ScaleSet 池身份](#scaleset-的-provider-池身份)保持不变。本文已采用下述 Go 包，依赖随对应消费者接入。现有 Python 快照用于行为对照，不由本片执行。GARM、LXD provider、nginx、rsyslog、Squid 和 dnsproxy 保持各自职责，不重写第三方组件。

跨仓库边界由 [CI 架构](../ci-architecture.md#runner-与-kiln) 维护。designs 拥有 runner 自有程序及发布成品。Kiln 拥有任务镜像，消费仓库拥有工作流和业务检查。新代码在独立工作树准备，原接入现场和未知操作保全。

## 从现有消费者确定职责

下列路径均相对本次保存的 `halo-20261026/` 源码快照。它们是代码分析依据，不表示当前宿主部署版本。版本和来源摘要由本地 `source-manifest.json` 记录。原目录名不承担运行版本选择。

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

初始 guard 与后续 records-only guard 不是等价副本。初始代码仍固定旧 repository URL。rebind 修复源码已有保全版本，但实际新 repository UUID 尚未交付。watchdog 消费显式 policy 输入并以合成身份验证；部署仍依赖真实绑定和原操作终态。[Records-only 恢复协议](records-recovery.md)维护新 Go 窗口的事实所有者、完整退出条件和实施依赖；当前运行能力尚未实现。

网络、镜像和诊断安装脚本还承担配置生成与宿主部署。模板来源与部署选择分开维护，不把现有 `/etc` 配置直接入库。已按来源清单核对日志模板：`guest-logs-server.conf` 的 SHA256 为 `4b93a348815e1079c31eb4c67996d7d5248efdf080ad462d4ef43f0a0541b0ad`。它用 `omfile` 写入 `/var/log/nucleiron/guests/current.log`，安装脚本配套 logrotate。旧测试中的 omprog 路径替换不证明当前存在该消费者。运行部署状态仍以原 owner 的宿主验收为准。

## 目标代码结构

使用一个自有 Go module，按实际进程边界设置入口。下图是目标，不预先创建空包。每个包随第一个真实消费者进入对应 PR。

```text
runner/
  go.mod / go.sum
  cmd/
    runner-firewall/       # owned nftables replacement
    guarded-provider/     # GARM external provider protocol and startup check
    provider-watchdog/     # privileged admission stop
    runner-bootstrap/     # guest JIT setup
    runner-admin/         # explicit connection, inspection and recovery commands
  internal/
    firewall/             # owned rules and atomic nftables transactions
    provider/             # policy, guard state, native invocation and reconciliation
    watchdog/             # admission checks and bound controller stop requests
    bootstrap/            # guest identity and completion protocol
    connection/           # repository and disabled ScaleSet binding
    credentials/          # sealed GARM payload, signing and session lifecycle
    lifecycle/            # VM execution evidence and resource recovery
    services/             # original systemd jobs and service windows
    release/              # verified installation, previous version and recovery
  templates/              # non-secret service and bootstrap templates
  testdata/               # synthetic protocol fixtures used by actual tests
```

入口负责参数、环境变量、文件来源及退出状态。能力包负责值校验、状态转换、外部调用条件和错误处理。外部适配器靠近实际消费者；只有两个消费者确实共享同一协议时才抽取共享包。不建立全局 `utils`、通用任务调度器或可编排任意命令的框架。

操作记录由各业务能力解释。文件独占创建、同步与安全读取可在首次复用时共享，但 `unknown` 的判断与恢复条件仍属于原业务。一次操作的输入、写意图、外部回执和结果不拆成彼此独立的基础设施 PR。

GARM 仍负责 controller 与其数据库。自有代码通过已采用 API 和受限只读核验观察它，不新增直接数据库写入。原 Go signer 使用 GARM 的解封库和准确 JWT 协议。迁移时保留实际依赖与测试，不重新实现加密格式。

## 已采用的 Go 包

以下选型覆盖整个 runner，均为已接受的目标依赖。正常启动已接入 `x/sys`，guard watchdog 已接入 LXD client 和 go-systemd 的只读单元查询。异常停接已接入 GARM client 与 go-systemd，包含原停止 job 恢复。创建和删除已采用 provider-common 与 LXD client；正常读取复用这些包及 GARM 的状态校验。原执行采集直接消费 go-github v84.0.0。已合入的闭合诊断已接入 modernc SQLite 和固定 GARM 已采用的 BurntSushi TOML，分别读取同事务快照和当前数据库路径。watchdog 暂停与 gateway 停止已复用 GARM client、go-systemd 和现有文件保护；本片历史 prepare 观察复用标准库及文件保护，不新增依赖。其余包随首个真实消费者进入独立实施片。已有实现也按表中目标迁移，不建立双版本兼容路径。

| 包与上游                                                                                                                  | 承担的能力与消费者                                                  | 自有代码保留的责任                                                                                      |
| ------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| Go 标准库：`os`、`crypto/sha256`、`encoding/json`、`net/http`、`os/exec`、`database/sql`                                  | 文件、摘要、编码、传输、原生 provider 进程及数据库接口              | 输入来源、准确字节、权限、时限、错误处理与秘密脱敏                                                      |
| [`golang.org/x/sys/unix`](https://pkg.go.dev/golang.org/x/sys/unix)                                                       | guard、watchdog 和 bootstrap 的 Linux 文件锁及受保护文件操作        | 锁的生命周期、owner/mode、链接限制与原子持久化；不能以一次 syscall 成功代替完整文件校验                 |
| [`github.com/cloudbase/garm/client`](https://github.com/cloudbase/garm/tree/v0.2.1/client)                                | watchdog、连接与生命周期能力访问 GARM API                           | 绑定 controller/repository/ScaleSet、发送前意图、准确目标读回及 unknown 处置                            |
| [`github.com/cloudbase/garm-provider-common/params`](https://pkg.go.dev/github.com/cloudbase/garm-provider-common/params) | guard 消费 external provider 请求与响应；bootstrap 使用原生模板参数 | 命令白名单、必填字段、身份、容量及准入校验；协议类型不代表已经获得许可                                  |
| [`github.com/canonical/lxd/client`](https://pkg.go.dev/github.com/canonical/lxd/client)                                   | guard 和生命周期能力读取 VM、卷与原 LXD operation                   | 资源归属、原操作终态和释放容量条件；VM 写操作仍调用现有 native provider                                 |
| [`github.com/google/go-github`](https://github.com/google/go-github) 的版本化 `github` 包                                 | 生命周期与连接能力读取 GitHub run、job、runner 和注册状态           | run/attempt/job 对应关系、权限边界与闭合判断                                                            |
| [`github.com/coreos/go-systemd/v22/dbus`](https://pkg.go.dev/github.com/coreos/go-systemd/v22/dbus)                       | guard 依赖单元查询、watchdog 停服与恢复；后续服务安装窗口及恢复校验 | 只读查询校验属性类型与状态；发送变更时先订阅、持久化原 job，并分别确认 job 终态与 unit/进程/cgroup 状态 |
| [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)，通过 `database/sql` 使用                                   | 原执行闭合诊断读取 GARM SQLite 事实；已接入 v1.61.0 / libc v1.79.0  | 原服务身份、只读连接、WAL 可见性与 schema 绑定；禁止通过直接写库制造完成状态                            |
| [`github.com/BurntSushi/toml`](https://pkg.go.dev/github.com/BurntSushi/toml)，固定 GARM 使用的 v1.6.0                    | 闭合诊断只解码当前 controller 的 database backend/db_file           | 不初始化配置或数据库，不公开秘密配置；当前文件不证明运行连接已加载该内容                                |
| [`github.com/google/nftables`](https://github.com/google/nftables)                                                        | firewall 通过 netlink 构造并原子提交自有表规则                      | 部署输入校验、自有表边界、规则语义、事务结果及未知发送恢复                                              |

基础库有明确的维护与实际依赖来源。[runc 使用 go-systemd](https://github.com/opencontainers/runc/blob/main/go.mod)，[GoReleaser 使用 go-github](https://github.com/goreleaser/goreleaser/blob/main/go.mod)，[Tailscale 使用 nftables](https://github.com/tailscale/tailscale/blob/main/go.mod)。这些是选型依据，不证明本项目已经集成验收。`google/nftables` 位于 Google 组织。README 同时声明它不是 Google 官方产品，两者不冲突。LXD client 随 LXD 维护。GARM client 与 provider-common 随 GARM 生态维护。产品维护不等同于 SDK 独立的长期 API 稳定承诺。

实施片在 `go.mod` 和 `go.sum` 固定实际导入版本。GARM client 以当前服务基线 v0.2.1 核对 API。provider-common 以 native provider v0.1.5 使用的 v0.1.9 核对协议。LXD client 绑定部署服务版本及使用的 API extensions。其余包在接入时选用核验过的稳定发布；没有适用发布标签时固定核验提交。`modernc.org/libc` 遵循 sqlite 所要求的配套版本。维护动态从上游发布与安全公告核对，不把本文的来源链接当成可浮动构建依赖。

SDK 接入保留现有超时、代理、重定向、响应上限和精确整数要求。逐项测试缺省、`null` 与错误类型的区别，不让生成类型的零值掩盖非法响应。写操作只由业务 owner 决定是否发送；关闭会重发业务写入的 SDK 或 transport 重试。SDK 返回错误或 context 结束后，仍沿原请求核验结果。依赖升级检查实际调用面、传递依赖、漏洞和协议变化，不自行增加兼容层。

### 防火墙接入 nftables 包

已合入的 firewall 读取 nft 文本并调用 CLI，具体行为见 [当前实现](README.md)。目标实现改用 `google/nftables`，不保留 CLI fallback。该包不承担 nft 文本解析；部署能力提供仅覆盖当前受管规则的结构化配置，firewall 校验配置并生成规则。配置生成、消费、unit 绑定与切换说明在同一实施片交付，不先建设通用规则语言或解析框架。

迁移片保持现有端口、地址、域名对应的网络策略及路由边界。存在性读取完成后，将自有表删除与重建放在一次 netlink batch 中。仅替换 `inet nucleiron` 与 `bridge nucleiron`，其他表保持。配置错误在发送前拒绝；内核拒绝 batch 时原规则保持。发送后超时或回执丢失保留未知结果，先核实际表状态，不重发。

验收在独立网络 namespace 比较原规则与新规则的实际允许、拒绝及转发行为。覆盖首次加载、替换、非法配置、内核事务拒绝、并发及回执丢失。发送和恢复路径的替身检查与真实内核原子性检查分别报告。新实现先校验配置，再提交一次内核事务。原 CLI 的检查与应用流程不变成两次 netlink 写入。实际切换固定配置与二进制版本，旧未知操作按原身份处理。

## 保持的协议与故障语义

### ScaleSet 的 provider 池身份

当前消费者是 repository ScaleSet。GARM `v0.2.1` 的创建和列表调用都生成 `scaleset_name + "-" + repository_id`，作为 provider 的 `PoolID`。这是 ScaleSet 的派生标识，不是普通 pool 的 UUID。创建调用链见上游 [instance manager](https://github.com/cloudbase/garm/blob/v0.2.1/workers/provider/instance_manager.go)，列表调用链见 [ScaleSet worker](https://github.com/cloudbase/garm/blob/v0.2.1/workers/scaleset/scaleset.go)。保全来源 `provider.py` 按同一规则生成 policy。

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

## 小 PR 的交付顺序

每个实现 PR 交付一个可执行、可检查的结果及其必要失败恢复。每片合入前独立审阅；依赖未合入时使用明确依赖的分支，不能把后续切片累积成一个 PR。表中后续项是顺序建议，不授权连续实现全部路线图。

| 切片                                                 | 可独立验收的结果                                                        | 包含内容与明确排除                                                                                                                    |
| ---------------------------------------------------- | ----------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| 结构说明                                             | 审阅者能定位职责、协议和实施顺序                                        | 本文及必要导航；不带历史源码大包，不带运行实现                                                                                        |
| 专用防火墙重载（已合入）                             | systemd 可消费 Go 入口，检查并原子替换自有 nftables 表                  | 实际入口、规则读取、nft 调用、unit 消费绑定、构建与失败测试；不改端口、域名、路由、其他表或生产服务                                   |
| guest bootstrap（已合入）                            | 原生 provider 渲染并启动 Go guest 程序，产出相同 ready 回执             | JIT 安装、失败反馈、guest 成品与模板绑定；不启用 ScaleSet，不改 controller 凭据                                                       |
| provider 异常停接（已合入）                          | Go watchdog 消费 guard 检查接口，异常时停止准确目标的接入               | 身份核验、禁用及读回、未知请求记录、失败停止请求；不迁移 guard 或 records-only，不启用服务                                            |
| Go guard 正常启动准入（已合入）                      | controller 正常启动前核准配置、程序摘要与 guard 状态                    | 标准库与 x/sys 文件保护、并发锁、intent/latch 拒绝；active 保留容量，所有维护模式启动拒绝；不含 VM 写操作和 records-only              |
| Go guard watchdog（已合入）                          | 现有 Go watchdog 消费完整健康观察及持久化结果                           | LXD client、systemd 状态查询、挂载与储备、ready/latch 转换、原子写入和中断拒绝；不清除旧操作，不含 VM 写操作和部署                    |
| Go guard 单次 VM 创建（已合入）                      | 准入后只调用一次 native 创建，准确核验后保存 active                     | provider-common 协议、LXD client 核验、写前意图、原进程结束及未知拒重发；不含删除、释放容量或部署                                     |
| Go guard VM 删除与容量释放（已合入）                 | 删除准确绑定实例，原 native 结束和 LXD 核验后才释放 provider 容量       | bootstrap 完成、写前记录、实例/可写卷及 operation 核验；未知拒重发，保留 latch，不代替真实 job 完整生命周期验收                       |
| Go guard 正常只读协议（已合入）                      | GARM 查询 native 版本或准确绑定的实例，guard 记录保持不变               | GetVersion、GetInstance、ListInstances，身份与响应核验；不含 records-only、清记录或部署                                               |
| ScaleSet 池身份修正（已合入）                        | 实际 ScaleSet 请求按原派生身份通过完整 guard 调用链                     | 共享 policy 校验、实际协议格式回归及错绑定拒绝；不增加普通 pool 模式，不改记录或部署                                                  |
| 原执行身份采集（已合入）                             | [正在运行的准确 job](execution.md) 具有平台和宿主核对后的独占原身份回执 | GitHub/GARM/LXD/进程/卷身份、guard 永久锁、受保护原件和原结果回读；不含清理、尚未分配执行或部署                                       |
| [原执行闭合诊断](closure.md)（已合入）               | 操作者从新 Go 原执行记录取得准确闭合事实及缺口                          | 实际诊断入口消费 GitHub/GARM/LXD/宿主、服务及 SQLite 只读观察；保留原操作与凭据证据缺口，不签许可、不清记录、不释放容量               |
| [原执行绑定的 watchdog 暂停](pause.md)（已合入）     | 准确终结 job 的原执行取得唯一 timer job 结果与原 worker 排空事实        | 全局 live 准入、固定独立状态 slot、原事件恢复及只读诊断消费；不停止 worker，不恢复 timer，不含 controller/gateway/凭据/许可/清理/部署 |
| [原执行绑定的 gateway 停止](gateway.md)（已合入）    | 已确认原 watchdog 暂停的执行取得唯一 gateway job 与原进程排空事实       | 共享原 pause 持锁消费、operation 互斥、固定 gateway slot、停止传播检查及原事件恢复；不停止 controller，不处理凭据、许可、清理或部署   |
| [历史 prepare 原件观察](prepare-evidence.md)（本片） | 取得准确五件的内部一致性、原字节摘要和 staging 观察                     | 固定只读路径与原锁，不恢复原请求；来源、外部 preflight、凭据和当前安装前态缺口保持                                                    |
| [records-only 维护窗口](records-recovery.md)         | 在准确许可下清理原 GARM 遗留记录，并回到停止且停接的状态                | 签发、原 job 启动准入、维护读取、清理、停止、guard 恢复和许可退役；不迁用旧许可，不含正常服务恢复或启用接单                           |
| watchdog GARM 客户端接入（已合入）                   | 原停接流程通过 GARM client 完成同一准确目标禁用与读回                   | 保留身份、输入、时限、未知登录和禁用请求边界；不同时改造 systemd 停止恢复                                                             |
| watchdog systemd 停止与恢复（已合入）                | 一次停止请求具有可追踪的原 job 与终态核验                               | go-systemd 订阅、写前记录、原 job 回执及 unit/进程/cgroup 核验；丢失终态不补造成功，旧 CLI 记录保全拒绝自动迁移，不重新接入或部署     |
| nftables 包接入                                      | 结构化部署配置经 Go 包原子替换自有表                                    | 配置生成与校验、netlink、unit 消费和隔离验收；不改变网络策略，不保留 CLI fallback                                                     |
| 服务与版本安装窗口                                   | 在准确前态中停止服务、安装一个版本，并恢复或保留可恢复状态              | systemd 原 job、文件事务、回滚与配置漂移检查；不将通用文件复制单独提前交付                                                            |
| 凭据消费与连接                                       | 一次已绑定操作建立并验证 disabled 连接，故障后保留可解释结果            | signer、凭据证明、受保护记录、repository/ScaleSet 回执及只读恢复；稳定 rebind 到达后再确定最小闭包                                    |
| VM 终态与恢复                                        | 对一个准确 job 区分用例通过和仅资源回收，并验证资源归零                 | GitHub/GARM/LXD/进程/cgroup 证据和必要服务恢复；四种任务终态按实际接口验证，不冒充生产全验收                                          |
| 运行版本切换                                         | 自托管消费已合入的准确 Go 版本，并通过真实验收                          | 停接、旧操作处理、成品校验、切换、回退和读回；不与纯文档合并绑定，不在本地重构中执行                                                  |

每次开始后续片前，依据已经到位的消费者重新检查边界。若某一行包含两个可独立验收结果，再拆小。同一恢复不变量跨层时保留在一片，确保中间版本也能安全运行。

已合入组件的具体行为与检查分别由 [firewall](README.md)、[bootstrap](bootstrap.md)、[watchdog](watchdog.md) 和 guard 各入口文档维护。采集和原结果回读由 [execution](execution.md) 维护，诊断由 [closure](closure.md) 维护，watchdog 暂停由 [pause](pause.md) 维护，gateway 停止由 [gateway](gateway.md) 维护，本片由 [prepare 原件观察](prepare-evidence.md) 维护，其余片再按真实依赖选择。日志采集继续消费 rsyslog 与 logrotate，不加入无消费者的 Go 日志程序。

## 构建、验证与后续切换

新自有程序使用 Go 工具链直接构建、测试。`go.mod` 与[构建说明](README.md)固定当前工具链和依赖版本。当前已接入 `x/sys`、GARM client、provider-common、LXD client、go-github、go-systemd、modernc SQLite 和 BurntSushi TOML。其余目标依赖随上表对应的真实消费者接入，新构建入口不依赖 Python。先联网取得并核验固定依赖，再在已准备的缓存中关闭网络下载进行构建。Linux 系统行为在独立 Linux 测试目录验证。

第三方源码归档仍使用已核版本和摘要。自有 Go module 不直接合并两份第三方 vendor 树。需要的库按实际 import 固定版本，并验证与原协议相符。发布包标注源码提交、依赖、目标平台与各成品摘要。构建记录证明来源，不代替运行许可。

每片至少验证受影响的业务边界、真实 CLI 协议和失败路径，再检查差异与秘密扫描。服务、挂载、LXD 和远端 API 的替身结果只证明离线行为。真实宿主或 VM 验收单独绑定准确版本与操作。未变输入的旧检查继续复用，不为拆 PR 重复全部运行。

后续切换消费本次实现合入 `main` 的准确提交。切换前先停止接入，核旧任务、unknown 操作和可回退成品。安装后核服务实际加载版本与真实任务结果。旧任务未结清时不通过更名或迁移记录强行切换。本片修改源码中的消费入口，不修改宿主已安装的程序或服务。

## 交付范围

结构说明随实现保持更新，运行实现按独立业务结果交付。108 份本地历史源码及既有构建检查保全，不整体进入实现 PR。本片交付历史 prepare 原件的只读观察；controller、其他原操作/凭据来源、records-only 和运行切换仍待后续实施。运行版本以稳定交付为准，不拼接修复中的文件。
