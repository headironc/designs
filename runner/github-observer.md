# GitHub 只读观察凭据

本文保留自有管理入口及其原凭据、记录和 unknown 合同。新的[正常运维路径](architecture.md)使用标准 GARM 配置、API/CLI，不依赖该自有窗口或状态机。

本片为现有执行采集、闭合诊断、watchdog 暂停和 gateway 停止供应受管 GitHub installation token。供应、消费者租约和原 token 退役共同构成一个结果。源码与隔离验收不授权真实签发、撤销、登录、部署或启用 runner。

## 入口与来源

`runner-admin acquire-github-observer --request=<protected-request>` 消费 root 所有、模式 `0600` 的可信 JSON 请求。入口限固定 `dev` 宿主和受控环境，关闭 core dump 与进程 dumpability。请求不包含私钥或 passphrase。`retire-github-observer` 使用相同原请求，另要求 `--issuer-sha256` 绑定原发行成品。

请求字段如下；每个身份都来自当前已核事实，不能从示例、历史 unknown 或 token 外形推导。

| 字段                                                | 含义                                        |
| --------------------------------------------------- | ------------------------------------------- |
| `version`                                           | 固定为 `1`                                  |
| `operation_id`、`controller_id`                     | 当前原操作和 controller UUID                |
| `garm_session_sha256`、`garm_request_sha256`        | 已完成 GARM 会话的 producer 和原请求摘要    |
| `credential_id`、`credential_name`、`endpoint_name` | 准确 GARM credential 及公共 GitHub endpoint |
| `credential_updated_at`、`payload_sha256`           | SQLite 原更新时间文本和加密 payload 摘要    |
| `app_id`、`installation_id`、`account_id`           | 当前已批准 App、installation 和账号身份     |
| `public_key_sha256`                                 | PKIX 编码 RSA 公钥的 SHA-256                |
| `required_until`                                    | 本次执行及后续诊断所需的绝对 Unix 秒时刻    |

目标固定为 `headironc/nucleus`，repository ID 为 `1432619931`。没有任意仓库、组织、签名或 replacement 模式。

供应在 `/var/lib/nucleus-halo-credentials/github-observer` 的单一永久槽位中保留原件。它取得既有 `session.lock` 的非阻塞独占租约，从不创建原锁。读取原 `session/` 时只复制已持锁的 open file description，不执行 flock 转换。缺锁、来源漂移或历史未结请求都拒绝。

原 [GARM 会话](session.md)必须完成且仍在期限内。供应保留原历史、watchdog、provider 锁序，核 controller invocation、监听、配置、停接、进程和 LXD 前态。它不调用轮换、服务 job 或 Login。数据库观察器以实际服务 UID/GID、空附加组读取 query-only SQLite/WAL 快照，仅返回当前加密 payload 和必要来源事实。root 进程按原 `garm-provider-common/util.Unseal` 格式解封，核 App、installation 与公钥，再用已采用的 jwt/v5 签名。

## 单次发行

本地 App JWT 的原意图先于一次 RS256 签名。JWT 保留原 `iat` 和 `exp`，不会在恢复时重签或自动刷新。Go 堆中的秘密副本不具备物理擦除保证；私钥不会写入原件或诊断。

发行首先读取原 installation，再签发仅 `metadata: read` 的审计 token。审计请求不提供 `repository_ids`，用于检查整个 installation。实际清单须为唯一准确目标、`total_count=1` 且无分页链接；多仓库、重复或不完整清单立即拒绝。前后 installation 完整响应必须一致且未暂停。审计 token 的原 `204` 撤销响应和同 token 的实际 `401` 都保留后，才允许下一次 mint。

观察 token 的请求显式提供唯一 `repository_ids: [1432619931]`，权限恰为 `metadata: read`、`actions: read`、`administration: read`。响应中的未知、缺失或额外权限均拒绝，发布前再核实际仓库清单和 GARM 来源。读取 installation 时允许 App 原安装权限高于观察权限；这些权限不会进入观察 token。

每个外部步骤保留准确方法、路径、查询、请求与授权摘要、producer、开始时刻及原 HTTP 响应。请求固定 HTTPS GitHub，不使用代理、重定向、自动刷新或 SDK 调试输出。每个 POST/DELETE 最多发送一次。回执丢失保留 unknown；不同请求名或目录不授予重发资格。GET 的原响应已保存时只重放本地响应；读回丢失时可恢复只读观察。

发前期限超过当前一小时上界或 GARM 到期时刻时，零 mint。实际观察 token 的 `expires_at` 不足以覆盖 `required_until` 时，不发布、不补发，保留原 token 并进入退役。续行不重置期限，时钟回退拒绝。新 token 的长度不固定为 40 字符，仍受安全 header 字节和响应上界限制。

只有原响应、审计收尾、范围与期限判断完整，才先写 `verified.json`、发布 `github-token`，最后写 `complete.json`。部分本地发布恢复复用原事实，不重新 mint。

## 四个消费者

四个入口新增 `--github-observer-sha256`，用于固定受管路径 `/var/lib/nucleus-halo-credentials/github-observer/github-token`。消费者验证原 producer、请求、各步骤意图和响应、权限、范围、期限及完成封印。共享租约覆盖实际 API 使用，每次请求前和完整响应读回后再核期限与原文件身份。受管 token 只允许准确 Nucleus 和 controller。

capture 在执行目录预留前取得凭据，diagnose 在原执行验证后取得凭据。pause/gateway 只在既有新请求准入回调中读取，保留外层原操作锁；原回执恢复不要求新凭据。所有锁非阻塞，冲突拒绝，不等待或升级。

原外部 raw 凭据文件保留既有合同，不能携带受管 producer 参数或冒充受管完成结果。GARM 与 GitHub 的凭据来源分别验证，不把其中一个来源当作另一个的证明。

## 原 token 退役

退役取得同一独占租约，持久化 `retirement.intent.json` 后阻止新消费者。原 token 向 `DELETE /installation/token` 至多发送一次，再向原 installation repositories GET 保存实际拒绝。只有原 `204` 与实际 `401` 齐全，才报告 `revocation_rejection_observed`。

DELETE 回执丢失时不补发。原准确 `expires_at` 已到且取得实际 `401` 后，可报告 `expiry_rejection_observed`，保留原撤销 unknown。到期前的单个 `401`、403、404、超时和网络失败都不替代撤销证据。退役不要求 GARM 仍在线，避免来源变化阻止原 GitHub token 收尾。

清理仅删除原 inode 的派生 `github-token`；原响应、签名结果和 unknown 保留。已保存的远端事实在本地结果恢复时不重新请求。缺失 mint 回执的原操作继续 unknown，不能声称已完成全部 token 退役。

## 验收与后继边界

定向用例使用真实 go-github SDK、合成 RSA/密文和私有传输。覆盖精确 mint JSON、长 token、权限和仓库范围、来源、期限、永久锁、独占租约不降级、共享消费者、部分发布、同字节异 inode、回执丢失及退役恢复。数据库用例核 WAL 不写入；完整 Go 容器层验证实际服务身份和空附加组。

本片接受根目录 [Go 检查](../README.md#go-检查)、格式、链接、秘密筛查及源码绑定漏洞检查，并进入正式代码和安全复审。隔离结果不证明实际安装范围、密文绑定、`required_until` 或历史前态已经核准。

本片不创建 repository/ScaleSet，不取得 registration 或 Actions admin 会话，不增加 Administration write，不启用接单。首次连接与现场验收沿独立设计和操作授权推进。
