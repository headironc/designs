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
