# 当前安装文件与历史计划的只读比对

`runner-admin inspect-installed` 比对固定历史 host-plan 的 25 个安装文件。结果是当前文件事实与原计划的关系。它不恢复历史操作，也不签发启动、重试、清理、迁移或 VM 验收许可。

本入口独立于 [inspect-prepare](prepare-evidence.md)。后者继续只观察五份原件及 staging，不默认读取当前安装文件。两个报告均不证明原 producer 已执行或历史操作已完成。

## 固定输入与输出

入口只接受下列固定摘要，不接受任意路径、operation、配置或凭据参数：

```sh
/opt/nucleus-halo/bin/runner-admin inspect-installed \
  --release-sha256 a50bf88764f85824e420e7f6ea46224493624ebf66f3c2e753a5b64681201f36 \
  --plan-sha256 63486c3d0ad1075ef3f075bc4cdfc41fe14f69c66fd262f8ab1e3f497e4e13df
```

这是后续获授权宿主运行的命令形状，不是本片已执行的命令。实际运行还需绑定准确成品摘要、宿主、读取范围、时限和独立输出位置。源码合入与私有测试不完成这些运行条件。

原 operation 为 `session-20261027-first-connection`，amendment 为 `credential-rebind-175824179`。只打开已有 `/var/lib/nucleus-halo-first-connection-inputs/session-20261027-first-connection/owner.lock`，取得排他非阻塞 flock。原锁必须为 root `0600` 单链接普通空文件；缺失、占用或不安全就报告 unknown，不创建或替换。

随后读取 `/var/lib/nucleus-halo-rebind-code/a50bf88764f85824e420e7f6ea46224493624ebf66f3c2e753a5b64681201f36/host-plan.json`。它必须为 root `0600` 单链接普通文件，原字节 SHA256 和严格 schema 均通过才继续。计划过期只作为历史事实输出，不延期，也不用于授权新操作。入口不扫描 14 个 staged 成员，不读取五份 prepare 原件。

固定集合复用 `prepareFixedPaths`，由 plan 严格核对：

| 类别                | 固定文件                                                                                                                                                                                                                                                                                                               |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| egress              | `/etc/nucleiron-egress/dns.yaml`、`/etc/nucleiron-egress/squid.conf`                                                                                                                                                                                                                                                   |
| gateway             | `/etc/nucleiron-gateway/gateway.conf`、`/etc/nucleiron-gateway/tls/ca.crt`、`/etc/nucleiron-gateway/tls/server.crt`、`/etc/nucleiron-gateway/tls/server.key`                                                                                                                                                           |
| controller/provider | `/etc/nucleus-halo/garm-provider-lxd.toml`、`/etc/nucleus-halo/garm.toml`、`/etc/nucleus-halo/provider-policy.json`                                                                                                                                                                                                    |
| logs                | `/etc/rsyslog.d/nucleiron/server.conf`                                                                                                                                                                                                                                                                                 |
| systemd             | `/etc/systemd/system/nucleiron-dns.service`、`nucleiron-egress.service`、`nucleiron-firewall.service`、`nucleiron-gateway.service`、`nucleiron-logs.service`、`nucleiron-provider-watchdog.service`、`nucleiron-provider-watchdog.timer`、`nucleiron.service`，以及同目录 `nucleiron.service.d/30-provider-guard.conf` |
| 原部署证书          | `/home/headiron/runner-deployment/halo-20261026/nucleiron-ca.crt`                                                                                                                                                                                                                                                      |
| 程序                | `/opt/nucleus-halo/bin/garm`、`/opt/nucleus-halo/bin/garm-provider-lxd`、`/opt/nucleus-halo/bin/guarded-lxd-provider`、`/opt/nucleus-halo/bin/network-firewall.py`、`/usr/sbin/nginx`                                                                                                                                  |

报告固定路径的 matched、mismatch、missing 或 unknown，以及固定原因。只有全部 25 项和原锁、计划的末次核验通过，聚合状态才是 `installed_files_match_historical_plan`。退出码 0 只表示报告成功输出，不能作为安装或业务通过条件。

## 文件合同与变化

