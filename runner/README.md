# 历史 Runner 恢复导航

当前 CI 使用标准 GitHub hosted runner，入口见 [CI 与运行边界](../ci-architecture.md)。自托管 CI 建设已取消，本目录只维护历史恢复导航。池建设、故障矩阵、扩容和 VM 生产切换不再是当前计划。

- [Nucleus CI 与依赖镜像](../ci-architecture.md#nucleus-ci-与依赖镜像)：当前 hosted 业务入口及镜像消费者。
- [历史恢复责任](records-recovery.md)：原身份、unknown、不重发、现场原件，以及自研与上游试验版本的追溯。

自研 guard、watchdog、admin、bootstrap 和 firewall 程序、模板及专用构建检查已退出主线。它们的源码与实施说明通过 Git 历史追溯，不再提供当前安装或构建入口。运行现场继续由原恢复 owner 按实际安装版本和原记录处理；源码退役不停止服务、不改变凭据，也不结束旧操作。

文档和通用工具检查使用[根目录入口](../README.md#格式与工具检查)。Nucleus 继续维护有真实消费者的 CI 镜像和业务检查。源码退役与路线取消都不证明现场资源已删除或历史执行已闭合；必要安全收口仍由原恢复 owner 按准确对象和有效授权处理。
