# Guest bootstrap

`runner-bootstrap` 在新 guest 中安装 JIT 身份并启动 runner 服务。它消费原生 provider 渲染的 [runner-install.tmpl](templates/runner-install.tmpl)，通过标准输入接收实例配置。自有程序使用 [Go 1.27.3](README.md#构建与检查)，只依赖标准库。

## 消费与结果

镜像必须预装 `/usr/local/bin/runner-bootstrap` 和 `/home/runner/actions-runner`。Kiln 拥有镜像构建；本片交付二进制源码及模板，不改镜像或运行中的 provider。原生 cloudconfig 仍以 `runner` 用户执行渲染脚本。模板关闭 shell 跟踪，token 不进入命令参数或环境变量。

输入只接受 JIT 模式和原网关的准确地址：`https://10.203.64.1:9443/api/v1/metadata` 与 `https://10.203.64.1:9443/api/v1/callbacks`。允许末尾斜线，不接受其他路径或协议。TLS 使用系统信任，绕过公共代理并拒绝重定向。每次请求限时 20 秒，响应上限为原合同的 1 MiB。

程序拒绝复用已有身份，包括悬空链接。单次安装持有本地文件锁；并发调用失败，不上报另一个安装失败。三份 JIT 文件和 `.service` 独占创建，权限为 `0600`。`agentId` 按正 `int64` 解析，服务名只接受 `actions.runner.*.service` 格式和原长度上限。

程序从原网关取回 unit，以原 sudo install 命令安装。它保留代理 drop-in、`runsvc.sh` 复制、`env.sh`、daemon-reload、enable 与 start 的顺序。unit 是受信任的 provider 输入，不接受 guest 工作流提供的 unit。每条子命令限时 30 秒，输出不进入诊断。

服务启动成功后，程序提交 system-info 和 idle 回调。两次回调成功后才发布 `.nucleiron-bootstrap-ready`，内容仍为 `instance` 与 `agent_id`。回执先完整写入并同步，再以不覆盖的方式发布。成功事件为 `bootstrap_complete`。

## 失败与恢复

失败日志只包含 `bootstrap_failed` 及阶段：`preflight`、`jit-files`、`service-install` 或 `service-start`。有效配置允许一次有时限的 failed 回调；失败回调也未确认时输出 `failure_callback_unconfirmed`。无效输入和并发拒绝不发送回调。程序不输出 token、JIT 文件、HTTP 响应或子命令错误正文。

命令超时、取消或回调丢失不证明外部操作未生效。程序保留已写身份和可能已安装、已启动的服务，不自动重试或回滚。后续安装会拒绝已有身份；恢复必须检查原 guest、服务及回调结果，不能靠删文件重启安装。本片不重启任何旧 Python 操作。

## 检查

沿用 [Go 构建环境](README.md#构建与检查)，在 `runner/` 运行：

```sh
go test -race ./internal/bootstrap
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o .build/runner-bootstrap ./cmd/runner-bootstrap
```

测试使用临时身份目录、TLS 测试网关和模拟的安装命令。覆盖成功回执、精确整数身份、错误响应、重定向、超限响应、已有身份、并发、命令取消、回调失败及脱敏。模板测试在 `bash -xv` 下验证输入没有被执行或记录。测试不调用真实 sudo、systemd、GARM 或 GitHub。

原生消费者验证使用 [LXD provider v0.1.5 源码归档](https://github.com/cloudbase/garm-provider-lxd/tree/v0.1.5)。归档 SHA256 为 `a3a9413db3d508ecfddfad58bb3b65e0267fd44a719b7f4539d10a4775f8e188`。先核摘要，再解包；在其根目录用 Go 1.27.3 运行：

```sh
go run -mod=vendor /path/to/designs/runner/tools/render-bootstrap.go \
  /path/to/designs/runner/templates/runner-install.tmpl \
  /path/to/fresh-output/install-runner.sh
bash -n /path/to/fresh-output/install-runner.sh
```

该工具只渲染 synthetic 身份，不运行安装脚本或创建 VM。固定 vendor 只供这个兼容性检查，不进入产品二进制。输出路径必须不存在；生成脚本和 cloud-init 留在检查目录，不提交 Git。

行为基线是修复后的 `vm-lifecycle-candidate-fa6b5782-20261026/runner-install.tmpl`，SHA256 为 `fa6b578241c96c1bccc3e88e1811c3eecdc4af1ee941820bced4d389e00fb79d`。它不读取原生参数中不存在的 `AgentMode`；Go 模板保留这一修复。实际镜像安装、ScaleSet 启用、VM 与生产验收属于后续切换。
