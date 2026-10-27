# Designs

本仓库维护跨仓库设计、[共享 CI 执行器](ci/contract.ts)、[本库文档检查策略](policy/docs.ts)，以及 [runner 自有 Go 实现](runner/README.md)。agents、Nucleus 与 designs 分别维护业务检查，消费同一固定执行器。代码按独立业务结果交付。

## 当前设计

- [CI、审查、准入与运行体系](ci-architecture.md)：跨库架构的唯一正文。当前 CI 片交付 designs 维护的唯一执行器及三库真实消费；Nucleus 限于四项 Docs 检查。runner 的 Python 流程已停止，原操作与恢复现场保留；Go 停接、正常启动、guard watchdog、GARM client、systemd 停止恢复、VM 创建与删除已合入，独立 runner 片已交付 provider 正常只读协议。
- [Rust 与 Halo 共用 runner](ci-architecture.md#runner-与-kiln)：共用基础设施、两层镜像、不可信池边界、生命周期、缓存和接入验收；文档收录不表示已部署。
- [Runner 的 Go 结构与实施切片](runner/architecture.md)：自有代码职责、已采用的 Go 包、协议与恢复边界，以及小 PR 顺序。[单次 VM 创建](runner/create.md)与 [VM 删除](runner/delete.md)保留准确目标、写前记录和未知结果不重发语义，删除确认后才释放 provider 容量。[正常读取](runner/read.md)核验版本与绑定实例，保持 guard 记录。

## 设计与实施

设计正文保留唯一维护位置，后续变更通过 PR 讨论。各文档明确标记草案、已接受或已替代；接受或替代时记录决定的范围、理由和关联版本，不复制整段聊天历史。

文档合入表示收录该版本。架构接受与实施片选择仍以 Human 的明确决定为准，不能从一次文档合入推导出所有阶段的实施授权。

产品仓库继续维护实际生效的业务契约、实现说明和验收入口，并引用对应设计版本。运行中的 CI 和审查不得依赖本仓库的未固定分支内容。

消费仓库的产品源码仍由各仓库维护。实际部署配置、凭据和运行原件不进入本仓库。runner 代码合并与生产切换分别验收。
