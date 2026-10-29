# Runner Go 实现

已交付专用防火墙重载、[guest bootstrap](bootstrap.md)、[provider 异常停接](watchdog.md)、[Go guard 正常启动准入](startup.md)、[guard watchdog 检查与状态持久化](guard-watchdog.md)、[单次 VM 创建](create.md)、[VM 删除与容量释放](delete.md)、[provider 正常只读协议](read.md)、[原执行身份采集](execution.md)、[闭合诊断](closure.md)、[watchdog 暂停](pause.md)、[gateway 停止](gateway.md)、[历史 prepare 原件观察](prepare-evidence.md)和 [当前安装文件比对](installed-evidence.md)。本片新增 [GARM 管理会话窗口](session.md)，覆盖认证域轮换、单次登录、受管消费和到期退役。异常停接已接入 GARM client 与 go-systemd，包含原停止 job 恢复。诊断分别报告 job 结论、资源事实和证据缺口，不签发清理许可。已采用的 Go 包及后续接入边界见 [架构选型](architecture.md#已采用的-go-包)。防火墙入口使用 [原生 nftables 事务](firewall.md)；源码验收与实际服务切换分别执行。

`runner-firewall` 供 `nucleiron-firewall.service` 的启动与重载使用。零参数入口读取 root 所有、模式 `0600` 的 `/etc/nucleiron-gateway/firewall.json`。纯 `render-config` 子命令输出当前版本的非敏感 JSON；服务不接受 nft 文本或 CLI fallback。

程序通过 `google/nftables` 在单次 batch 中删除并重建 `inet nucleiron` 和 `bridge nucleiron`。配置只允许既有 bridge、IPv4 子网、gateway 和四个 TCP 端口，不能由工作流或 guest 提供。新实现保留最终网络策略、域名及路由；其他表保持。永久锁、原意图、读回及归档由 `/var/lib/nucleiron-firewall` 内的独立记录维护。

[GitHub 只读观察凭据](github-observer.md)为 capture、diagnose、pause 和 gateway 提供单次 installation token 供应、受管读取及原 token 退役恢复。入口消费已有 GARM 会话，不再次登录；源码与隔离检查不执行真实凭据或接入操作。

## 构建与检查

仓库完整验收使用根目录的 [Go 检查入口](../README.md#go-检查)。本地与原生 CI 运行相同三层检查，包含 race、五个命令构建、私有降权容器和 nft namespace。下文保留各能力的底层命令及运行边界。

当前自有 module 使用标准库、x/sys、GARM client、LXD client、go-github、go-systemd、modernc SQLite、BurntSushi TOML、google/nftables 和 mdlayher/netlink；版本由 `go.mod` / `go.sum` 固定，其余目标依赖随对应实施片接入。Linux amd64 构建固定使用 [Go 1.27.3](https://go.dev/dl/)，即本片核定时的最新稳定版。官方工具链归档 SHA256 为 `65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40`。先下载并验证工具链，再准备固定依赖。provider、watchdog 和 lifecycle 的私有 D-Bus 测试还需 `dbus-daemon`，不连接宿主 system bus。闭合诊断的实际降权用例另需 [一次性私有容器](closure.md#验证与后续边界)，普通测试的 skip 不证明该项通过。

首次在本目录联网准备缓存，使用 Go 官方模块代理与校验数据库：

```sh
export GOTOOLCHAIN=local GOWORK=off
export GOCACHE="$PWD/.build/go-cache" GOMODCACHE="$PWD/.build/go-mod"
GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org go mod download
go mod verify
```

随后在本目录离线运行：

```sh
export GOTOOLCHAIN=local GOWORK=off GOPROXY=off GOSUMDB=off
export GOCACHE="$PWD/.build/go-cache" GOMODCACHE="$PWD/.build/go-mod"
go test ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o .build/runner-firewall ./cmd/runner-firewall
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o .build/runner-admin ./cmd/runner-admin
```

普通测试使用 native netlink 替身和受保护文件 fixture，核对单批范围、原协议字节、锁、同步失败、内核 NACK 与 ACK 丢失。测试不连接宿主 netfilter。ACK 丢失恢复只读取原标记与完整策略，不再次 Flush。

Linux 的实际 nft 测试另用一次性的用户和网络 namespace。宿主必须支持无特权 user namespace，并已安装 nft、iproute2 和 util-linux。namespace 创建被拒绝时，测试命令失败，实际 nft 验证未运行。保留该失败，不用 sudo 绕过。

```sh
RUNNER_FIREWALL_NAMESPACE=1 go test -count=1 -tags=integration \
  -run '^TestReloadInPrivateNamespace$' ./internal/firewall
```

测试通过 `unshare --user --map-root-user --net` 启动子进程。父进程在隔离前打开自己的 namespace 描述符；子进程核描述符类型及原身份，证明 user 和 network 均已隔离，不依赖跨 user namespace 的 `/proc/<parent>/ns` 读取权限。三个额外 network namespace 构造 guest、peer 和外侧转发端点；先证明无过滤时全部探测路径可达，再比较首次原生加载、最终旧规则和原生替换。fixture 的两条路由接口各有固定 `nodad` 链路本地地址，供转发报文立即发起 IPv6 邻居发现；自动地址可能尚在 DAD 阶段。覆盖 IPv4/IPv6、DHCP、DNS、四个 TCP 端口、错误源/目标及双向转发，并核原子拒绝和其他表保持。namespace 随测试进程结束回收，不进入生产网络 namespace。测试编译或替身通过不证明真实内核验收通过。

## 服务消费与切换

[nucleiron-firewall.service](templates/nucleiron-firewall.service) 保留原 unit 的依赖、启动顺序和停止语义。`ExecStart` 与 `ExecReload` 消费 `/opt/nucleus-halo/bin/runner-firewall`，新增 root `0700` 状态目录及 `0077` umask。停止 unit 仍保留已有防护规则；不增加自动重启或自动重试。

发布成品由 Go 二进制、配置生产能力、该 unit 模板及对应源码提交与摘要组成。当前构建与测试不安装 unit，不执行 daemon-reload，也不加载生产规则。切换时由准确宿主计划核当前 unit、二进制、规则与旧操作，再安装固定 JSON 和成品并验证实际消费。旧 `.nft` 文件保留为原件，不作为新入口的输入。

新入口在发送前验证配置与前态，先持久化原意图，再发送一次 netlink batch。取消或回执丢失保留 unknown；恢复仅核原操作。具体记录、已知结果与下一次显式 reload 的边界见 [事务恢复](firewall.md#事务与恢复)。

本片不能代替完整 runner 的 Go 接续。provider、凭据、连接、VM 生命周期及旧未知操作仍按 [结构与实施切片](architecture.md) 分别交付。维护窗口的进入、写入和退出条件见 [records-only 恢复协议](records-recovery.md)，对应 Go 运行能力尚未实现。现有 Python 恢复现场保全；本片不重启原操作。

## 行为来源

本片按已保存的原安装调用链核对，未复制历史运行包。`network.py` 生成的 unit 明确消费 `network-firewall.py`，后者先检查再提交 nft batch。

| 原源码              | SHA256                                                             |
| ------------------- | ------------------------------------------------------------------ |
| network-firewall.py | `2d542bcad00f5624306c597bb3c063629ed81f5eaf0294d2be7e90fbdc20e693` |
| network.py          | `27eef430978620bf8d80c89c9ec49e1b1335012c4d609f67b724ac9bbf181fc6` |

原件位于接入 owner 保留的 `halo-20261026` 来源包。来源摘要用于行为追溯，不是执行授权。历史快照和其余恢复脚本不进入本片。
