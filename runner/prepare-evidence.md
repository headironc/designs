# 历史 prepare 原件观察

`runner-admin inspect-prepare` 只读核验一份准确历史 prepare 的五份原件，并报告 release 与 amendment 的 staging 事实。它核 schema、跨件绑定、原 `before.json` 字节摘要和中断发表状态。`prepare_records_consistent` 只表示这次观察中的原件内部一致，不证明历史 producer 确已执行，也不表示恢复或操作完成。

本入口不构造 Go execution，不新建或恢复请求，不运行或 import 旧 Python/helper，不登录、解密或使用凭据。当前 25 份 installed file 内容、八 unit 实时状态、API、DB 和资源核验均排除。报告明确保留这些缺口。

## 固定目标与入口

只支持 `session-20261027-first-connection` 下的 amendment `credential-rebind-175824179`。原父目录固定为 `/var/lib/nucleus-halo-first-connection-inputs/session-20261027-first-connection`，下文简称 P；amendment 子目录简称 A。没有任意状态、operation、unit、数据库或凭据路径参数。

```sh
/opt/nucleus-halo/bin/runner-admin inspect-prepare \
  --release-sha256 a50bf88764f85824e420e7f6ea46224493624ebf66f3c2e753a5b64681201f36 \
  --plan-sha256 63486c3d0ad1075ef3f075bc4cdfc41fe14f69c66fd262f8ab1e3f497e4e13df
```

这个命令形状只有在准确 Go 成品已获独立安装和运行授权后才可使用。入口要求 root 和受控环境；两个摘要只选择已核的原格式，不是新写许可。报告另给自身 observer image 摘要，不能把它填成原 producer 身份。旧 host-plan 的正常阶段期限已过，观察不延期或修改计划。

实际运行另行绑定准确成品摘要、宿主、上述受保护读取范围、时限和原目录之外的新输出目标。输出由获授权的调用者以 `O_EXCL` 保存，不能覆盖旧 Human 原件。源码合入、私有夹具或退出码 0 都不完成这些运行条件。

## 原锁与 staging

程序只打开已有 `P/owner.lock`，以 `O_RDONLY` 取得排他非阻塞 flock。锁须为 root `0600` 单链接普通空文件；缺失或占用保持 unknown，不创建或替换。持锁至原件及目录末次复查结束。它不调用旧 Store 构造器，也不创建新的 services 锁。

原 bootstrap 会先暂存 `/var/lib/nucleus-halo-rebind-code/<release-sha256>`。带 plan 的 prepare 包有 16 文件：manifest 的 14 成员、manifest 本身和 host-plan。staging 发生在父锁之前；entry 又可能先创建 A 和 A/services，再进行外部预检。因此没有 repair intent 不能证明没有 staging，更不能授予重发。

release 观察只读取两份公开 JSON 并核其原字节摘要，按 manifest 检查固定成员的元数据、模式和大小。它不读 staged 成员内容，不执行 credential helper，不声称全部代码字节已验证。父锁不保护 release 目录；前后条目、对象、元数据或公开 JSON 变化形成 `release_staging_changed`，不新造锁补偿。

原件和目录通过无 symlink/magiclink 路径打开，检查 owner、权限、链接数、类型与边界。程序持有原 descriptor，并在结束前复核字节摘要和对象身份。读取与目录扫描使用 `O_NOATIME`，失败就报告具体拒绝。入口不 create、write、fsync、chmod、chown、rename 或 unlink 原材料；不把自身只读承诺扩大成其它进程从未改变现场。

## 五份原件

| 固定文件                        | 核验事实                                                                                                             |
| ------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| A/repair.intent.json            | 原 operation、amendment 和 approval_message；没有 producer 摘要或执行签名                                            |
| A/before.json                   | 12 个原字段的 schema；公开 App、旧 installation、repository、credential 选择器、历史文件事实及 controller invocation |
| A/services/service-binding.json | boot_id 与 before/after 文件事实，均和原 host-plan 绑定；使用 entry 实际构造的三字段 shape                           |
| A/services/services-before.json | 八个固定 unit 的原前态与 host.files；跨件核文件事实及 invocation，不查询当前 unit                                    |
| A/prepare.complete.json         | before_sha256 与准确 before 原字节一致；不包含另两份 service 材料的原摘要                                            |

