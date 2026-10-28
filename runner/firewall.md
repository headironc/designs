# 原生 nftables 防火墙

本片让原防火墙服务消费受限 JSON，并以一次 Go/netlink 事务替换两张自有表。当前源码已接入入口、持久化及恢复；真实 namespace 对比和完整发布验收仍待完成。源码变更不代表宿主切换或启用。

## 配置与网络策略

`runner-firewall render-config` 只输出非敏感配置，不连接 netfilter 或安装文件。零参数服务只读固定 `/etc/nucleiron-gateway/firewall.json`。文件须为 root 所有的单链接普通文件，模式 `0600`；父目录须为 root 所有且不允许组或其他用户写入。路径不得经过软链接。配置身份、原字节与文件 inode 在发送前复查。

配置要求完整的 `version`、`bridge`、`subnet`、`gateway` 和 `tcp_ports`。缺失、null、重复、大小写别名、未知字段、错误类型及额外 JSON 均拒绝。当前配置固定为版本 1、`nucleiron0`、`10.203.64.0/24`、`10.203.64.1` 和 TCP `53/9443/3128/6514`，不能扩大规则语言或选择任意表与 verdict。

- `inet nucleiron/guest_input` 保留 DHCP UDP `68→67`；允许该 bridge 上准确 IPv4 子网到 gateway 的 UDP 53 和上述 TCP 端口；其余来自该 bridge 的输入丢弃。
- `inet nucleiron/guest_forward` 丢弃该 bridge 的双向转发。
- `bridge nucleiron/guest_ipv6` 丢弃来自该 bridge 的 IPv6 输入。
- `bridge nucleiron/guest_peers` 丢弃该 bridge 的双向桥接转发。

其他接口的流量保持。TCP 接受规则由原端口集合展开为四条，每条各有 counter；累计 TCP 接受量须汇总四条 counter。包的允许范围保持，单条 counter 的统计含义改变。额外的 `managed` 链没有 hook，只保存原操作和配置来源，不参与包路径。

## 事务与恢复

服务使用 `/var/lib/nucleiron-firewall/operation.lock` 的永久独占锁。存在任一原槽位或归档时，锁缺失就拒绝，不能创建替代 inode。状态目录由 unit 以 root `0700` 建立。原文件须为 root `0600` 单链接普通文件；写入独占创建并同步文件和目录。

每个 `active/intent.json` 绑定随机操作 ID、实际运行程序摘要、host boot、network namespace 的 device/inode、原配置字节、原始内核前态及预期完整策略摘要。前态保留旧规则、匿名集合及集合元素的原字节，不要求旧 lookup 表达式经过新规则 decoder。完整 generation 在前态读取前后及提交前核对。配置与前态错误在发送前拒绝；意图持久化后只调用一次 Flush。

`active/result.json` 只表达以下已知结果，并绑定原意图摘要：

- `applied`：原标记及完整规则语义已读回，读取期间 generation 不变。
- `rejected`：原 socket 收到内核明确的事务 NACK；socket syscall 错误、缓冲丢失及中断不算此结果。
- `not_sent`：本次执行在 Flush 前明确中止。意图仍保留，进程中断造成的空档不能事后猜成未发送。

发送后超时、取消、ACK 丢失或无法核准策略时保留 unknown。再次调用时，原 unknown 只进入只读核验；配置、程序、boot 或 namespace 不同则拒绝。恢复成功只封存原结果，当次不再发送新 batch。无法证明原结果时保持占用，不能通过删除记录、换操作 ID、改名或重启绕过。新 boot 也不自动关闭旧 unknown。

完整策略核验保留表 flags、链的 hook/priority/policy、规则顺序、全部表达式字段和原 userdata。仅忽略内核 handle、枚举顺序及可变计数。SDK 会跳过不支持的表达式，因此读回另核原始字段；无法完整解释的表达式和额外集合、对象、flowtable 都拒绝。

原意图与已知结果齐全后，下一次显式服务 reload 可将原目录以不覆盖的 rename 保存为 `completed-<operation_id>`，再创建新事务。每次准入检查归档的完整意图、结果和原 ID；仅改归档目录名不能把 unknown 变成已知。状态目录最多观察 4096 个条目；前态最多 256 项、32 KiB 原始数据，单份记录最多 64 KiB。达到边界时拒绝，不能自动删历史。

netlink 连接的等待上限为 30 秒，也保留调用者更早的期限。取消直接关闭原 socket，结束前等待取消回调完成，再释放操作锁。取消不证明 batch 未生效。连接不重拨，也不保留可执行的后台重发任务。

## 切换和验证边界

切换须绑定准确宿主、原服务、二进制与规则版本，保护旧 unknown 和可回退原件。准备对应版本的 JSON 与 unit 后，才可在另行授权的窗口安装和读回。新入口不消费旧 `.nft` 文本，没有生产 CLI fallback。停止服务仍保留规则。

普通测试只使用原生 netlink 和文件替身。已覆盖单 batch、原操作恢复、并发锁、同步失败、不可覆盖归档和未知协议字段。完整验收还须在已证明独立的 user/network namespace 中比较旧/新包行为、验证真实内核原子拒绝及其他表保持。宿主拒绝创建 namespace 时，该验收未运行，不通过 sudo 或宿主规则操作补做。
