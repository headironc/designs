# Runner Go 实现

已交付专用防火墙重载、[guest bootstrap](bootstrap.md) 和 [provider 异常停接](watchdog.md)。下一实现片为 Go guard 正常启动准入。已采用的 Go 包及后续接入边界见 [架构选型](architecture.md#已采用的-go-包)。以下描述当前已合入的 firewall；目标 nftables 包实现尚未接入。

`runner-firewall` 供 `nucleiron-firewall.service` 的启动与重载使用。它用 `/usr/sbin/nft` 列出现有表，读取 `/etc/nucleiron-gateway/isolated-network.nft`，检查后应用同一份规则字节。

程序只为已存在的 `inet nucleiron` 和 `bridge nucleiron` 表追加删除语句。规则文件沿用宿主的准确配置，不能由工作流或 guest 提供。本片不修改规则内容、端口、域名或路由。规则文件本身仍是受信任的部署输入，程序没有新增 nft 规则语言解析器。

## 构建与检查

当前自有 module 只依赖 Go 标准库，目标依赖随对应实施片加入 `go.mod` 和 `go.sum`。Linux amd64 构建固定使用 [Go 1.27.3](https://go.dev/dl/)，即本片核定时的最新稳定版。官方工具链归档 SHA256 为 `65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40`。先下载并验证工具链，再运行检查。引入外部包的实施片同时提供固定依赖的下载、校验与缓存准备步骤；下列检查命令保持离线。

在本目录运行：

```sh
export GOTOOLCHAIN=local GOWORK=off GOPROXY=off GOSUMDB=off
export GOCACHE="$PWD/.build/go-cache" GOMODCACHE="$PWD/.build/go-mod"
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o .build/runner-firewall ./cmd/runner-firewall
```

普通测试使用实际子进程替身核对 nft 的参数、标准输入、错误及调用次数。测试不调用宿主 nft。规则在检查期间变化时，应用仍使用检查过的字节；检查失败不进入应用，应用失败不自动重试。

Linux 的实际 nft 测试另用一次性的用户和网络 namespace。宿主必须支持无特权 user namespace，并已安装 nft。namespace 创建被拒绝时，测试命令失败，实际 nft 验证未运行。保留该失败，不用 sudo 绕过。

```sh
RUNNER_FIREWALL_NAMESPACE=1 go test -count=1 -tags=integration \
  -run '^TestReloadInPrivateNamespace$' ./internal/firewall
```

测试通过 `unshare --user --map-root-user --net` 启动子进程。子进程确认自己的网络 namespace 不同于父进程后，才创建测试表。验证首次加载、替换旧表、检查失败时规则不变，以及其他名称和 family 的表保持。namespace 随测试进程结束回收，不进入生产网络 namespace。

## 服务消费与切换

[nucleiron-firewall.service](templates/nucleiron-firewall.service) 保留原 unit 的依赖、启动顺序和停止语义。仅将 `ExecStart` 与 `ExecReload` 从 Python 入口改为 `/opt/nucleus-halo/bin/runner-firewall`。停止 unit 仍保留已有防护规则；不增加自动重启或自动重试。

发布成品由 Go 二进制、该 unit 模板及对应源码提交与摘要组成。当前构建与测试不安装 unit，不执行 daemon-reload，也不加载生产规则。切换时由准确宿主计划核当前 unit、二进制、规则与旧操作，再安装到上述固定路径并验证实际消费。

`nft --check` 和应用是两个调用，系统状态可能在两者之间变化。检查通过不保证应用成功；应用以 nft 的单次事务完成。非零退出保留失败阶段与原因；应用超时、取消或结果丢失后，先检查实际表状态，不假定未生效，不自动重发。

本片不能代替完整 runner 的 Go 接续。provider、凭据、连接、VM 生命周期及旧未知操作仍按 [结构与实施切片](architecture.md) 分别交付。现有 Python 恢复现场保全；本片不重启原操作。

## 行为来源

本片按已保存的原安装调用链核对，未复制历史运行包。`network.py` 生成的 unit 明确消费 `network-firewall.py`，后者先检查再提交 nft batch。

| 原源码 | SHA256 |
| --- | --- |
| network-firewall.py | `2d542bcad00f5624306c597bb3c063629ed81f5eaf0294d2be7e90fbdc20e693` |
| network.py | `27eef430978620bf8d80c89c9ec49e1b1335012c4d609f67b724ac9bbf181fc6` |

原件位于接入 owner 保留的 `halo-20261026` 来源包。来源摘要用于行为追溯，不是执行授权。历史快照和其余恢复脚本不进入本片。
