# LXD 源码适用性判断

本判断仅覆盖 [Go 漏洞入口](scripts/go-vuln.ts)选定的 Linux amd64 源码图。五项公告的 daemon 缺陷实现没有编译进入该图，因此可按准确绑定的 `source-not-affected` 记录处置。原始告警及原生文本退出 3 继续保留。本地与 CI 的通过条件为处置绑定有效且没有未处置的可达告警。

这不证明实际 LXD daemon 已修复。实际部署版本、配置、权限和其他消费者仍未核验，也不属于本工具的操作范围。此判断不豁免整个 LXD 模块，不授权服务端调查或修改。

## 已核源码

本次领域判断覆盖 [GARM 管理会话窗口](runner/session.md)候选，基线为已合并的 [81dafd0](https://github.com/headironc/designs/commit/81dafd091e96e9acb9dee0c4df1e96c01565a45c)。准确 Go 源码与两锁由[五项处置记录](scripts/go-vuln-lxd.json)绑定。记录随本片 PR 接受独立代码和安全审查；领域核定时刻不代表正式审查已完成。

依赖固定为 `github.com/canonical/lxd@v0.0.0-20261012123619-3c94d4fae7ff`。固定 Go 1.27.3、Linux amd64、cgo、`-test -tags=integration` 的实际图仅包含 `client` 和 13 个 `shared` 包。准确包名列在记录中，没有 daemon 的 `lxd` 包和服务端 handler。

当前调用链如下：

- [provider 观察入口](runner/internal/provider/observations_linux.go)的 `withLXD` 使用显式 Unix socket，禁代理和重定向。`lxdReads` 限制 GET；创建、读取及删除观察复用此入口。实际写操作由独立的 [native provider](runner/internal/provider/native_linux.go) 负责，其安全状态不由该 Go 客户端判断替代。
- [lifecycle 客户端](runner/internal/lifecycle/machine_linux.go)的 `withMachineClient` 使用相同的 Unix socket 约束。诊断读取实例、卷和 operations，不提供证书管理或浏览器接口。
- [operations 诊断投影](runner/internal/lifecycle/diagnostic_machine_linux.go)仅提取 ID、状态与位置，不公开完整 metadata 或 WebSocket secret。传递导入 `shared/ws` 不等于业务使用终端劫持入口。实际 daemon 对其他读者的 secret 过滤仍是部署责任。
- [会话生产准入](runner/internal/lifecycle/session_host_linux.go)使用[会话资源观察](runner/internal/lifecycle/session_machine_linux.go)。新增 GET 覆盖所有 project 的实例、专用池卷及 operations。它复用固定 Unix transport，不调用证书管理或 WebSocket。operations 只投影 ID、状态与位置，形成私有准入摘要。job 和公开报告均不接收完整 metadata。JWT 认证域轮换作用于 GARM，不增加 LXD daemon handler。

本次重新检查了新增生产调用链、依赖锁及选定包图。原始扫描报告仍有五项可达符号告警，原生文本退出 3。当前图仍只有上述 14 个 LXD 包，没有本地非 Go 或 embed 输入。LXD 版本、checksum、工具范围及五项完整 OSV 内容均与记录相符。本次只更新准确源码绑定与核定时刻。逐项结论及 daemon 未核责任保持，不扩大豁免范围。

本片审查修复又核定了窗口永久锁和退役拒绝回执的源码变化。这些变化只影响本地锁创建及原响应持久化和恢复，不新增 LXD 调用、依赖或编译输入。上述 LXD 入口与投影保持，处置记录绑定修复后的准确 Go 源码。

## 逐项依据

| GO ID 与公告                                                                             | 当前源码成品的判断                                                                                          | 未核部署责任                                                                  |
| ---------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| [GO-2026-5306](https://github.com/canonical/lxd/security/advisories/GHSA-c3h3-89qf-jqm5) | daemon 更新 restricted TLS 证书 type 字段时的授权 handler 不在编译图内，当前客户端不调用证书更新接口。      | 准确 daemon 系列、revision、修复及 TLS 身份和监听未知。                       |
| [GO-2026-4595](https://github.com/canonical/lxd/security/advisories/GHSA-crmg-9m86-636r) | daemon 非递归证书列表缺少逐对象过滤；该实现不在编译图内，当前源码不调用证书列表。                           | daemon 6.6 的适用范围及修复未知。                                             |
| [GO-2025-4121](https://github.com/canonical/lxd/security/advisories/GHSA-3g2j-vm47-x4mj) | 缺陷属于 daemon 的 storage pool 权限及 shifted custom volume 处理，不是当前 client 解码实现。               | pool 目录权限、启动迁移、其他 shifted volume 和宿主访问面未知。               |
| [GO-2025-4003](https://github.com/canonical/lxd/security/advisories/GHSA-p8hw-rfjg-689h) | 当前成品不提供 LXD-UI、浏览器证书认证或服务端表单接口；其客户端使用 Unix socket。                           | 实际 UI/TLS、客户端证书认证和服务端请求防护未知。                             |
| [GO-2025-3999](https://github.com/canonical/lxd/security/advisories/GHSA-3g72-chj4-2228) | 泄漏 secret 的 daemon handler 不在编译图内。当前业务没有利用泄漏 secret 建立终端或控制台 WebSocket 的路径。 | Runner 确实读取 operations；daemon 的过滤、身份权限和其他消费者仍须独立核验。 |

原生调用报告中的 `api.NewStatusError`、`api.NewURL`、`MetadataAsStruct`、client 初始化和文件读取等共用符号，不是上述服务端缺陷可利用性的证明。[Go 数据库的版本映射限制](https://go.dev/doc/security/vuln/database#versions)也说明，Go 模块版本不能直接替代产品版本判断。

## 有效性与恢复

记录绑定所有 Runner Go 源码（含测试）和 `go.mod`、`go.sum` 的有序路径及 SHA-256。纯 Markdown 变化不使判断失效。新增、删除或修改 Go 源码、依赖锁变化、LXD 包集合变化、工具或构建范围变化、五项 OSV 内容变化，都要求重新判断。每条处置还核对准确模块版本、checksum 和 finding 的首帧包名。

源码摘要计算对象为按路径字节排序的 `{path, sha256}` 数组。OSV 摘要计算对象为原生 JSON 中解码后的单个完整 `osv` 对象；两者均对 `JSON.stringify` 的 UTF-8 字节计算 SHA-256。内容相同的重复 OSV 事件可接受，内容冲突则失败。字段序列化变化也可能保守地使旧摘要失效，不自动规范化或刷新记录。

当前 Runner 没有本地非 Go 编译或嵌入输入。工具读取 `go list` 元数据，拒绝新增 cgo、本地汇编、其他编译输入或 embed 输入的旧处置。未来若确有消费者，先明确把这些字节纳入受审输入，再修改入口。依赖自身的编译输入由准确锁及模块缓存校验约束。

`reviewedAt` 记录本次源码判断与准确绑定核定的 UTC 时刻，不代表后续正式 PR 审查已经完成。它不自动更新，也不用于自动延期。这里不是限期接受实际漏洞风险，没有日历豁免或自动刷新流程。记录缺失、损坏或失效时保留失败。恢复时保留原报告，重新核公告与源码调用链，再在普通 PR 中更新记录并接受独立审查；不得只刷摘要来消除失败。

绑定有效是独立的通过条件。即使原生扫描退出 0、可达告警为零，绑定失效也返回 CLI 1，并显示具体原因。修改 build tags 后路径离开选定图、删除 LXD 依赖或上游修正 OSV，都不能自动使旧判断通过；应在审查中更新或退役不再适用的记录。这不把原生零发现改写为漏洞命中，也不把仅模块或包层面的发现升级为可达告警。有效完整绑定且没有可达发现时仍可通过。

其他未知或新增可达告警继续阻断。仅模块或包层面的发现沿原 symbol 门槛完整展示，没有被本记录处置。报告中的 `nativeJSONExitCode`、`nativeTextExitCode`、`disposed` 和 `pending` 分别保留扫描完成、原生漏洞判断与源码适用性结果。`assessment.json` 只是扫描阶段结论，最终 CLI 还必须完成输入复核和资源清理。
