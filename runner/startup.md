# Guard 正常启动准入

`guarded-provider --check-startup` 为 controller 的 `ExecStartPre` 提供正常启动校验。本片使用标准库和 `golang.org/x/sys/unix v0.49.0`。程序只读取配置、native provider 和 guard 记录。检查成功退出 `0`，操作锁忙退出 `75`，其他拒绝退出 `1`。失败日志仅包含固定事件和阶段，不输出路径内容、原始错误或秘密。

## 启动条件

正常启动入口为 `--check-startup`。无参数和 provider 命令均拒绝；同一二进制另有独立的 [watchdog 检查](guard-watchdog.md)。真实和有效 UID 必须匹配本机 `nucleiron` 用户，有效 GID 匹配其主组；root 调用也拒绝。正常启动不执行 native provider，不访问 GARM、GitHub、LXD 或 systemd API。

| 输入 | 校验与含义 |
| --- | --- |
| `/etc/nucleus-halo/provider-policy.json` | root 所有、`0644`、单链接普通文件，上限 64 KiB；必填 `provider_sha256` 和 `config_sha256`，均为小写 SHA256；其他字段归对应消费者校验 |
| `/opt/nucleus-halo/bin/garm-provider-lxd` | root 所有、`0755`、单链接普通文件；流式计算原字节摘要并与 policy 对比，不执行程序 |
| `/etc/nucleus-halo/garm-provider-lxd.toml` | root 所有、`0644`、单链接普通文件；核原字节摘要，本片不解析或重写 TOML |
| `/var/lib/nucleiron/provider-guard` | 服务用户所有、`0700` 的真实目录；固定目录描述符用于读取锁和状态 |
| `operation.lock` | 预先存在，服务用户所有、`0600`、单链接普通文件；非阻塞排他锁覆盖完整检查，不创建或替换锁文件 |
| `state.json` | 服务用户所有、`0600`、单链接普通文件，上限 64 KiB；准确版本 `1`，必填 `active/intent/latch`，不接受未知字段 |
| `/run/nucleiron-records-only` | 任何现存条目都拒绝，包括目录、文件、FIFO 和悬空链接；其他读取错误也拒绝，不读取许可或调用维护校验器 |

policy 与 state 的大小上限用于限制本地输入内存。文件类型、owner、权限位和硬链接数从打开的描述符核验，特殊权限位也拒绝。`openat2` 拒绝路径各级符号链接；状态文件通过固定目录的 `openat` 读取。宿主要求 Linux 5.6 或更新内核并允许这些系统调用；不支持时明确拒绝，不降级为宽松路径读取。

JSON 对象拒绝重复键和大小写别名，防止有值的意图被后一个空值覆盖。`intent` 与 `latch` 只有显式 `null` 才通过。空对象、`false`、缺失字段和不支持的版本均拒绝，不修改记录解释结果。

存在 `state.pending` 或 `state.new` 时拒绝，保留 watchdog 中断写入的原事务材料，不自动删除或恢复。

`active` 允许显式 `null` 或有效的当前实例记录。记录包含实例名、VM UUID、正 Unix 秒时间及 `ready` 布尔值。已 ready 的记录另有正 `int64` agent ID；尚未 ready 的记录没有 agent ID。已有 active 允许 controller 正常重启，继续占用容量。本检查不宣称资源闭合，也不授权创建下一台 VM。

维护标记在检查前、取得锁后和返回成功前分别核对。摘要检查核文件是否被替换及读取期间的大小、修改时间变化。返回前核状态目录、锁和状态文件仍指向原 inode。所有合法状态写者仍须持有同一永久锁；部署修改配置、文件或维护模式时先停接并使用独占维护窗口。退出后锁释放，启动检查不提供覆盖后续 controller 生命周期的锁。

## 消费与部署边界

[30-provider-guard.conf](templates/30-provider-guard.conf) 保留原挂载依赖和 `lxd` 附加组。切换时作为 `nucleiron.service.d/30-provider-guard.conf` 的完整替换文件消费，将该文件中的 `ExecStartPre` 指向新 Go 入口。核对其他 drop-in，避免遗留启动校验重复执行；不清空其他能力拥有的启动条件。

本片不修改 GARM external provider 的执行路径。这个二进制提供正常启动和 watchdog 检查，仍不能接管 provider 请求，不覆盖旧 `guarded-lxd-provider`。完整运行切换等待 Go provider 及部署验收；不恢复已停止的 Python 流程，也不建立 Python fallback。

部署能力准备已有身份绑定的 policy、native 配置、guard 状态与永久锁。缺文件时启动检查失败，不自动初始化或修复状态。已有意图、latch、维护许可和未知操作保持原字节及恢复责任。旧操作由固定原恢复程序解释，不通过迁移记录或删除标记放行。

## 检查与来源

沿用 [构建环境](README.md#构建与检查)准备固定工具链与依赖。在 `runner/` 运行：

```sh
go test -race -count=1 ./internal/provider
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o .build/guarded-provider ./cmd/guarded-provider
```

测试使用当前测试用户拥有的临时文件，独立绑定预期 owner。覆盖清洁及 active 状态、摘要漂移、错误身份与权限、链接/FIFO、畸形 JSON、维护标记和原件不变。跨进程测试以管道同步持锁和释放，验证忙拒绝与再次检查。路径替换测试核固定描述符仍识别旧 inode。实际 CLI 检查非法参数和非服务用户拒绝，不启动真实 controller。

行为来源是保全的 `vm-restore-guard-pin-20261026/guarded-lxd-provider.py` 正常启动分支，以及 `provider.py` 的 policy/state 与 drop-in 生成链。沿用正常启动拒绝 `intent/latch`、保留 active 占用的含义。本片对全部维护模式拒绝；旧 records-only 许可不会被读取、转换或接管。离线检查不证明宿主安装、真实 controller 启动或完整 VM 生命周期已验收。
