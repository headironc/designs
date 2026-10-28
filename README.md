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

## Go 检查

[原生 Go 工作流](.github/workflows/go.yml)与本地使用同一个入口。运行环境为满足以下条件的 Linux amd64，不是任意 Linux 宿主。Node/Bun 版本见根 manifest，完整 Go SDK 版本见 `runner/go.mod`。另准备 GCC、`dbus-daemon`、Docker daemon、`unshare` 和 `/usr/sbin/nft`。真实进程测试要求非根 unified cgroup。nft 测试要求允许创建私有 user/network namespace。缺条件会失败，不将 skip 解释为通过。本地命令不安装包、修改服务或安全策略。

Go 工作流固定使用 GitHub hosted `ubuntu-22.04`，在一次性 job 中用 apt 准备系统工具。随后以普通用户执行 `unshare --user --map-root-user --net /bin/true` 预检，失败就停止。Ubuntu 24.04 及后续版本默认限制无特权 user namespace，不能假定 `ubuntu-latest` 满足上述条件。不要用 sudo 运行测试，也不要修改 sysctl 或 AppArmor 来绕过拒绝。升级 runner 环境时重新验证 module、container 和 nft 三层；旧环境或部分层成功不证明新环境完整可用。平台支持范围见 [GitHub hosted runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)，发行版限制见 [Docker rootless 故障说明](https://docs.docker.com/engine/security/rootless/troubleshoot/)。

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

每个容器只读挂入本次 lifecycle 测试产物，再复制到可替换的 `/tmp/lifecycle.test`。容器中创建 UID 24567/GID 24568 的私有 NSS fixture。数据库目录初始不存在，不挂业务 socket、数据库或凭据。race 容器先用 `/lib64/ld-linux-x86-64.so.2 --list` 核动态依赖，再执行真实用例。缺 loader/libc 时保留失败，不换镜像或联网补装。日志由 Go `test2json` 转成事件验证。nft 层沿 [原 namespace 测试](runner/README.md#构建与检查)在内部执行 `unshare`。核网络 namespace 已改变后才调用 nft。它不在宿主加载规则，也不运行 runner 服务或安装 unit。

编译缓存和日志放在忽略的 `runner/.build/`。测试使用本次创建的短真实 `/tmp/dg-*` 目录。Go 1.27 的 `testing.T.TempDir` 优先使用 `GOTMPDIR`。`go test -exec` 仅对测试进程切换到短路径，大编译目录仍留在工作树。子进程使用 umask `022`，让现有夹具的准确权限合同成立。CLI 只在同步 spawn 窗口设置并立即恢复父进程 umask，不改变宿主配置。日志与容器句柄显式使用 `0600`。

恢复或分权限运行时，可执行 `bun run go:check --stage=module`、`--stage=container` 或 `--stage=nft`。每次只对指定层声明部分结果。首次交付或串联入口变化时验证默认全入口。后续接纳可合并同候选和适用输入下仍有效的三层证据，不重跑未变层。默认 CLI 仅在本次完整执行通过时打印完整成功。未知、空白或重复选择直接失败。`test:go-tools` 包含有界的真实子进程取消与私有 Docker 清理回归，要求先完成工具和镜像准备。

CLI 为每条命令保留日志。准备总期限为 20 分钟，检查为 30 分钟；到期后仍执行有界清理。模块测试先用既有 Go SDK 构建仓库内的 `scripts/go-supervise.go`，产物留在本次工作目录。这个 Linux subreaper 托管唯一 `go test`，保持原 PID、UID、cgroup 与 `/proc` 视图。测试创建独立进程组或 session 后，孤儿后代仍归这个托管进程回收。只有 `wait4` 返回 `ECHILD` 才证明子孙已收齐；自身 task 的 children 列表为空不证明结束。目标正常退出但留下活后代也判失败，并继续清理。

取消时托管进程先向自己的直接子进程发送 TERM，两秒后按需 KILL。读取 children 到发信号之间不并发 reap，避免 PID 被重用。八秒内无法收齐就保留 unknown。Node 给托管进程更长的十二秒清理窗口；最终强杀不证明清理完成。专用 pipe 只交给托管进程，回执另存为 `*.supervisor.log`。回执缺失、异常、清理未闭合或强杀时保留短临时目录，不用外层进程组消失补判成功。工具版本、编译等命令沿独立进程组清理；超时或取消时先 TERM，再按需 KILL。

容器随机名称在 create 前写入本次工作目录；create/start 不自动重发。清理沿原名称与 ID 读取状态，确认终态后先保存日志，再删除并核名称与 ID 均消失。wait 失败或取消时仍以有界、不可取消的读取保全容器日志。原执行错误与日志读取错误分别保留；日志读取失败不跳过容器清理。create 结果未知且暂未查到对象时保留 unknown，不能宣称清理完成。删除客户端报错后仍回读原对象，确认已移除才结束。

检查失败先读日志并修复对应输入或环境，再决定重跑范围。CLI 会输出有界的错误原因及原日志路径。CI 在 run 的 Artifacts 中保留 `go-check-<run_id>-<attempt>` 三天。内容为本次日志、容器句柄和测试输出，不上传 SDK、模块缓存或二进制。normal/race 原件分别使用 `container-normal-name.txt`、`container-race-name.txt` 及各自的 `container-<variant>-test-output.txt`，不会相互覆盖。取消会在清理后返回失败。强制 SIGKILL、daemon 不可达或清理超时可能留下未知结果。保留日志和对应名称文件，沿原名称查询，不重新 create。确认原容器终态后处理该对象；不要用删日志、换名称或启动第二份检查绕过未知结果。源码修复与依赖升级通过普通 PR 完成，原操作材料保留。完整 Go 通过仍不代表生产部署、文档结构或安全扫描已验收。
