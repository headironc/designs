# S/P 使用与维护

designs 维护共享 CI 执行实现 S，也维护自己的格式与文档检查策略 P。S 位于本目录，designs 的完整 P 位于 [`policy/checks.ts`](../policy/checks.ts)。agents 和 Nucleus 分别维护自身 P。[CI 架构](../ci-architecture.md)拥有跨库职责与信任边界；本页维护实际工具调用。

## 选择入口

| 使用者与目的         | 入口                                                                                         | 结果范围                                   |
| -------------------- | -------------------------------------------------------------------------------------------- | ------------------------------------------ |
| S 开发者             | 根目录 `typecheck`、`test`                                                                   | 类型检查和普通回归                         |
| P 来源准备者         | [`bootstrap.mjs`](bootstrap.mjs)                                                             | 校验 bundle、源码与锁，展开新目录          |
| 固定 P               | [`BoundInputs.verify`](binding.ts) → [`VerifiedExecution.run`](execution.ts) → 本库 verifier | 绑定实际输入、隔离执行、验证业务义务       |
| 本地维护者           | [`local-docs.ts`](local-docs.ts)                                                             | 用一次性容器运行格式与文档探针，保留原输出 |
| 固定 hosted producer | [`upload-hosted.ts`](upload-hosted.ts)                                                       | 上传原 job 的报告并保留阶段记录            |
| hosted 证据消费者    | [`HostedReport.read`](artifact.ts)，再运行 P verifier                                        | 校验平台来源与独立业务义务                 |

runner 的 Go 检查、模板渲染和服务入口见 [runner 文档](../runner/README.md)。文档检查通过不代表 Go 测试、真实服务或 VM 生命周期通过。

## 开发依赖与检查

在仓库根目录运行。Node 与 Bun 的准确版本由 [`package.json`](../package.json) 固定，依赖由 [`bun.lock`](../bun.lock) 固定。当前分别是 Node 26.8.1 和 Bun 1.4.2。先核实际命令版本，再安装固定依赖。

```sh
node --version
bun --version
bun install --frozen-lockfile --ignore-scripts
```

先按下节准备固定 S 的实体 `.shared` 和依赖，再回到仓库根目录检查。正常类型检查也覆盖 P，缺少 `.shared` 时会失败。

```sh
bun run typecheck
DESIGNS_GO_BIN="$PINNED_GO_BIN" bun run test
```

安装访问依赖源，写入当前目录的 `node_modules` 和 Bun 缓存。下载失败或锁不匹配时停止，不改锁去适配机器。`typecheck` 执行 `tsc --noEmit`；`test` 执行 `node --test ci/*.test.ts ci/*.test.mjs`。实际检查范围由 manifest 与 [`tsconfig.json`](../tsconfig.json) 维护，不包含容器验收或 artifact 上传。

普通回归使用临时文件、Git fixture、本地测试服务和进程替身。真实 UID 切换与进程清理另在一次性容器验收。不要在共享宿主用 root 直接运行 `container-acceptance.ts` 或 `designs-acceptance.mjs`。

P 静态导入 `.shared/ci/`。核验 P 的类型或运行 P 前，先准备相同版本的实体 `.shared`。安装 S 自身依赖不等于准备好了 P/S 组合。

## 全库格式

