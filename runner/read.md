# Provider 正常只读协议

`guarded-provider` 无参数运行时接受 `GetVersion`、`GetInstance` 和 `ListInstances`。正常查询成功退出 `0`，永久操作锁忙退出 `75`，其余拒绝退出 `1`。这三个入口只返回经过核验的 native 响应，不写 guard 状态，也不创建或删除 VM。

## 调用与准入

沿用[正常启动](startup.md)的服务身份、受保护文件、native/config 摘要及事务残留检查。读取期间持有同一个永久操作锁。维护标记、未结 intent、任何 latch、非法状态或事务残留均拒绝。active 继续占用容量，ready 和 agent ID 保留原值；ready 回执的持久化仍由 [watchdog](guard-watchdog.md) 负责。

三个命令都核对 controller、固定配置路径和 `v0.1.0` 接口版本。`ListInstances` 要求 policy 的准确 pool；其他命令不要求 pool，但提供时也须匹配。`GetInstance` 要求已有合法 active，且 `GARM_INSTANCE_ID` 与记录中的名称一致。其他命令不接受实例名。

native 只收到对应命令、controller 和必要的 pool 或实例名，以及固定描述符的配置路径。不继承额外环境，不读取调用方 stdin。复用创建和删除的摘要绑定、进程组、取消后等待与输出上限：native 最长 300 秒，完整入口最长 330 秒，stdout 最多 2 MiB。native 失败或进程组未结束时，返回拒绝；本入口不自动重试。

## 响应与实际身份

`GetVersion` 沿用 provider-common 的纯文本版本协议。返回值去掉外围空白后须为非空、可打印 ASCII 单个 token；不将 native 版本改写成 guard 版本，也不强制解释为 JSON 或比较版本顺序。原始响应字节保留。

`GetInstance` 响应为一个对象，`ListInstances` 响应为非 null 数组。实例对象使用 `provider-common/params.ProviderInstance` 解码，拒绝顶层重复键、大小写别名、未知字段和尾随数据。名称和 provider ID 均须与 active 一致，系统为 Linux/amd64；状态复用 GARM 的 `IsValidProviderStatus`，允许 running、stopped、error 和 unknown。正常读取可以报告实例故障，不把故障状态改写成成功运行。

已有 active 时，列表必须恰好含该实例。LXD client 再核对 `halo-ci` project 中的名称、VM UUID、controller/pool、镜像摘要、唯一 profile 和 VM 类型，并拒绝运行中或未知状态的 operation。同名替换 VM 与原 UUID 不符时拒绝，不能借查询重新绑定 active。

没有 active 时，仅允许返回空列表。guard 还核对专用 project 无实例、storage pool 只剩镜像卷、可见 operation 均为已知终态。native 过滤后的空列表不能掩盖实际残留。记录存在而列表为空、无记录而出现实例、重复或多实例结果都拒绝，不清记录、不释放容量、不补造恢复成功。

全部核验完成后才把原始响应交给 GARM。拒绝时 stdout 没有响应，诊断只包含固定事件与阶段。返回管道失败也不会改变 guard 记录。永久锁协调受管调用；专用 project 的部署边界仍要求不存在绕过 guard 的并发写者。

## 验收与边界

沿用[固定工具链和缓存](README.md#构建与检查)，在 `runner/` 执行：

```sh
go test -race -count=1 ./internal/provider
go vet ./internal/provider ./cmd/guarded-provider
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o .build/guarded-provider ./cmd/guarded-provider
```

状态测试覆盖三个命令、未 ready 与已 ready、错误身份、维护与未结操作、非法响应、空清单与残留冲突、取消和失败后原记录保持。真实子进程替身验证三个命令的最小环境、空 stdin 和固定描述符。私有 Unix HTTP 服务通过实际 LXD SDK 验证记录 UUID、归属及 operation 状态；创建和删除的共享执行失败、事务与资源回归同时运行。

本片完成正常模式读取，不提供 records-only、清记录或历史记录迁移。GARM 已安装的 external provider 路径保持不变。完整运行切换仍需剩余能力和宿主验收；源码检查不访问真实 LXD，不启用 ScaleSet，不部署服务。
