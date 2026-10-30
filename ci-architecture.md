# CI 与运行边界

## 当前选择

agents、Nucleus 和 designs 各自维护仓库原生 CI、独立审查及分支保护，执行使用标准 GitHub hosted runner。发布与部署使用独立入口。自托管 CI 建设、故障矩阵和 VM 生产切换路线已取消。

旧共享执行器 S、冻结配方 P、报告认证、集合准入和自动合并控制器已退役，不作为当前 CI 的依赖或兼容目标。各仓库以自身实际 workflow、业务检查和平台保护为准，源码清理不代替运行验收。

## 开发与交付

代码、测试、依赖和 workflow 可以在同一 PR 中修改。每次执行绑定准确提交，工具和依赖使用项目的版本及锁文件。独立审查同时判断业务代码、测试覆盖和 CI 配置变化，不能只凭检查绿色接纳已知缺陷。

经过受保护流程和独立审查接纳的 CI 配置成为后续运行基线。项目不要求依赖清单与旧版本逐字一致，也不要求每次升级先发布特权控制器或兼容配方。

普通错误由检查发现，检查机制的变化由独立审查判断，敏感凭据由权限隔离保护。执行来源可信只说明该次执行的事实，不证明业务覆盖充分。测试删除、断言弱化和空检查仍属于审查责任。

三库默认独立维护 workflow、选检、依赖准备、结果解释与发布入口。每库应能在自身 PR 中理解、修改、升级和回滚 CI，不要求同步升级其他仓库或先发布共同执行器。

共享 CI 不列为后续预定阶段。当前项目的语言、业务验收和运行环境不同，统一执行器增加了跨库发布、协议兼容和故障协调成本，现有证据不足以证明它降低了总维护成本。少量相似 YAML 和命令允许分别维护。

小型工具复用只在存在相同责任、真实消费者和可说明的维护收益时考虑。消费者独立选择版本，工具失败不要求其他仓库同步切换；若仍需共同发布或协调恢复，就重新判断复用是否值得。工具复用不拥有项目检查义务或最终审查判断。

## 发布与外部操作

普通检查不持发布凭据或管理权限。发布入口固定准确产物和目标，执行后读取实际结果。发布结果未知时沿原对象确认，不能用重新检查或重复发布代替恢复。

版本固定以单次执行和实际发布为单位，不将整包长期冻结作为普遍要求。操作日志、凭据和生产配置不进入源码。源码合入不自动部署、扩容、激活 runner 或改变仓库与包的可见性。

## 删除与恢复边界

旧 CI 的共享执行器、固定配方、自动入口和专属依赖已退役。旧源码仍可从 Git 历史读取，外部运行原件、原操作身份及未知结果继续保留。旧操作由其原恢复责任处理，不改写成新流程成功，也不因源码删除而重复发送。

自研与上游自托管试验的历史版本、已验范围和 unknown 由[历史恢复协议](runner/records-recovery.md)维护。已取消路线的探针、启动模板及专属检查不再作为当前 CI 依赖。必要现场安全收口由原恢复 owner 按原对象和有效授权完成，不要求先补齐旧矩阵才能使用 hosted CI。

## Nucleus CI 与依赖镜像

Nucleus 拥有真实业务检查、选检、依赖镜像和发布协议。以下入口固定到已合并的 `8d18e521b3ec6be675603939335c77bdb33bbbc2`，用于核对标准业务消费者；该版本中自托管试验入口不属于当前采用路线。后续检查范围与运行要求以 Nucleus 的[验证合同](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/docs/engineering/verification.md)为准。

| 消费者                                                                                                                                                                                                                                                                                                                                                                                         | 行为与责任                                                                            |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| [Docs](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/docs.yml)、[Maintenance](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/maintenance-tools.yml)、[Rust](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/rust-checks.yml) | 标准 hosted `ubuntu-24.04`；保留各自检查与结果解释                                    |
| [Packages](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/packages.yml)                                                                                                                                                                                                                                                                  | 标准 hosted `ubuntu-24.04`；包含选检、PG producer、包矩阵及汇总                       |
| [OCI 构建与独立下载](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/halo-image.yml)                                                                                                                                                                                                                                                      | 标准 hosted `ubuntu-24.04`；构建、冻结、恢复验收及独立下载校验真实产物                |
| [镜像发布与冷拉取](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/halo-publish.yml)及[专用检查](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/workflows/halo-publish-checks.yml)                                                                                                            | 标准 hosted `ubuntu-24.04`；发布权限、来源校验、原结果恢复及冷消费                    |
| [Halo 本地容器验收](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/tools/halo-local.ts)                                                                                                                                                                                                                                                                    | 以匹配当前输入的镜像运行完整网站验收；保留工作区、私有数据库、nginx、TLS 与浏览器要求 |

[输入分类器](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/tools/check-inputs.ts)与[包矩阵入口](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/tools/native-packages.ts)拥有候选、选检和汇总规则。未知输入保守全检，所选 package、shard 和 group 保持完整。自托管路线取消不减少这些检查，也不改变 required checks 或平台保护。

### 镜像归属与实际消费

Nucleus 的[镜像配方](https://github.com/headironc/nucleus/tree/8d18e521b3ec6be675603939335c77bdb33bbbc2/.github/halo-ci)与[镜像合同](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/halo/README.md#ci-依赖镜像)拥有 Rust、Bun、Node、PostgreSQL、浏览器和 nginx 的工具兼容性。镜像有 hosted OCI 构建、独立下载、发布、冷拉取及本地消费者，继续随真实消费者维护。独立 Kiln 仓库不再是必需发布者；其历史材料仍按原恢复责任保留。

容器镜像封装依赖，GitHub hosted runner 提供 CI 执行环境。保留 Nucleus 镜像不要求建设自托管 VM 基础镜像、GARM ScaleSet 或资源闭合平台。普通包检查、镜像工具验收和 Halo 完整网站验收各有覆盖范围；薄消费者或镜像构建成功不替代产品验收。

[数据库夹具](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/fixture/src/lib.rs)按准确版本准备隔离集群、证书和临时目录。共用安装镜像不表示共用数据目录或长期数据库服务。升级和回退均核工具、运行用户、输入摘要及真实消费者，保留原失败、进程和清理结果。

### 镜像发布与证据边界

Nucleus 的[发布与冷拉取合同](https://github.com/headironc/nucleus/blob/8d18e521b3ec6be675603939335c77bdb33bbbc2/halo/README.md#首次私有包发布与冷拉取)拥有实际协议。构建、独立下载、发布和冷消费分别核来源与结果。配方 SHA、输入摘要、归档摘要与 registry digest 各有含义；滚动 APT 来源不提供逐字节可复现保证。

镜像归档属于发布输入，另核可信构建来源、配方、原 run/attempt、不可变 artifact ID 和字节摘要。artifact 名称、分支名或自述 provenance 不代替来源。下载和解包限制大小、文件数、路径和链接。可信发布任务不执行 producer 候选配方代码，写权限不进入普通检查或本地验收容器。

原固定私有镜像的短期 job-token 冷消费已验，范围包含非 root、私有 PG、双 Chromium 与 133 个薄消费者测试。该历史结果不证明其他 digest、完整 Halo 网站或全部权限负例均已通过。包访问继续使用准确仓库的短期 token 和最小权限，公开源码不自动公开 GHCR 包、现场配置或恢复材料。

发布结果未知时，先确认原进程、上传记录及准确标签和 digest，沿原发布合同恢复。冷缓存准备只适用于合同指定的全新 hosted job，不能据此清理共享 daemon。源码合并、工具回归、真实发布和业务消费分别验收，不能相互替代。
