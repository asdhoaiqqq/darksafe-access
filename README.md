# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go run ./cmd/darksafe plan <JSON文件路径>
go test ./...
```

`plan` 离线读取发布配置并输出发布计划（JSON），不连接 Kubernetes，不执行实际发布。输入字段：

- `app`、`revision`、`image`：应用名称、待发布修订号、容器镜像（非空）。
- `batchSize`：每批最多容纳的集群数量（正整数）。
- `clusters`：候选集群列表，每项含唯一 `id`、可选 `disabled`、可选 `tags`（字符串标签）。
- `include` / `exclude`：标签条件列表；同一条件内所有键值精确相等才算匹配，条件之间满足任意一个即可。未提供 `include` 时允许所有可用集群，未提供 `exclude` 时不排除任何集群。停用集群始终不能进入计划；同时命中包含和排除时以排除为准。

成功时标准输出只包含完整计划（应用信息、从 1 连续编号的批次、按标识升序排列的未入选集群及原因）；失败时以非零状态退出，标准错误指出具体问题。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
