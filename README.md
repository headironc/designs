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

工作流在 PR 和 `main` push 上运行格式、类型与工具回归，仅持源码读取权限。当前使用 GitHub hosted `ubuntu-latest`；真实调度结果与 required checks 由平台交付另行核验。格式通过不代表完整 Go、文档结构或安全检查通过。依赖准备失败时保留原错误，核对工具版本、网络和锁文件；用相同锁重新准备后再检查，不通过删除检查或修改锁文件掩盖失败。工具或配置修复使用普通 PR，并保持 manifest 与锁文件一致；回退同样恢复配套源码、配置和锁。

旧执行器与配方的准确版本保留在 Git 历史。原操作和未知结果的恢复继续使用原身份与原材料，不通过恢复旧门禁或兼容桥接实现。

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

`go:prepare` 通过官方模块代理和校验数据库下载锁定依赖。它还拉取脚本固定 digest 的 Ubuntu 26.04 Linux amd64 夹具镜像。准备成功不代表检查通过。检查阶段关闭 Go 网络代理与校验数据库。Docker 只消费已有镜像，不自动联网补装工具。Go 使用 `GOTOOLCHAIN=local`、`GOWORK=off`、`GOENV=off` 和 `GOFLAGS=-mod=readonly`，前后确认 `go.mod`/`go.sum` 未改变。继承的 `GO*` 设置与 `RUNNER_*` 夹具开关会被清除，再按对应层设置，避免将私有容器用例带到宿主。

默认 `go:check` 依次完成 module、container 和 nft 三层，全部通过才输出完整成功。module 运行 `go mod verify`、全模块 `go test -race -count=1`、`go vet` 和五个命令的 `CGO_ENABLED=0` Linux amd64 构建。测试使用真实私有 D-Bus。Go JSON 事件必须证明实际进程身份测试执行并通过。普通层唯一允许跳过的测试是另行隔离运行的容器用例。没有测试文件的包由 `go list` 明确识别，其他测试或包的 skip 均失败。必需测试必须恰好运行一次并给出预期终态，缺失或重复均失败。

container 层使用静态 lifecycle 测试产物。只读挂入一次性无网络容器后，复制到可替换的 `/tmp/lifecycle.test`。容器中创建 UID 24567/GID 24568 的私有 NSS fixture。数据库目录初始不存在，不挂业务 socket、数据库或凭据。原 [降权用例](runner/closure.md#验证与后续边界)必须真实通过，日志由 Go `test2json` 转成事件验证。nft 层沿 [原 namespace 测试](runner/README.md#构建与检查)在内部执行 `unshare`。核网络 namespace 已改变后才调用 nft。它不在宿主加载规则，也不运行 runner 服务或安装 unit。

编译缓存和日志放在忽略的 `runner/.build/`。测试使用本次创建的短真实 `/tmp/dg-*` 目录。Go 1.27 的 `testing.T.TempDir` 优先使用 `GOTMPDIR`。`go test -exec` 仅对测试进程切换到短路径，大编译目录仍留在工作树。子进程使用 umask `022`，让现有夹具的准确权限合同成立。CLI 只在同步 spawn 窗口设置并立即恢复父进程 umask，不改变宿主配置。日志与容器句柄显式使用 `0600`。

恢复或分权限运行时，可执行 `bun run go:check --stage=module`、`--stage=container` 或 `--stage=nft`。每次只对指定层声明部分结果。首次交付或串联入口变化时验证默认全入口。后续接纳可合并同候选和适用输入下仍有效的三层证据，不重跑未变层。默认 CLI 仅在本次完整执行通过时打印完整成功。未知、空白或重复选择直接失败。`test:go-tools` 包含有界的真实子进程取消与私有 Docker 清理回归，要求先完成工具和镜像准备。

CLI 为每条命令保留日志，使用独立进程组。准备总期限为 20 分钟，检查为 30 分钟；到期后仍执行有界清理。超时或收到 SIGINT/SIGTERM 时先发 TERM，再按需 KILL，并核组已结束。容器随机名称在 create 前写入本次工作目录；create/start 不自动重发。清理沿原名称与 ID 读取状态，确认停止后删除，再核名称与 ID 均消失。create 结果未知且暂未查到对象时保留 unknown，不能宣称清理完成。删除客户端报错后仍回读原对象，确认已移除才结束。

检查失败先读日志并修复对应输入或环境，再决定重跑范围。CLI 会输出有界的错误原因及原日志路径。CI 在 run 的 Artifacts 中保留 `go-check-<run_id>-<attempt>` 三天。内容为本次日志、容器句柄和测试输出，不上传 SDK、模块缓存或二进制。取消会在清理后返回失败。强制 SIGKILL、daemon 不可达或清理超时可能留下未知结果。保留日志和 `container-name.txt`，沿原名称查询，不重新 create。确认原容器终态后处理该对象；不要用删日志、换名称或启动第二份检查绕过未知结果。源码修复与依赖升级通过普通 PR 完成，原操作材料保留。完整 Go 通过仍不代表生产部署、文档结构或安全扫描已验收。
