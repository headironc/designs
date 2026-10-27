# Runner 的 Go 结构与实施切片

本设计将 runner 自有实现改为 Go，并按独立业务结果交付。[专用防火墙重载](README.md)已合入，当前实现片是 [guest bootstrap](bootstrap.md)。现有 Python 快照用于行为对照和原操作恢复。GARM、LXD provider、nginx、rsyslog、Squid 和 dnsproxy 保持各自职责，不重写第三方组件。

跨仓库边界由 [CI 架构](../ci-architecture.md#runner-与-kiln) 维护。designs 拥有 runner 自有程序及发布成品。Kiln 拥有任务镜像，消费仓库拥有工作流和业务检查。新代码在独立工作树准备，原接入现场和未知操作保全。

## 从现有消费者确定职责

下列路径均相对本次保存的 `halo-20261026/` 源码快照。它们是代码分析依据，不表示当前宿主部署版本。版本和来源摘要由本地 `source-manifest.json` 记录。原目录名不承担运行版本选择。

| 实际消费者及调用链 | 自有职责 | 现有事实与边界 |
| --- | --- | --- |
| GARM external provider → `guarded-lxd-provider.py` → native LXD provider | provider 准入、单 VM 容量、写前意图、实例归属、创建与删除后的核验 | `GARM_*` 环境、stdin、stdout 和退出码构成接口；原生程序响应不是资源终态的唯一证据 |
| watchdog timer → `provider-watchdog.py` → guard 检查 → GARM ScaleSet | 异常后停止接入 | 只定位绑定的 ScaleSet；先读状态再决定禁用，失败时请求停止 controller；root watchdog 不直接改 guard 状态 |
| controller 的 ExecStartPre → guard → `records_only.py` | 正常启动与 records-only 启动资格 | maintenance 存在时禁止全部 provider 写入；例外启动绑定原 permit、boot、job、invocation 和 guard 字节 |
| native cloudconfig → `vm-lifecycle-candidate-fa6b5782-20261026/runner-install.tmpl` → guest runner | JIT 凭据安装、服务安装和 bootstrap 完成回执 | 使用固定 metadata/callback TLS 网关；已含身份的镜像拒绝复用；ready 标记包含实例名和 agent ID |
| `network.py` 生成的 firewall unit → `network-firewall.py` → nft | 检查并原子替换自有防火墙表 | 只删除已存在的 `inet nucleiron` 与 `bridge nucleiron` 表；检查与应用使用同一规则输入，保留其他表 |
| `diagnostics.py` → rsyslog `omfile` 与 logrotate | TLS 日志采集、过滤与轮换配置 | 准确模板没有调用 `guest-log-store.py`；旧 omprog 程序仅是历史材料，不纳入 Go 运行组件 |
| `vm-first-connection-20261027/first-connection.py` → GARM/GitHub 读取和单次创建 | 首次 repository / disabled ScaleSet 连接 | 对齐两端对象与原成功回执；观察到对象缺失不授权重试；导出连接不代表已运行任务 |
| `vm-session-receipt-recovery-20261027/acquire-session.py` → `sign-app.go` → API | 凭据消费证明、有限会话取得和清理 | 凭据留在受保护记录；mint、登录和撤销各有原意图；丢回执后保留 unknown，不重新签发 |
| `vm-restore-guard-pin-20261026/install-services.py` → D-Bus adapter → 文件安装事务 | 安装服务窗口、停止与恢复 | 先订阅再发送；持久化原 job 路径及 JobRemoved 事件；终态后再核 unit、进程和文件前态 |
| `vm-restore-guard-pin-20261026/lifecycle.py` → GitHub/GARM/LXD/宿主事实 | 一次 VM 执行的验收与故障恢复 | 绑定 run/job/attempt、agent、VM UUID、QEMU 身份及 cgroup；回收成功与任务用例通过是两个结果 |
| staging / collect / diagnosis 入口 → 固定源码、受保护原件和公开输出 | 程序发布绑定、只读诊断与脱敏 | 发布清单不授予执行；公开文件在特权操作前独占创建；历史未知操作仍由原程序解释 |

初始 guard 与后续 records-only guard 不是等价副本。初始代码还固定旧 repository URL，后续 rebind 将改变准确消费绑定。以接入 owner 交付的稳定版本补齐新 provider 的对照。最终仓库绑定以该版本为准。

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
    firewall/             # owned tables, checked rules and atomic application
    provider/             # policy, guard state, native invocation and reconciliation
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

## 保持的协议与故障语义

| 边界 | Go 实现的要求 | 验收重点 |
| --- | --- | --- |
| GARM external provider | 保留接口 `v0.1.0`、命令白名单、环境绑定及 native 输出；忙时退出 75 | 合法只读调用、错误身份、超限输入、并发锁、秘密不进入错误输出 |
| guard 状态 | 保留 `version/active/intent/latch` 和 `blocks_delete` 含义；写前持久化，核验后清除 intent | 写前崩溃、发送后超时、回执丢失、残留 VM/卷、bootstrap 失败、维护模式拒写 |
| 单次外部写入 | 同一目标不能靠更换目录或 operation 名重复发送 | 并发、超时、非成功响应、结果写失败均保留原请求及 unknown |
| systemd | 订阅先于请求；原 job 的终态和当前 unit 状态分别核验 | 事件早于调用返回、丢回执、重启、失败终态、另一个 job、PID 复用和残留 cgroup |
| credentials | 输入结构、解封字节、签名 claims、有效期和清理含义保持 | 不导出私钥；公开诊断不含 token、密码、原始响应或低熵秘密摘要 |
| guest bootstrap | 固定端点、JIT 模式、runner 身份与 ready 回执保持 | 原生模板渲染、部分写入、callback 失败、服务失败、禁止重用已配置 guest |
| 记录编码 | 按具体记录保留字段、整数含义、时间单位、缺省语义及摘要输入字节 | Python 排序/转义、Go HTML 转义、数值及换行差异使用固定向量逐项比较 |
| 受保护文件与公开回执 | 保留 owner/mode、链接限制、容量界限、原子持久化和 O_EXCL 防覆盖 | 符号链接、硬链接、FIFO、目录、同名文件、半写入及目录替换 |

不能把 Python JSON 解析后重新编码的摘要，默认等同于 Go 编码摘要。协议采用哪一种规范化由原消费者确定；读取历史记录时优先核原字节和原判断证据。Go `int64` 与精确数值解析承担标识符，避免经浮点数中转。

历史恢复不通过批量改写路径或重新生成意图升级。新版本只接管明确完成前态核验的新操作。旧操作仍保留原身份、程序摘要、回执与恢复入口，直至原 owner 确认终态。新实现遇到不支持的记录格式明确拒绝，不猜测升级。

## 小 PR 的交付顺序

每个实现 PR 交付一个可执行、可检查的结果及其必要失败恢复。每片合入前独立审阅；依赖未合入时使用明确依赖的分支，不能把后续切片累积成一个 PR。表中后续项是顺序建议，不授权连续实现全部路线图。

| 切片 | 可独立验收的结果 | 包含内容与明确排除 |
| --- | --- | --- |
| 结构说明 | 审阅者能定位职责、协议和实施顺序 | 本文及必要导航；不带历史源码大包，不带运行实现 |
| 专用防火墙重载（已合入） | systemd 可消费 Go 入口，检查并原子替换自有 nftables 表 | 实际入口、规则读取、nft 调用、unit 消费绑定、构建与失败测试；不改端口、域名、路由、其他表或生产服务 |
| guest bootstrap（当前实现片） | 原生 provider 渲染并启动 Go guest 程序，产出相同 ready 回执 | JIT 安装、失败反馈、guest 成品与模板绑定；不启用 ScaleSet，不改 controller 凭据 |
| provider 准入与停止接入 | GARM 经 Go guard 执行受限 native 操作，异常时停止接入 | guard、watchdog、records-only 启动校验、原子状态及必要恢复兼容；不创建新 repository 或改变审批范围 |
| 服务与版本安装窗口 | 在准确前态中停止服务、安装一个版本，并恢复或保留可恢复状态 | systemd 原 job、文件事务、回滚与配置漂移检查；不将通用文件复制单独提前交付 |
| 凭据消费与连接 | 一次已绑定操作建立并验证 disabled 连接，故障后保留可解释结果 | signer、凭据证明、受保护记录、repository/ScaleSet 回执及只读恢复；稳定 rebind 到达后再确定最小闭包 |
| VM 终态与恢复 | 对一个准确 job 区分用例通过和仅资源回收，并验证资源归零 | GitHub/GARM/LXD/进程/cgroup 证据和必要服务恢复；四种任务终态按实际接口验证，不冒充生产全验收 |
| 运行版本切换 | 自托管消费已合入的准确 Go 版本，并通过真实验收 | 停接、旧操作处理、成品校验、切换、回退和读回；不与纯文档合并绑定，不在本地重构中执行 |

每次开始后续片前，依据已经到位的消费者重新检查边界。若某一行包含两个可独立验收结果，再拆小。同一恢复不变量跨层时保留在一片，确保中间版本也能安全运行。

已选的首个实现片是专用防火墙重载。原 `network.py` 为 unit 的 `ExecStart` 和 `ExecReload` 设置了 Python 入口。新片交付 Go 可执行程序及对应 unit 模板，规则内容保持外部输入。检查通过后使用同一份规则应用；失败返回非零，不自动重试。

首片验证自有表全部存在、部分存在及均不存在，且其他表保留。失败路径包括规则读取、列表读取或解析、检查与应用失败。检查失败时没有应用；应用失败不报告成功且不重发。用受控 nft 替身测试调用边界，再在隔离 namespace 核实际原子替换。生产加载状态留到切换验收，当前不安装 unit 或修改防火墙。日志采集继续消费 rsyslog 与 logrotate，不加入无消费者的 Go 日志程序。

## 构建、验证与后续切换

新自有程序使用 Go 工具链直接构建、测试。`go.mod` 与[构建说明](README.md)固定当前工具链版本及来源。本片只依赖标准库，新构建入口不依赖 Python。Linux 系统行为在独立 Linux 测试目录验证。

第三方源码归档仍使用已核版本和摘要。自有 Go module 不直接合并两份第三方 vendor 树。需要的库按实际 import 固定版本，并验证与原协议相符。发布包标注源码提交、依赖、目标平台与各成品摘要。构建记录证明来源，不代替运行许可。

每片至少验证受影响的业务边界、真实 CLI 协议和失败路径，再检查差异与秘密扫描。服务、挂载、LXD 和远端 API 的替身结果只证明离线行为。真实宿主或 VM 验收单独绑定准确版本与操作。未变输入的旧检查继续复用，不为拆 PR 重复全部运行。

后续切换消费本次实现合入 `main` 的准确提交。切换前先停止接入，核旧任务、unknown 操作和可回退成品。安装后核服务实际加载版本与真实任务结果。旧任务未结清时不通过更名或迁移记录强行切换。当前重构不改运行入口。

## 交付范围

结构说明随实现保持更新，运行实现按独立业务结果交付。108 份本地历史源码及既有构建检查保全，不整体进入实现 PR。当前交付 guest bootstrap，后续能力仍待选择。运行版本以稳定交付为准，不拼接修复中的文件。