文件事实沿原 `host.py:runtime_fact` 和 `install-candidate.py:file_fact`：SHA256、size、uid、gid、含特殊位的 permission mode，以及 xattrs。正文只流式计算摘要，不解码 TOML/PEM，不使用私钥或其他凭据。报告不输出正文、实际内容摘要、xattr 名和值或原系统错误。

原 plan 的 25 项 xattrs 摘要均为 `44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a`，对应字面 `{}`。新入口显式要求此合同；fd-based listxattr 为空才匹配，非空为 mismatch，读取错误为 unknown。它不读取属性值，也不实现另一套 Python JSON canonical。

先通过 `O_PATH` 核普通单链接叶对象，再以 `O_RDONLY|O_NOATIME|O_NONBLOCK` 打开正文并复核相同对象。FIFO 或链接替换不会进入阻塞读取。大小不同直接报告 mismatch；hash 最多读计划大小加一个字节，并检查取消。读取或 `O_NOATIME` 被拒绝保持 unknown，不回退到可能改 atime 的路径。

原锁不保护管理员修改系统文件。观察保留叶与可信父目录 descriptor，结束前重核路径、对象和元数据，再次 hash、检查 xattrs；变化转为 unknown。缺失文件后来出现也不能沿用 missing。跨文件观察不是原子快照，末次复核之后仍可能发生新变化。

祖先继续遵守现有 protectedFiles 规则：root 所有、无 group/other 写权限、无 symlink/magiclink。固定证书位于用户目录，若其祖先不可信，就报告 `parent_untrusted`/unknown。入口保留其他文件的观察，不能为获得全匹配而放宽规则或跳过该项。

## 未观察事实与验证

报告所有完成与写入许可固定为 false。它保留原执行来源、五份 prepare 原件、当前服务与 boot、API/DB/资源、额外 watchdog 和完整 Host.facts 的缺口。原 Host.facts 还核宿主、boot、额外 watchdog 成品及 D-Bus configuration；25 文件一致不覆盖这些职责。

行为来源为同一保全包：host.py SHA256 `36daadbeab5c5aa03d5da17591ba6a2a080f6993a23d32b11218917cf850f78f`，install-candidate.py SHA256 `ee35dab47d9b4a3796fb3d5b148de154e0cf65a8918d53d0f54ecfddf677f564`。源码仅用于解释合同，不执行或 import 旧程序。

私有合成夹具覆盖全部匹配、metadata/content/xattr 漂移、缺失与拒绝、祖先限制、链接/FIFO、原锁/计划拒绝、观察间替换与取消、atime 和输出错误。精确 root-owned plan 的端到端用例在固定镜像的一次性 `--network none` 容器运行；普通非 root 测试的 skip 不证明它通过。宿主仅运行私有文件与读取边界测试，不读取真实 protected 路径，也不接真实服务或 API。

复验使用已准备的隔离 Docker 环境，先确认下面的镜像已存在且 ID 相符。只挂本次合成测试成品，容器在自身 `/tmp` 创建夹具；不挂宿主业务路径，不加 privileged 或 capability。以下命令在 `runner/` 执行，沿 README 设置离线 Go 缓存：

```sh
mkdir -p .build/installed-fixtures
CGO_ENABLED=0 go test -c -o .build/installed-fixtures/normal.test ./internal/lifecycle
go test -race -c -o .build/installed-fixtures/race.test ./internal/lifecycle
image=sha256:088e409b5821b1eb584aae3409bf3899a54f34c0441952c0e1cae6731941671e
docker image inspect "$image" --format '{{.Id}}'
docker run --rm --network none --name runner-installed-private-test \
  --mount "type=bind,src=$PWD/.build/installed-fixtures,dst=/fixtures,readonly" \
  --entrypoint /bin/sh "$image" -c \
  'set -eu; /fixtures/normal.test -test.run=^TestInstalled -test.v; /fixtures/race.test -test.run=^TestInstalled -test.v'
docker ps -a --filter 'name=^runner-installed-private-test$' --format '{{.ID}} {{.Status}}'
```

保存原退出状态、测试输出和精确名称的清理回读。该本地镜像只是本片已验证环境的准确身份，不表示其它环境已准备。镜像或 race 所需 libc 不满足时保留缺口，不自动下载替代品。Go module、private D-Bus、vet 与静态 admin 检查仍沿 README；本片不改变 CI。
