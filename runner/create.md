# 单次 VM 创建准入与执行

`guarded-provider` 无参数运行时接收 GARM `v0.1.0` 的 `CreateInstance` 请求。它在准入后调用一次 native LXD provider，核对结果并持久化 active。成功退出 `0`，永久操作锁忙退出 `75`，其余拒绝退出 `1`。其他 provider 命令均拒绝；正常启动与 watchdog 参数入口保持各自职责。

本片交付单次创建，不提供删除、容量释放、records-only、旧记录迁移或部署。完整 provider 切换还须完成后续命令及宿主验收，GARM 已安装的 external provider 路径不变。

## 准入与输入

入口复用[正常启动](startup.md)的服务身份、文件保护、摘要及永久锁。整个操作持有同一锁，维护标记、状态事务残留、intent、latch 或 active 均阻止创建。已有记录保持原解释，不因 VM 消失自动清空。初始清洁状态由部署窗口核定；本程序不初始化状态，也不证明历史操作已经闭合。

`GARM_COMMAND` 必须为 `CreateInstance`，controller 与 pool 必须匹配 policy。配置路径固定为 `/etc/nucleus-halo/garm-provider-lxd.toml`，接口为 `v0.1.0`。只将本次操作所需的环境变量传给 native，不继承调用方注入的其他配置。

使用 `github.com/cloudbase/garm-provider-common/params v0.1.9` 解析请求与响应，与 native provider `v0.1.5` 的协议来源一致。输入上限为 2 MiB，顶层拒绝重复键、大小写别名和未知字段。请求要求准确实例名、policy 镜像 alias 与 pool、`halo-ci` flavor、Linux/amd64、JIT 和非空 instance token，拒绝 SSH keys。本片沿用当前消费者的 Nucleus repository URL，以及 `10.203.64.1:9443` 的 metadata/callback 地址，不扩展为任意仓库或网关。

`extra_specs` 仅接受当前生成器拥有的 `disable_updates:true` 和 `runner_install_template`。模板是规范 Base64 字符串；按这两个固定字段的排序、紧凑 JSON 编码核对 policy 中的 `extra_specs_sha256`。字段名与值均为 ASCII，与原 Python 生成器的摘要字节一致；不引入通用 JSON 规范化协议。通过后，将原请求字节交给 native，不重新编码 bootstrap 内容。token 与完整输入只在内存中使用，不写入意图或诊断。

创建前检查现有挂载、空间储备及五个依赖服务。LXD client 只读 `halo-ci` project：实例清单须为空，storage pool 仅允许镜像卷，镜像 alias 须指向固定摘要。operation 必须处于已知终态；LXD 短暂保留的已完成记录不会被误判为运行中。缺失或 null 清单、缺失状态、运行中 operation 和未知状态都拒绝。Unix socket、响应上限、取消和禁止代理／重定向沿用 watchdog 的读取边界。

## 一次执行与状态提交

准入通过后，先将 `CreateInstance`、实例名与时间写入 intent。使用[现有状态事务](guard-watchdog.md#锁持久化与失败保全)同步文件和目录，持久化成功后才执行 native。写入失败不调用 native，事务残留原样保全。

执行前重新打开 native 与配置，核对受保护文件及 policy 摘要。子进程通过继承的描述符读取准确程序和配置，避免路径替换后执行另一份文件。永久锁描述符也传给子进程，guard 意外退出时原 native 仍受同一锁约束。native 使用独立进程组和父进程死亡信号；取消时终止该组并等待原子进程结束。正常返回还须确认该进程组已无成员。原生创建程序本身等待 LXD 创建和启动 operation 完成；杀死本地进程不能证明服务端 operation 已结束。

native 最长运行 300 秒，完整入口最长 330 秒。取消会关闭本命令拥有的 stdin，使未完成的管道输入退出。stdout 最多接收 2 MiB；stderr 被丢弃，最外层只记录固定操作和失败阶段。输出超限、非零退出、取消或进程组残留均保留 intent。

只有 native 成功结束，且响应的名称、provider ID、Linux/amd64 与 running 状态均正确，才进入 LXD 读回。读回要求同名 VM、准确 controller/pool、镜像摘要、唯一 `halo-ci` profile、有效 UUID，以及没有运行中或未知 operation。随后原子保存 `active`，标记 `ready:false` 并清除 intent。bootstrap 完成继续由已交付的 watchdog 核验；创建成功不等于 runner 已 ready。

active 持久化完成后才返回响应。若向 GARM 返回时管道断开，active 仍占用容量；再次创建被拒绝。请求发出后只要结果或持久化不确定，原 intent 或事务标记就继续阻止写入。后续调用不重发、不删除 VM，也不凭当前资源清单补造成功。原操作由授权的恢复窗口处理，本片不增加自动恢复或清记录入口。

## 验收与限制

沿用[固定构建环境](README.md#构建与检查)，在 `runner/` 运行：

```sh
go test -race -count=1 ./internal/provider
go vet ./internal/provider ./cmd/guarded-provider
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -o .build/guarded-provider ./cmd/guarded-provider
```

测试覆盖错误身份与输入、占用容量、持久化先于调用、原始请求字节、未知结果不重发、输入取消和输出上限。私有 Unix HTTP 服务验证实际 LXD SDK 的清单与实例核验。真实子进程测试验证 native 退出、取消，以及 SIGKILL 后原意图保留和 native 终止。原有启动、watchdog 与状态事务测试覆盖共享实现的变化。

这些测试不创建真实 VM，不访问宿主 LXD 或 system bus，不修改安装文件，也不启用 ScaleSet。固定依赖版本未升级；provider-common 从既有间接依赖改为直接消费。[LXD 告警处置](guard-watchdog.md#包验收与消费)继续适用，客户端仍仅使用 GET。宿主安装、真实 GARM 调用及完整生命周期另行验收。