[`policy/format.ts`](../policy/format.ts) 维护本地与 CI 共用的格式规则。Prettier 3.9.9 由 manifest 和依赖锁固定；Go 使用 [runner 固定工具链](../runner/README.md#构建与检查)的 `gofmt`。`PINNED_GO_BIN` 指向该工具链的 `bin` 目录，内含 `go` 和 `gofmt`。入口核 `gofmt` 的构建版本为 Go 1.27.3，不下载工具或依赖。

```sh
bun run format:check --go-bin "$PINNED_GO_BIN"
bun run format --go-bin "$PINNED_GO_BIN" ci/local-docs.ts
bun run format --go-bin "$PINNED_GO_BIN"
```

`format:check` 只读；`format` 显式写回。未传文件时，入口选择 Git 跟踪文件及未忽略的新文件，排除已经删除的路径。传文件时使用仓库相对路径，且仅处理列出的支持格式。先检查，再按编辑范围选择修复；写回后检查 diff。检查退出码 0 表示通过，1 表示存在格式差异，2 表示输入、工具或语法错误。写回不是事务：中途失败时，先前文件可能已经修改；保留 diff，修正失败后重新检查。

Prettier 覆盖 `.ts`、`.tsx`、`.js`、`.jsx`、`.mjs`、`.cjs`、`.json`、`.md`、`.yml`、`.yaml`；`gofmt` 覆盖 `.go`。规则固定 LF，Markdown 保留段落换行，禁用代码块内嵌格式化。候选的 Prettier 配置、ignore 与插件不参与规则加载。文件名按普通路径处理，不作为 glob 展开；符号链接、越界和重复路径失败。没有支持格式的选择也失败。

`bun.lock`、`go.mod`、`go.sum` 由包管理器维护；Dockerfile、systemd unit、配置与模板未配置通用格式器。这些文件仍属于源码与其业务检查范围，格式检查不会宣称覆盖它们。忽略的依赖、缓存和历史运行证据不参与本地全库选择；被 Git 强制跟踪的支持格式仍会检查。

固定 P 从已验证候选清单构造 `format.sources`，与三项文档检查共同属于 `maintenance` phase。命令从固定 P 启动同一入口，对准确候选执行 `--check`。P/S 工具记录绑定 Prettier 全部文件及 Go 两个二进制的摘要；候选只提供被格式化的内容，不能提供检查脚本。旧 [`policy/docs.ts`](../policy/docs.ts) 仍只代表文档义务，不能据此宣称全库维护检查通过。新 P 的 hosted 发布与来源激活继续由部署 owner 独立处理，本地通过不表示平台已经启用。

## P 准备固定 S

来源指定者提供准确 bundle 和独立固定的锁。锁包含 `commit`、`tree`、`lockPath`、`lock`、`distribution`。`lockPath` 为 `bun.lock`；后两项分别是锁文件与 bundle 的 SHA256。Git 对象使用完整 ID。包自报值或下载后临时计算的摘要不能代替独立来源批准。

P 持有准确 S 版本的 bootstrap 副本，静态导入 `P/.shared/ci/`。`.shared/` 在 P 中被忽略且未跟踪，内部保留自身 Git 和依赖身份。P、S、候选 C 使用实体路径；只允许这个准确 P/S 嵌套，C 另置。外部 `node_modules` 符号链接不能代替 S 的实际安装。

下列变量由固定 P 来源确定。`S_LOCK` 指向独立固定的锁，`S_BUNDLE` 指向对应 bundle。目标 `.shared` 尚不存在，其父目录由准备者独占拥有。

```sh
node "$P_ROOT/ci/bootstrap.mjs" "$S_BUNDLE" "$S_LOCK" "$P_ROOT/.shared"
cd "$P_ROOT/.shared"
bun install --frozen-lockfile --ignore-scripts
```

bootstrap 校验不超过 128 MiB 的 bundle、唯一发布 ref、commit/tree 和锁。它拒绝符号链接、submodule 与特殊成员。临时展开成功后才发布目标。失败时保留原 bundle 和锁，不运行部分展开内容。已有目标不会被覆盖；组合更新使用新的准备目录，保留旧执行绑定。

P 自身依赖另按其配方准备。安装成功不证明 runtime、工具字节或宿主权限适合监督执行。源码合并、bundle 发布和运行来源激活分别接受；本页命令不授权激活。

## 执行与业务结果

固定入口提供 [`Composition`、`Candidate`、`ExecutionPaths`、`PolicyTarget`](contract.ts)。组合绑定 P/S 的 commit、tree、锁及分发摘要。目标绑定 repository、repository ID、目标分支和 workflow。普通 PR 绑定 B/H/I 和 tree；I 的两个父提交严格依次为 B、H。push 候选另绑定 commit/tree。

调用链为 `BoundInputs.verify` → `designsChecks` → `VerifiedExecution.run` → `verifyDesigns`。P 从固定来源构造检查义务，不从候选加载配方或 verifier。S 核实际加载位置、清洁 P/S/C、源码条目、分发和工具字节。监督 Node 的版本和摘要也参与校验，`NODE_OPTIONS` 与 `NODE_PATH` 不能改变启动。

每个 `Check` 包含唯一名称、绝对 argv、cwd、环境增量和期限。工具记录包含 executable、SHA256、版本参数与预期输出，另附所用脚本的摘要。P 声明业务环境和候选输出位置。HOME、TMPDIR、PATH、启动环境与凭据隔离由 S 拥有。

候选类型检查也需要静态 S 导入时，P 在 preparation 中声明准确 S 根目录到候选 `.shared` 的完整复制。目标须被候选忽略且未跟踪；S 排除 Git 元数据，并保护复制后的源码和依赖。部分复制、其他来源、逃逸链接及可写 `.shared` 均拒绝。可信执行器仍从 P 的 `.shared` 加载，候选副本只作为检查输入。完整边界见 [CI 架构](../ci-architecture.md#三条消费流程及失败恢复)。

supervisor 以 root 保护输入，以非 root 身份运行检查。它会改变准备树的 owner 和权限，因此输入使用可丢弃副本。报告目录在全部输入之外，由 root 所有且权限为 `0700`；S 独占写入 `0600` 的 `report.json`。重复运行另建报告目录，不覆盖原结果。

每项检查有独立 scratch，进程结清后才删除。verifier 要求检查确实启动、成功退出、没有信号、错误、超时或取消，并已结清进程。报告中的布尔值或结构合法的 JSON 不能独立构造认证执行。

## designs 文档义务

[`policy/docs.ts`](../policy/docs.ts) 从已验证候选清单选择全部 `.md` 文件，并要求包含 `AGENTS.md`、`README.md`、`ci-architecture.md`、`runner/README.md`。命令 cwd 是固定 P，operand 指向准确候选文件。

| 检查         | 当前固定工具与参数                                                                          | 成功范围                                             |
| ------------ | ------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `docs.prose` | Node 26.8.1、prose 3.0.0、`--errors-only`                                                   | 所选文档没有工具识别的中西文空格错误；句长提示不阻塞 |
| `docs.ids`   | Node 26.8.1、ids 1.0.0、全文件                                                              | 表格编号规则；不按改动行截断编号关系                 |
| `docs.links` | lychee 0.24.2，`--config /dev/null --offline --no-progress --include-fragments=anchor-only` | 离线链接与适用锚点；不证明远程服务或外链内容有效     |

每项限时 180 秒。脚本和 Node 字节都纳入工具摘要。通用检查器由 agents 交付维护；designs 固定本库配方，不复制个人通用工具合同，也不要求 CI 从某个人的 Codex home 取输入。

## 本地容器验收

[`local-docs.ts`](local-docs.ts) 调用 [`designs-acceptance.mjs`](designs-acceptance.mjs)。调用者指定本机镜像的完整 ID、固定 S bundle 与锁、文档工具、Go 二进制目录和新的输出目录。Prettier 来自已安装的固定依赖。入口不拉取镜像，不安装工具，不读取真实平台凭据。

```sh
node ci/local-docs.ts \
  --image "$LOCAL_IMAGE_ID" \
  --shared-bundle "$S_BUNDLE" --shared-lock "$S_LOCK" \
  --node "$PINNED_NODE" --lychee "$PINNED_LYCHEE" \
  --prose "$PINNED_PROSE_SCRIPT" --ids "$PINNED_IDS_SCRIPT" \
  --go-bin "$PINNED_GO_BIN" \
  --output "$NEW_EVIDENCE_DIRECTORY"
```

镜像可按 [`test.Dockerfile`](test.Dockerfile) 从准确 `BASE` 单独准备。该步骤可能联网，镜像准备不属于上面命令的隐式行为。镜像提供 Git、procps、sudo 与证书；它只是测试镜像，不是已发布的 Kiln 成品。

入口复制 Git 源码清单和已安装依赖，保留输入摘要；仅将准备副本挂入容器。容器使用 `--init`、只读根、独立 PID namespace、无网络及临时 `/work`、`/tmp`。工具与源码只读，只有 evidence 目录向宿主输出。运行不挂载宿主服务 socket 或凭据。

探针在容器内建立本地 P/C 提交，S 使用调用者提供的原 bundle，不重新打包。P/S 的 Bun 锁相同才复用已安装依赖；锁不同则拒绝。四项检查经真实 `.shared` 静态调用链执行，并绑定 `headironc/designs`、repository ID `1434658318` 和 `main`。workflow 路径只是本地标签，不表示同名 workflow 已安装。

报告为 `local-development-probe`，包含准确 S 组合与本地候选。通过只证明本次开发快照的调用链，不替代三个真实 P 的组合验收或 hosted 来源核验。

输出保留输入 receipt、容器身份、日志和 `designs-probe.json`。退出或取消后入口核原容器状态；运行中的容器只请求一次停止，再查原状态。未知状态使本次失败，保留身份供恢复，不创建第二次执行。已停止的容器也保留供查验；确认原状态后由维护者按 receipt 中的准确 ID 删除，不使用批量 prune。

排障先读输出目录的 `result.json`，核 `passed`、`cancelled`、`attached`、`container.State` 和 `error`。准备阶段失败可能只有 `receipt.json`，尚无 result 或容器日志；保留调用错误，核 receipt 中的原容器名是否实际创建，不能只凭分配了名称认定已启动。磁盘配额或复制失败先修复存储条件，再使用新输出目录，保留原失败目录。

若没有生成 `evidence/designs-probe.json`，再读已有的 `container.log` 定位失败阶段，不能把缺报告当作检查通过。已有报告时，检查 `report.errors` 和 `report.observations`：用 `request.name` 找到失败义务，连同启动、退出、超时、取消、结清状态及 `diagnostic` 判断。诊断文本供定位问题，不能替代 verifier 的结果；原报告可能含候选输出，按私有证据保存，不直接粘贴到公共日志。

本版本监督器返回的 `diagnostic` 是含 `stdout`、`stderr` 的 JSON 字符串；监督器返回前失败时该字段可能为空。两路各保留首尾 16 KiB，超出部分标出省略字节数，不代表保留了完整日志。按报告绑定的 S 版本解释历史诊断，不能对旧报告强行套用新格式。两路累计输出超过 32 MiB 时检查失败，仍须核进程是否结清。诊断保留与输出限制由 [S 的启动合同](../ci-architecture.md#运行时启动也是可信输入)维护。

[`container-acceptance.ts`](container-acceptance.ts) 验收监督器自身的行为：非 root、失败退出、超时、取消、后代结清和私有数据拒绝。该脚本仍限于一次性容器，不能在宿主直接运行。

[`preparation-acceptance.mjs`](preparation-acceptance.mjs) 验收候选 `.shared` 的完整只读复制及七项拒绝场景。它导入文档探针，复用探针已验证的 inputs，必须同样传入固定 bundle 和锁；缺少参数会在建立夹具前失败。仅在采用上述相同隔离设置、工具与源码挂载的全新容器内，将启动命令替换为：

```sh
/host-tools/node /source/ci/preparation-acceptance.mjs \
  /shared-input/shared.bundle /shared-input/shared.lock.json
```

维护探针先通过，preparation 才继续。核新 evidence 目录的 `preparation-report.json` 和 `preparation-result.json`，确认正例成功且七项拒绝均完成。该验收会向可丢弃 S 副本加入逃逸链接反例，不复用它运行后续验收，也不覆盖原报告；`local-docs.ts` 默认只运行维护探针。

格式反例使用同样的新容器和 bundle/lock 参数，将启动脚本替换为 [`format-acceptance.mjs`](format-acceptance.mjs)。它先运行完整维护探针，再分别构造未格式化和已修复的候选。核 `format-result.json` 及对应原报告：`format.sources` 先退出 1，再退出 0，且两次均已结清。候选的 `*` 忽略配置不能让未格式化代码通过。普通格式测试也使用真实 `gofmt`，通过 `DESIGNS_GO_BIN` 指定同一工具链。

## Hosted 上传与消费

固定 P 的 JavaScript action 拥有报告 operand 和 artifact 名。S 的上传入口核准确 Node、真实 hosted 环境及原 job 的受保护报告。`ACTIONS_RUNTIME_TOKEN`、`ACTIONS_RESULTS_URL` 由平台提供，不接受候选提供或写进日志。

```sh
node "$P_ROOT/.shared/ci/upload-hosted.ts" "$REPORT_FILE" "$ARTIFACT_NAME"
```

成功时向 `GITHUB_OUTPUT` 写 `artifact-id` 与 `artifact-digest`。日志保留 prepared、create、content、finalize、verified 阶段及身份、大小和摘要。非零退出不证明远端未生效；保存原报告和阶段记录，按原 run/attempt/job/artifact 恢复，不重新触发检查掩盖缺失报告。

读取方独立固定 [`HostedExpectation`](artifact.ts)：组合、候选、源码、工具、目标、producer，以及 job、artifact、workflow blob 和 head branch。预期不能来自待验证报告。`HostedReport.read(expected, token, signal)` 验证来源后，才允许 `BoundInputs.fromHosted`、`VerifiedExecution.fromHosted` 构造认证对象；P 随后重建自身义务并运行 verifier。

S 核原 attempt 的完整 job 列表、准确 artifact ID、平台摘要和 workflow，再读取有界 ZIP，只接受一个普通 `report.json`。存储下载不携带 GitHub 凭据。读取方核本地 P 来源；需候选材料重建业务义务时，先调用 `inputs.verifyCandidate`。原 argv 使用认证后的远端路径，不换成读取机器的目录。

## 失败处置

| 情况                                    | 保留与下一步                                        |
| --------------------------------------- | --------------------------------------------------- |
| runtime、source、lock、工具或路径不匹配 | 停止执行，核原独立绑定；不换成浮动版本或候选脚本    |
| 检查非零、信号退出、未启动              | 保留原观察；修复后另建准确候选，原失败仍保留        |
| 超时或取消                              | 区分停止等待、子执行终止和进程结清；沿原句柄核终态  |
| 进程未结清                              | 保留 scratch 和原身份，不复用身份或把缺报告当成功   |
| 上传结果未知                            | 保留原报告、身份和阶段记录，恢复原操作，不盲目重发  |
| 平台原件、摘要或绑定缺失                | 报告证据不足；JSON 合法或本地通过不等于 hosted 接纳 |

S 拥有监督、传输和来源校验，P 拥有检查义务和业务结果，部署 owner 拥有激活与配置。恢复旧执行继续使用原版本和记录，不靠换目录、重算绿色报告或清记录放行。
