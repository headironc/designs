# VM 删除与容量释放

`guarded-provider` 无参数运行时按 `GARM_COMMAND=DeleteInstance` 处理单次删除。成功退出 `0`，永久操作锁忙退出 `75`，其余拒绝退出 `1`。入口不提供批量删除、清记录或自动恢复；删除与[单次创建](create.md)使用同一个永久锁和状态事务。

## 准确目标与准入

沿用[正常启动](startup.md)的服务身份、受保护文件、native/config 摘要和事务残留检查。维护标记或原 intent 存在时拒绝。请求名称必须与合法 `active` 一致；没有 active 或目标已消失时，不能补造删除成功。

GARM `v0.1.0` 删除协议从 `GARM_INSTANCE_ID` 取得目标，没有 stdin 请求体，也不要求 pool 环境变量。controller、接口版本和配置路径按 policy 核对；若调用方提供 pool，值必须匹配。native 只收到删除所需环境、准确实例名和空 stdin，不继承调用方其他变量。

LXD client 在 `halo-ci` project 核对名称、VM UUID、controller/pool、镜像摘要、唯一 `halo-ci` profile 和 virtual-machine 类型，并拒绝未结束或未知状态的 operation。UUID 必须与原 active 完全相同，同名替换实例不能删除。已有合法 ready/agent ID 作为原 bootstrap 完成事实保留；尚未 ready 时，复用 watchdog 的回执核验，取得有效 agent ID 后才允许删除。失败 bootstrap 的 VM 保留，另由原恢复责任处理。

已有 latch 仅在明确 `blocks_delete:false` 时允许继续核验删除；缺省、null、true 或非法记录均拒绝。删除成功仍保留原 latch，不解除停接或新建限制。删除不检查新建所需的空闲储备，因此磁盘不足等非阻删 latch 不会阻止安全回收。

## 原操作与释放条件

通过准入后，将原 active 与 `DeleteInstance`、实例名、时间一起写入 intent，并完成文件与目录同步。刚核实的 ready/agent ID 与该意图一并提交。持久化失败不调用 native，原事务残留保全。

native 只调用一次。创建和删除共用固定描述符执行、配置摘要、继承永久锁、独立进程组、父进程死亡信号及取消后等待。native 最长 300 秒，完整删除入口最长 330 秒；stdout 限制为 2 MiB，stderr 不进入诊断。成功删除只接受空白输出，非零退出、意外输出、取消或残留进程组均保留意图。

固定 native provider `v0.1.5` 先停止原实例，再发送删除并等待 LXD operation。guard 只在原 native 成功结束后核对专用 project 的实例清单为空、storage pool 只剩镜像卷，以及全部可见 operation 处于已知终态。custom 卷、VM 卷、容器、缺失或 null 清单及未知状态都阻止释放。已经完成但短暂保留的 operation 不被误判为运行中。

这些条件全部满足后，使用原状态事务清空 active 和 intent。提交完成前原 active 持续占用 provider 容量；保留下来的 latch 继续阻止新建。若成功退出的回执丢失，下一次删除因无 active 被拒绝，不再次调用 native。

只要 native 结果、资源核验或状态提交不确定，原 intent/active 或事务标记继续阻止写入。下一次调用不因 VM 已消失而清记录，也不重发删除。终止本地进程不能证明 LXD 原操作终结；未知结果由原恢复窗口处理。永久锁约束本程序的写入，部署仍须保证专用 project 不存在绕过 guard 的并发写者。

## 验收与运行边界

沿用[固定工具链和缓存](README.md#构建与检查)，在 `runner/` 执行：

```sh
go test -race -count=1 ./internal/provider
go vet ./internal/provider ./cmd/guarded-provider
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o .build/guarded-provider ./cmd/guarded-provider
```

测试使用私有 Unix HTTP 服务验证实际 LXD SDK，以及真实子进程替身验证参数、固定输入、成功、退出失败、取消、输出超限和 SIGKILL。状态测试覆盖名称错配、UUID 替换、未完成 bootstrap、写前失败、资源残留、提交失败、latch 保留、再次调用拒绝和释放后的创建准入。创建、watchdog 与共享状态事务的原回归同时运行。

本片交付 provider 删除与占用记录的闭环。跨 GitHub job、runner 注销、GARM 原实例、QEMU/cgroup、快照和临时凭据的完整生命周期验收仍由 [runner 架构](../ci-architecture.md#生命周期与恢复)约束。本片测试不证明真实任务或宿主资源已经闭合。没有启用 ScaleSet、连接真实 LXD、创建或删除真实 VM，也没有修改安装路径。[正常读取](read.md)由对应能力维护；records-only、旧记录迁移和部署另行实施。
