# Designs

本仓库维护跨仓库设计和 [runner 自有 Go 实现](runner/README.md)。agents、Nucleus 与 designs 分别维护原生 CI 和业务检查。旧共享执行器、固定配方和自动准入已退役，新 CI 从零建立。

## 当前设计

- [CI 与运行边界](ci-architecture.md)：原生 CI、独立审查、分支保护和发布入口的职责；保留 runner 与 Kiln 的资源隔离及恢复要求。
- [Rust 与 Halo 共用 runner](ci-architecture.md#runner-与-kiln)：共用基础设施、两层镜像、不可信池边界、生命周期、缓存和接入验收；文档收录不表示已部署。
- [Runner 的 Go 结构与实施切片](runner/architecture.md)：自有代码职责、已采用的 Go 包、协议与恢复边界，以及小 PR 顺序。[单次 VM 创建](runner/create.md)与 [VM 删除](runner/delete.md)保留准确目标、写前记录和未知结果不重发语义，删除确认后才释放 provider 容量。[正常读取](runner/read.md)核验版本与绑定实例，保持 guard 记录。

## 设计与实施

设计正文保留唯一维护位置，后续变更通过 PR 讨论。各文档明确标记草案、已接受或已替代；接受或替代时记录决定的范围、理由和关联版本，不复制整段聊天历史。

文档合入表示收录该版本。架构接受与实施片选择仍以 Human 的明确决定为准，不能从一次文档合入推导出所有阶段的实施授权。

产品仓库继续维护实际生效的业务契约、实现说明和验收入口，并引用对应设计版本。运行中的 CI 和审查不得依赖本仓库的未固定分支内容。

消费仓库的产品源码仍由各仓库维护。实际部署配置、凭据和运行原件不进入本仓库。runner 代码合并与生产切换分别验收。

## 格式与工具检查

[关闭后锁定](.github/workflows/lock-pr.yml)在 PR 关闭或合并后锁定对话，owner 和合作者仍可评论。它只消费默认分支的 workflow，不 checkout 或执行候选代码；使用本次 `GITHUB_TOKEN` 的 `pull-requests: write`，不读取仓库 secret 或 PAT。执行前回读原 PR，已重开或已锁定就跳过。写入报错也回读，不重发，未确认锁定则任务失败。同一 PR 的事件串行处理。事件处理有延迟，PR 在读取与锁定之间被重开仍有竞争窗口。该入口不解锁重开的 PR，不关闭或归档 PR，也不限制 open PR 的外部评论。历史批量锁定单独执行，不由此工作流扫描补做。

本地与 [原生 PR 工作流](.github/workflows/format.yml)执行相同入口。格式工具要求 Linux，并可访问 `/proc/self/fd`。缺少该条件时直接失败；其他平台未支持。仓库根路径的每级目录和所选文件都拒绝符号链接，请使用真实目录路径。准备 `package.json` 声明的 Node 26.8.1、Bun 1.4.2，以及 `runner/go.mod` 声明的 Go 1.27.3。使用官方工具链安装，并核对下载的校验和；Go 准备方式见 [runner 说明](runner/README.md#构建与检查)。随后在仓库根目录运行：

```sh
bun install --frozen-lockfile --ignore-scripts
bun run format:check
bun run typecheck
bun run test:format
```

`format:check` 默认检查 Git 已跟踪文件和未忽略的新文件，排除工作树已删除的文件。忽略的依赖、缓存和构建产物不进入默认范围；强制跟踪的文件仍会检查。支持 `.ts`、`.tsx`、`.js`、`.jsx`、`.mjs`、`.cjs`、`.json`、`.md`、`.yml`、`.yaml` 和 `.go`，其余文件跳过。Prettier 使用根目录 `.prettierrc.json`，Go 使用真实 gofmt；配置与依赖可随普通 PR 修改。工具不查找父目录配置或 EditorConfig，也不使用 `.prettierignore` 改变 Git 源码范围。

传入文件时只处理这些仓库相对路径。路径按字面解释，不展开 glob；带空格或方括号时给 shell 参数加引号。例如：

```sh
bun run format:check README.md scripts/format.ts
bun run format README.md
```

只选择 Prettier 文件时不要求 Go。包含 Go 文件或运行完整回归时，工具从 `PATH` 查找 `go` 与 `gofmt`，并核对 gofmt 的构建版本。另一个已准备的工具目录可通过 `DESIGNS_GO_BIN=/absolute/path/to/bin` 指定。`typecheck` 仅覆盖 `scripts/`；`test:format` 覆盖格式工具的读写、文件选择、失败与并发保护，包含真实 gofmt。

检查只读，格式差异退出 1；路径、工具、解析或读写错误退出 2。`bun run format` 显式写回全部支持文件，也接受同样的文件参数。修复后查看 `git diff`，再运行检查。写回逐文件进行，后续文件失败时先前修复可能已保留。先核对日志和 diff，再决定后续修复。工具通过已打开的目录 fd 逐级访问文件，打开时使用非阻塞和不跟随链接标志。目录改名不会将已保留的 fd 重定向到外部文件；写前仍复查根目录、文件身份和内容元数据。链接、特殊文件、无效 UTF-8、语法错误或检测到的并发修改会阻止对应文件写回。写回并非事务，也不提供文件锁；最后一次核对之后的并发改名或编辑仍有竞争窗口。停止并发编辑后重新检查，不把失败当作已回滚。

工作流在 PR 和 `main` push 上运行格式、类型、内部链接与工具回归，仅持源码读取权限。当前使用 GitHub hosted `ubuntu-latest`；真实调度结果与 required checks 由平台交付另行核验。格式通过不代表完整 Go、文档结构或安全检查通过。依赖准备失败时保留原错误，核对工具版本、网络和锁文件；用相同锁重新准备后再检查，不通过删除检查或修改锁文件掩盖失败。工具或配置修复使用普通 PR，并保持 manifest 与锁文件一致；回退同样恢复配套源码、配置和锁。

旧执行器与配方的准确版本保留在 Git 历史。原操作和未知结果的恢复继续使用原身份与原材料，不通过恢复旧门禁或兼容桥接实现。

## 文档内部链接检查

在上述 Node 和 Bun 环境中运行以下入口。本地与原生 PR 工作流使用相同命令；链接工具仅支持 Linux amd64，并要求 `/proc/self/fd` 和 `/usr/bin/tar`。

```sh
bun run links:prepare
bun run test:links
bun run links:check
```

`links:prepare` 是显式联网准备步骤。它从官方 release 下载 lychee 0.24.2，并核对固定归档大小和 SHA-256。工具只提取指定的二进制成员，大小、摘要和版本也必须匹配。声明由 [链接工具](scripts/links.ts)统一维护。工具只安装到仓库忽略的 `.cache/designs-links/lychee`，不使用 `PATH` 中的 lychee，也不修改用户配置。下载期限为 120 秒，提取和版本验证分别有界。下载、提取或发布前验证失败会保留原安装。全部核验完成后，通过缓存目录内的原子 rename 发布，再读回安装结果。

发布后读回失败不代表旧安装仍在。先核对原缓存路径、文件身份与固定摘要，再决定是否重新准备。普通失败会清理本次暂存目录；暂存路径被并发替换时保留现场并报告错误。中断可能遗留 `.cache/designs-links/prepare-*`。先确认原进程已经结束，再核对原目录归属和内容，按原路径处理；不要删除其他运行的暂存目录。

`links:check` 默认检查全部 Git 已跟踪及未忽略的新 `.md` 文档，排除工作树已删除的文件。图片、代码等链接目标来自同一源码集合，忽略的本地文件不能补足断链。工具按原相对路径复制完整源码，并保留目标文件字节。lychee 负责解析 Markdown、HTML 内嵌链接和标题锚点。重复标题与中文锚点沿用 lychee 的规则；代码块和行内代码中的示例不检查。仅接受实际解析成超链接的引用，未定义的 reference 标签属于文档结构检查范围。

检查离线运行，外部 URL 被排除，不对外站可用性作结论。以 `/` 开头的本地链接按仓库根解析，目录目标允许存在的源码目录。越过源码根的相对链接或 `file:` URL 直接报告失败，不检查宿主对应文件。根路径的每级目录、源码文件和工具路径都拒绝符号链接；源码集合中的特殊文件也会失败。文档文件名允许空格、中文、隐藏路径和 glob 特殊字符，控制字符路径明确拒绝。命令不接受选检参数，未知参数退出 2。

lychee 在独立控制目录运行，并使用显式空配置。固定参数强制离线，禁用缓存和忽略规则。子进程环境移除 token、代理和 lychee 设置。仓库或父目录的配置、`.ignore` 和 `.lycheeignore` 不能缩小范围。`XDG_CONFIG_HOME` 仅指向本次子进程控制目录，不修改持久配置。运行前校对 lychee 实际消费的文档清单，复制后核对快照字节。结束时复查源码集合、根及文件身份和内容。检测到并发变更时检查失败；该检查不提供源码锁，最后一次核对之后仍有竞争窗口。停止并发编辑后重新检查。

成功退出 0；内部断链退出 1，并报告仓库路径、可用的行号、目标及原因。缺工具、摘要或版本错误、输入遗漏、解析诊断、并发修改等检查错误退出 2。检查不会联网补装或自动修复文档。先按原错误修复路径、目标或环境，再运行同一入口；工具损坏时重新运行 `links:prepare`。网络或校验失败时保留错误，核对官方发布材料后再准备，不改用宿主任意版本。升级通过普通 PR 同时更新唯一工具声明和说明，重新执行准备、`test:links`、`links:check`、类型及格式检查。回退源码后，按回退版本重新准备工具并检查。

## Go 依赖漏洞检查

漏洞检查与完整 Go 测试分别运行。[原生 Go 工作流](.github/workflows/go.yml)中的 `vulnerabilities` job 使用 `ubuntu-latest`，与本地执行同一命令。漏洞扫描不需要 Docker、nft、私有 user/network namespace 或 runner 服务，因此无需沿用测试 job 的 Ubuntu 22.04 限制。环境为 Linux amd64、可用的 `/proc/self/fd`、Node/Bun 和完整 Go 1.27.3 SDK。SDK 可通过当前 `PATH` 或 `DESIGNS_GO_BIN` 指定，工具会核对版本。首次运行先显式准备：

```sh
bun install --frozen-lockfile --ignore-scripts
bun run go:vuln:prepare
bun run test:go-vuln
bun run go:vuln:check
```

[Dependabot 告警](https://docs.github.com/en/code-security/concepts/supply-chain-security/dependabot-alerts)检查默认分支的依赖图，依据 GitHub Advisory Database 生成告警。查看本仓库的 [Security → Dependabot](https://github.com/headironc/designs/security/dependabot)。告警由仓库平台设置启用，本工作流不会自动开启该设置，也不会自动启用依赖升级 PR。本地和 PR 中保留 [govulncheck 调用图检查](https://go.dev/doc/security/vuln/)，用于分析当前候选的 Go 函数可达性。两者范围不同，任何一项无告警都不保证所有依赖安全。

`go:vuln:prepare` 通过 `proxy.golang.org` 与 `sum.golang.org` 构建固定的 `govulncheck v1.9.0`，并下载 runner 锁定模块。构建使用 Go 1.27.3、Linux amd64、`GOAMD64=v1`、`CGO_ENABLED=0`、`-trimpath` 和 `-buildvcs=false`。唯一工具声明位于 [go-vuln.ts](scripts/go-vuln.ts)，包含固定二进制 SHA-256；版本输出不能替代字节校验。下载、构建、校验和收尾成功后才原子替换工具，准备失败保留旧安装和本次日志。准备不会下载 SDK、拉取镜像或修改用户配置，也不代表扫描通过。

检查先验证工具字节与构建身份，再执行私有副本。源码来自 Git 已跟踪文件及未忽略的新文件中的 `runner/`，排除工作树删除项；忽略文件不能补入源码。协议解码 helper 和五项处置记录也纳入同一输入绑定。路径与文件拒绝符号链接和特殊文件，副本、原源码及模块锁均核对前后内容。当前入口拒绝模块 replace 指令，避免从副本之外补入源码。并发修改或清理失败属于操作失败。Go 使用 `GOENV=off`、`GOWORK=off`、`GOTOOLCHAIN=local`、`GOFLAGS=-mod=readonly`；检查关闭模块代理与校验数据库，不自动补装依赖。环境按允许列表构造，外部代理、令牌、Go 设置及 runner 夹具变量不会传入工具。

扫描前执行离线 `go mod verify`，核对已准备模块缓存的完整性。缓存损坏属于操作失败，先保留日志，再重新准备对应缓存。

原生命令固定为 `govulncheck -db=https://vuln.go.dev -scan=symbol -test -tags=integration -format=json ./...`。它分析 Linux amd64、启用 cgo 的源码导入图，包含测试及 integration 标签，但不运行测试、runner 服务或 nft。范围包含该图使用的模块和标准库，不等于 `go list -m all` 的完整模块列表，也不涵盖其他平台或未选 build tags。报告保留可达函数、已导入包和仅模块层面的发现，不能把“无可达发现”解释为“所有依赖无漏洞”。工具的 module 模式要求当前目录有 Go 文件且不接受包 pattern，因此本入口不使用该模式。

扫描正常结束并收齐 stdout 后，标准库 helper 解码协议，核验 config、SBOM 和 finding 关联。随后将同一未修改 JSON 交给 `govulncheck -mode=convert -format=text -show=version,verbose`，保留原生文本退出 0 或 3，并核对可达发现是否一致。转换使用本地元数据，不再次在线扫描或查询数据库。文本先显示转换器配置，再显示原件中的扫描配置；后者才是本次扫描来源。空输入或 JSON 原生退出 0 均不能证明扫描通过。

CLI 退出 0 要求处置绑定有效且没有未处置的可达告警。退出 1 表示仍有未处置可达告警或处置绑定已失效；即使原生扫描为零发现，绑定失效也阻断。退出 2 表示输入、工具、数据库、协议、转换、取消、超时、并发变化或清理错误。扫描误差仍需结合调用方式与部署事实审查，报告不是可利用性的证明。官方数据库在线更新；相同源码在不同时间可能得到不同结果，原件包含工具、Go、数据库地址和更新时间。单独 `-version` 不执行扫描，也不证明数据库完整可用。

当前五项 LXD 发现按[源码适用性判断](go-vuln-lxd.md)与[准确记录](scripts/go-vuln-lxd.json)消费。记录绑定全部 Runner Go 源码、两锁、准确版本与 checksum、14 包集合、工具构建范围和五项 OSV 内容。变化会使判断失效；新增非 Go 编译或嵌入输入也会拒绝旧判断。原始告警与 native 3 保留，结果明确区分 source-not-affected 和未处置数量，不宣称没有发现或 daemon 安全。未知可达告警仍失败，模块/包层面的发现继续展示。判断变更须经领域核实和独立 PR 审查，不提供通用 ignore 或自动刷新。依赖修复和实际 daemon 调查各按其授权实施。

工具、模块与编译缓存位于忽略的 `.cache/designs-go-vuln/`。每次原件保存在 `run-*` 目录，包含编号日志、`source-manifest.json`、原生 `scan.json`、`packages.json` 和 `assessment.json`。最后一项分别记录 JSON 完成、原生文本退出码、处置与未处置结果；它是扫描阶段结论，最终 CLI 还须完成输入复核和清理。正常结束会删除本次 source/control 副本；取消或单命令十分钟超时先终止进程组，CLI 总期限十五分钟，完成有界清理后失败。若进程清理未知，保留副本并按日志核查原 PID/进程组，不直接重跑或删除现场。CI 上传 `go-vuln-<run_id>-<attempt>`，仅包含上述原件，保留三天，不上传源码副本、模块缓存或可执行文件。

恢复时先保留失败原件：缺工具、摘要错误或缺模块时重新运行准备；数据库或网络错误在确认原因后重试检查；源码并发变化先稳定工作树。取消不代表漏洞结果。工具升级时在独立缓存用固定 SDK 和同一构建参数至少构建两次，比较二进制 SHA-256，并核官方模块校验与 `go version -m`。在普通 PR 中一起更新唯一工具声明、Go 版本约束及本说明，重新运行准备、真实正常/命中/操作失败回归、类型、格式和链接检查。共享命令生命周期变化还须运行 `test:go-tools`。回退源码后按回退声明重新准备；不要修改摘要来接受来历不明的现有二进制。

## Go 检查

[原生 Go 工作流](.github/workflows/go.yml)与本地使用同一个入口。运行环境为满足以下条件的 Linux amd64，不是任意 Linux 宿主。Node/Bun 版本见根 manifest，完整 Go SDK 版本见 `runner/go.mod`。另准备 GCC、`dbus-daemon`、Docker daemon、`unshare` 和 `/usr/sbin/nft`。真实进程测试要求非根 unified cgroup。nft 测试要求允许创建私有 user/network namespace。缺条件会失败，不将 skip 解释为通过。本地命令不安装包、修改服务或安全策略。

完整测试的 `go` job 固定使用 GitHub hosted `ubuntu-22.04`，在一次性 job 中用 apt 准备系统工具。随后以普通用户执行 `unshare --user --map-root-user --net /bin/true` 预检，失败就停止。Ubuntu 24.04 及后续版本默认限制无特权 user namespace，不能假定 `ubuntu-latest` 满足上述条件。不要用 sudo 运行测试，也不要修改 sysctl 或 AppArmor 来绕过拒绝。升级 runner 环境时重新验证 module、container 和 nft 三层；旧环境或部分层成功不证明新环境完整可用。平台支持范围见 [GitHub hosted runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)，发行版限制见 [Docker rootless 故障说明](https://docs.docker.com/engine/security/rootless/troubleshoot/)。

Go SDK 下载与模块准备分开。先按 [runner 工具链说明](runner/README.md#构建与检查)验证官方归档摘要。完整解包到临时或忽略目录，再将 `bin` 加入当前 shell 的 `PATH`。不要用仅含 `go`/`gofmt` 的格式工具目录编译。随后在仓库根运行：

```sh
bun install --frozen-lockfile --ignore-scripts
bun run go:prepare
bun run typecheck
bun run test:go-tools
bun run go:check
```

`go:prepare` 通过官方模块代理和校验数据库下载锁定依赖。它还拉取脚本固定 digest 的 Ubuntu 26.04 Linux amd64 夹具镜像。准备成功不代表检查通过。检查阶段关闭 Go 网络代理与校验数据库。Docker 只消费已有镜像，不自动联网补装工具。Go 使用 `GOTOOLCHAIN=local`、`GOWORK=off`、`GOENV=off` 和 `GOFLAGS=-mod=readonly`，前后确认 `go.mod`/`go.sum` 未改变。继承的 `GO*` 设置与 `RUNNER_*`、`GUARD_*`、`WATCHDOG_STOP_*` 夹具开关会被清除，再按对应层设置。外部环境不能启用测试 helper 或指定它们的状态目录与 D-Bus。

默认 `go:check` 依次完成 module、container 和 nft 三层，全部通过才输出完整成功。module 要求非 root 宿主用户，运行 `go mod verify`、全模块 `go test -race -count=1`、`go vet` 和五个命令的 `CGO_ENABLED=0` Linux amd64 构建。测试使用真实私有 D-Bus。Go JSON 事件必须证明实际进程身份测试执行并通过。普通层仅允许数据库降权用例、installed 历史匹配用例及明确登记的 31 个 root 子用例 skip。installed 的其余五个父级用例必须 pass。没有测试文件的包由 `go list` 明确识别，未登记测试或包的 skip 均失败。必需测试必须恰好运行一次并给出预期终态，缺失或重复均失败。

container 层按 normal、race 顺序使用两个独立的一次性无网络容器。normal 静态产物执行原 [数据库降权用例](runner/closure.md#验证与后续边界)及全部 `TestInstalled*`；race 产物按 [installed 验证合同](runner/installed-evidence.md#未观察事实与验证)执行全部 `TestInstalled*`。两份事件分别要求六个 installed 父级及 31 个子用例真实 pass，normal 还必须证明数据库用例通过。normal 失败就结束，不启动 race 掩盖失败。

每个容器只读挂入本次 lifecycle 测试产物，再复制到可替换的 `/tmp/lifecycle.test`。容器中创建 UID 24567/GID 24568 的私有 NSS fixture。数据库目录初始不存在，不挂业务 socket、数据库或凭据。race 容器先用 `/lib64/ld-linux-x86-64.so.2 --list` 核动态依赖，再执行真实用例。缺 loader/libc 时保留失败，不换镜像或联网补装。日志由 Go `test2json` 转成事件验证。nft 层沿 [namespace 测试](runner/README.md#构建与检查)在内部执行 `unshare`。核实际父进程及自身的 user/network namespace 均已隔离后，才构造私有网络和调用 nft。该层需要 nft、iproute2 和 util-linux，不在宿主加载规则，也不运行 runner 服务或安装 unit。

编译缓存和日志放在忽略的 `runner/.build/`。测试使用本次创建的短真实 `/tmp/dg-*` 目录。Go 1.27 的 `testing.T.TempDir` 优先使用 `GOTMPDIR`。`go test -exec` 仅对测试进程切换到短路径，大编译目录仍留在工作树。子进程使用 umask `022`，让现有夹具的准确权限合同成立。CLI 只在同步 spawn 窗口设置并立即恢复父进程 umask，不改变宿主配置。日志与容器句柄显式使用 `0600`。

恢复或分权限运行时，可执行 `bun run go:check --stage=module`、`--stage=container` 或 `--stage=nft`。每次只对指定层声明部分结果。首次交付或串联入口变化时验证默认全入口。后续接纳可合并同候选和适用输入下仍有效的三层证据，不重跑未变层。默认 CLI 仅在本次完整执行通过时打印完整成功。未知、空白或重复选择直接失败。`test:go-tools` 包含有界的真实子进程取消与私有 Docker 清理回归，要求先完成工具和镜像准备。

CLI 为每条命令保留日志。准备总期限为 20 分钟，检查为 30 分钟；到期后仍执行有界清理。模块测试先用既有 Go SDK 构建仓库内的 `scripts/go-supervise.go`，产物留在本次工作目录。这个 Linux subreaper 托管唯一 `go test`，保持原 PID、UID、cgroup 与 `/proc` 视图。测试创建独立进程组或 session 后，孤儿后代仍归这个托管进程回收。只有 `wait4` 返回 `ECHILD` 才证明子孙已收齐；自身 task 的 children 列表为空不证明结束。目标正常退出但留下活后代也判失败，并继续清理。

取消时托管进程先向自己的直接子进程发送 TERM，两秒后按需 KILL。读取 children 到发信号之间不并发 reap，避免 PID 被重用。八秒内无法收齐就保留 unknown。Node 给托管进程更长的十二秒清理窗口；最终强杀不证明清理完成。专用 pipe 只交给托管进程，回执另存为 `*.supervisor.log`。回执缺失、异常、清理未闭合或强杀时保留短临时目录，不用外层进程组消失补判成功。工具版本、编译等命令沿独立进程组清理；超时或取消时先 TERM，再按需 KILL。

容器随机名称在 create 前写入本次工作目录；create/start 不自动重发。清理沿原名称与 ID 读取状态，确认终态后先保存日志，再删除并核名称与 ID 均消失。wait 失败或取消时仍以有界、不可取消的读取保全容器日志。原执行错误与日志读取错误分别保留；日志读取失败不跳过容器清理。create 结果未知且暂未查到对象时保留 unknown，不能宣称清理完成。删除客户端报错后仍回读原对象，确认已移除才结束。

检查失败先读日志并修复对应输入或环境，再决定重跑范围。CLI 会输出有界的错误原因及原日志路径。CI 在 run 的 Artifacts 中保留 `go-check-<run_id>-<attempt>` 三天。内容为本次日志、容器句柄和测试输出，不上传 SDK、模块缓存或二进制。normal/race 原件分别使用 `container-normal-name.txt`、`container-race-name.txt` 及各自的 `container-<variant>-test-output.txt`，不会相互覆盖。取消会在清理后返回失败。强制 SIGKILL、daemon 不可达或清理超时可能留下未知结果。保留日志和对应名称文件，沿原名称查询，不重新 create。确认原容器终态后处理该对象；不要用删日志、换名称或启动第二份检查绕过未知结果。源码修复与依赖升级通过普通 PR 完成，原操作材料保留。完整 Go 通过仍不代表生产部署、文档结构或安全扫描已验收。
