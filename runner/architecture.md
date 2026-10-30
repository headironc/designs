# Runner 精简架构与迁移边界

正常路径采用 GARM v0.2.1 与上游 LXD provider v0.1.5。GitHub 原生 job container 消费 Nucleus 维护的固定 CI 镜像 digest。GARM、provider 和 LXD 承担实例生命周期；Nucleus 维护实际消费者和镜像，本仓库维护跨库边界与恢复导航，不再建设第二套正常容量或资源闭合状态机。

这是已采用的架构，标准成功路径已验收，生产 CI 切换尚未完成。[当前消费者与验证边界](../ci-architecture.md#当前源码与实际消费链)区分源码、成功场景和未完矩阵。旧自研运行程序及其专用模板、依赖和检查已从本仓库主线退役；历史 unknown、现场原件及原恢复责任由[历史恢复协议](records-recovery.md)维护。

## 正常目标流与事实所有者

GitHub 接收仓库原生 workflow，负责排队、分配、job 结论、取消及超时。GARM ScaleSet 使用 ephemeral/JIT 为新 runner 创建唯一实例，直接调用上游 provider。LXD 从无实例身份的固定基础镜像创建 VM。VM 内官方 runner 使用原生 job container 拉取固定 Nucleus CI 镜像，执行实际业务检查。

完成消息进入 GARM 的清理路径。provider 停止原实例、请求删除并等待 LXD operation。GARM 处理删除结果后，更新其管理的实例和容量。正常链路不经过自有 guarded-provider，也不生成每个 PR 必须消费的 closed seal。上游行为见 [ScaleSet](https://github.com/cloudbase/garm/blob/v0.2.1/workers/scaleset/scaleset.go)、[完成消息](https://github.com/cloudbase/garm/blob/v0.2.1/workers/scaleset/scaleset_helper.go)、[实例状态机](https://github.com/cloudbase/garm/blob/v0.2.1/workers/provider/instance_manager.go)和 [LXD provider](https://github.com/cloudbase/garm-provider-lxd/blob/v0.1.5/provider/lxd.go)。

| 事实                                | 正常 owner                         | 项目消费边界                                                |
| ----------------------------------- | ---------------------------------- | ----------------------------------------------------------- |
| commit、run/attempt/job、结论       | GitHub Actions 与消费仓库 workflow | 使用原生 checks、artifacts 和 API；guest 自述不授予管理权限 |
| runner、JIT、ScaleSet、数据库状态   | GARM 与上游 worker                 | 使用支持的配置、API/CLI；不直接写库或维护镜像状态库         |
| VM、根盘、operation 与 project 配额 | provider 与 LXD                    | 接受服务成功合同，异常沿原对象调查                          |
| VM 基础镜像与 bootstrap             | 版本化镜像配方、上游模板及必要配置 | 无长期凭据和既有 runner 身份；提供 Docker、CA 与日志环境    |
| CI 容器镜像及业务检查               | Nucleus 镜像维护与原生 CI          | 固定 registry digest，核工具、权限和环境兼容                |
| 旧 unknown 与迁移资格               | 原恢复 owner                       | 保留原件和原判断，不复制成新正常状态机                      |

初始采用一个仓库级 ScaleSet，min-idle=0、max-runners=1。专用 LXD project/profile 设置 VM 数量、CPU、内存和根盘上限，并隔离其他控制器及设备。配额约束 LXD 管理的资源，不独立证明内核孤儿消失。磁盘 quota 也不保证物理空闲；存储池、缓存容量和空闲告警必须另行配置。

## 凭据与不可信执行边界

每个 job 使用新 VM，VM 承担与可信控制面的隔离。guest Docker 只提供任务容器环境。App 私钥、LXD socket、管理 token、宿主目录与发布凭据不进入 guest。guest 只取得本实例的 bootstrap/JIT 和任务所需短期 token。镜像不得带已有 runner 身份。

GitHub App 安装范围、workflow permissions、Secret/Environment 和私有镜像授权使用平台标准配置。可信发布任务与仓库级不可信池隔离。所有能进入该池的 workflow 必须满足边界，不能靠任务名称或执行后拒绝报告保护秘密。网络和包权限按[跨仓库边界](../ci-architecture.md#runner-与-nucleus-ci)验收；guest Docker socket 与宿主管理 socket 分属不同边界。

独立 bridge/project 与标准 ACL/防火墙阻断宿主、其他项目、内网及未授权 IPv6。若确需按域名出口或限制 metadata/callback 路径，复用 Squid/nginx 配置，GARM 管理 API 不对 guest 直通。成功场景已核必要管理面拒绝，DNS、IPv4/IPv6、重定向等完整覆盖仍以部署验收为准。[上游 provider](https://github.com/cloudbase/garm-provider-lxd/blob/v0.1.5/README.md) 不自动配置 ACL，这属于部署职责。

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

停接优先使用 GARM 标准 disabled 或控制器运维路径。禁止新建与允许现有待删资源继续回收分别验证，不假定 disabled 天然同时满足两者。告警及人工处置链必须在启用前演练；不预建新 watchdog 平台，也不承诺网络分区下的固定恢复时限。

原生 CI 按候选、业务覆盖和仓库保护接纳检查。清理异常触发运行侧停接和告警，保留独立基础设施状态，不改写 job 结论，不再为每个 PR 增加自有资源闭合门禁。

## 必要定制

部署维护负责固定上游版本、VM 基础镜像、网络、CA、日志和根盘配置。Nucleus 随首个实际消费者维护专用 JIT 模板、workflow 及容器镜像，入口见[当前源码](../ci-architecture.md#当前源码与实际消费链)。优先使用上游模板、标准配置和 API/CLI；删除旧 Go firewall 源码不证明现场已有等价配置。

正常路径没有自研 guard、watchdog、管理会话或首次连接状态机，也不增加第二套 active/intent 容量库、常驻资源闭合程序或每 PR 的封印。若上游确实不能表达某项必要规则，再为实际消费者选择最小代码。场景验收使用薄用例和原生日志，不拥有运行状态或签发常驻运行许可。

## 迁移顺序与下一片

标准成功路径已完成，后续继续验证失败、取消、超时、bootstrap 失败、专用控制器重启、容量、删除异常及两任务排队场景。各场景保持独立验收，复用输入未变的成功证据；进行中的场景不能计为通过。必要授权沿已选批次执行，架构文档本身不授予现场操作权。

已交付 probe 和 JIT 模板只覆盖其固定输入与场景。[成功范围及证据限制](../ci-architecture.md#镜像发布与已验范围)由跨库消费说明维护。模板字节或消费者输入变化时，先判断受影响覆盖，再补必要现场验收；源码回归不能代替真实 VM 生命周期。

生产启用前核 job/runner/VM 对应、基础镜像与容器 digest、包权限正负例、guest 边界、必要网络拒绝和根盘上限。每个场景分别核 GitHub 终态、GARM 自动回收、原 LXD 删除 operation 和实例/根盘结果。故障恢复、连续排队及实际 CI route 仍须验证，一个手动 platform job 不代表 Packages 或 Halo 已切换。

生产配置切换独立交付，保持 min-idle=0、max-runners=1 的已采用边界。源码发布不启用生产接单、不扩容，也不删除旧材料或结束历史 unknown。

新生产池接单前，原恢复 owner 核对可能影响当前目标的旧写者、队列、凭据及请求，证明已终止或有效隔离。换 controller/project 名称不能隔离同仓库的旧 token 或原 LXD 请求；与当前目标无实际交叉的历史 unknown 继续保全，不要求补造成功。按准确现场计划保留恢复入口并保持失败后停接，不自动恢复旧 controller 或回滚数据库。历史版本与运行材料沿[历史恢复责任](records-recovery.md)追溯，暂停的四 unit WIP 不属于本树交付或后续默认计划。