JSON 拒绝缺失、非法 null、未知字段、重复或折叠别名及尾随值。整数保持准确类型，不经 float64；TTL 沿原 duration 秒数合同保留小数，先拒绝巨大指数再做精确数值检查。原 worker 前态可能 active 或 activating，不伪造“原先已排空”。

before 的 repository_row 有 7 列，credential_row 有 13 列。程序只检查必要公开实体/FK/endpoint-name 与 scalar shape。密文字段不做 Base64/hex 解码、解密、凭据可用性验证或单字段摘要，也不进入报告或原错误。完整性核验确实读取含密文的整体 bytes；报告准确使用 `credentials_not_decrypted_or_used`。

原 producer 将 Python canonical JSON 追加一个 LF，并在 complete 中保存追加前的摘要。因此只移除磁盘 before 最后一个 LF 后计算 SHA256，不 TrimSpace，也不用 Go 重序列化、改 key 顺序或实现 Python canonical 框架。Unicode、escape、数字拼写和空白都由原字节决定。

controller invocation 来自两次原采样。两件不同就报告 `historical_invocation_conflict`；不改绑，也不声称旧 producer 保证两次为同一实例。原材料没有完整 kernel PID/starttime、cgroup inode 或 manager sender 证明，不能升级为新 Go 服务身份回执。

## partial 与报告边界

原发布链为 `name.pending → hardlink target → unlink pending`。任何 pending、nlink2、半文件或链接冲突都保留。程序观察固定 pending 的元数据；target 与 pending 的相同 device/inode 和 nlink2 可报告 `linked_publication_incomplete`，仍不补写或 unlink。未知条目只给固定类别和数量，不读取其凭据结果或回显任意名字。

原件顺序为 intent、before、service-binding、services-before、complete。缺前件但有后件形成 inconsistent；hash 不同为 mismatch；读取失败、缺锁、观察期间变化和未完发表分别给具体 unknown。EACCES 或非法对象不能被说成不存在。五件一致也只给内部一致性状态。

报告分别给 staging、原锁、五件 presence/schema、publication、before_hash、cross_binding 和允许公开的历史投影。固定保留以下缺口：历史 producer 执行来源、service 前态原摘要、未重演的外部 preflight、未解密或使用凭据，以及未观察的当前安装前态。

`operation_complete`、`recovered`、`retry_allowed`、`enable_allowed`、`service_write_allowed`、`cleanup_allowed`、`migration_allowed` 和 `vm_acceptance` 始终为 false。返回码只说明是否产生有界报告，不是业务通过，也不签维护许可。错误仅含固定 stage/code，不回显原 JSON、SQL、异常或秘密。

## 来源与检查

本片解释固定保全包 `halo-20261026/vm-repository-rebind-host-20261027` 的 bootstrap、entry、rebind、host、Secrets.write 和 service snapshot 调用链。关键源为 `bootstrap.py:96–119`、`entry.py:170–213`、`rebind.py:94–106`、`acquire-session.py:52–67`、`lifecycle.py:45–74,111–121` 和 `install-services.py:122–137`。bootstrap SHA256 为 `9271a4154f0bf742129efb630fb4453f7ced06a6e65e1978eccc68cff2b701c1`。这些 pins 固定解释来源，不构成历史执行证明。

测试不 import 或运行旧 producer。独立合成字节与字面身份期望覆盖完整五件、所有前缀、原 bytes/LF/Unicode、错 schema/绑定/数字、worker busy 前态、实际只读 flock、`O_NOATIME`、pending/hardlink/FIFO、替换/并行变化、取消和脱敏。按 [runner 检查](README.md#构建与检查)完成受影响 normal/race/vet、静态 admin、文档格式与秘密扫描；不连接真实 protected 原件、服务、API、DB 或凭据。
