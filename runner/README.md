# Runner 架构与恢复导航

正常路径采用 GARM v0.2.1 与上游 LXD provider v0.1.5，由 GitHub 原生 job container 消费 Nucleus 维护的私有 CI 镜像。GARM、provider 和 LXD 拥有正常生命周期。标准成功路径已完成真实 VM job、私有拉取和自动回收验收；完整故障矩阵与生产 CI 切换仍未完成。

- [目标架构与迁移边界](architecture.md)：组件职责、最小定制、安全边界、正常删除和异常停接，以及后续迁移顺序。
- [跨仓库 CI 与 Nucleus 消费者](../ci-architecture.md#runner-与-nucleus-ci)：当前源码入口、镜像归属、不可信池和已验范围。
- [历史恢复责任](records-recovery.md)：原身份、unknown、不重发、现场原件，以及准确历史源码与恢复版本的追溯。

自研 guard、watchdog、admin、bootstrap 和 firewall 程序、模板及专用构建检查已退出主线。它们的源码与实施说明通过 Git 历史追溯，不再提供当前安装或构建入口。运行现场继续由原恢复 owner 按实际安装版本和原记录处理；源码退役不停止服务、不改变凭据，也不结束旧操作。

本目录仅保留架构与恢复文档。文档和通用工具检查使用[根目录入口](../README.md#格式与工具检查)。当前 probe、专用 JIT 模板、镜像和业务检查由 Nucleus 随实际消费者维护；本仓库提供跨库架构与恢复导航。实际部署、启用及历史恢复分别绑定目标、前态、授权和验收。
