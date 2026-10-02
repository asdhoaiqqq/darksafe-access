# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe plan ./rollout.json
go run ./cmd/darksafe version
go test ./...
```

## 离线发布计划预览（plan）

`plan` 读取本地 JSON 配置，离线输出应用将发布到哪些集群、如何分批；不连接 Kubernetes，不执行实际发布。

配置字段：

- `application` / `revision` / `image`：应用名称、待发布修订号、容器镜像，均不能为空白；镜像不做远端校验。
- `clusters`：候选集群列表。`id` 唯一且非空白；`disabled` 缺省视为可用；`labels` 为字符串键值，缺省视为空集合，标签值允许为空字符串。
- `batchSize`：每批最多容纳的集群数量，必须是正整数。
- `include` / `exclude`：标签条件列表。一个条件内所有键值精确相等才算匹配，多个条件满足任意一个即可；无通配符、无大小写转换，标签不存在即不匹配。`include` 缺省或为空表示允许所有可用集群，`exclude` 缺省或为空表示不排除；同时命中时以排除为准，停用集群始终不入选。

输出为标准输出上的 JSON：应用信息、从 1 开始连续编号的 `batches`（集群按标识字符串升序后按容量分批，最后一批可不足容量），以及按标识升序排列的 `skipped` 未入选集群与原因（集群已停用 / 命中排除条件 / 未命中包含条件）。结果对输入集群顺序、标签键顺序、条件顺序均不敏感，重复预览输出一致。

文件无法读取、JSON 非法、字段类型或校验不通过、候选列表为空或没有可发布集群时，命令以非零状态退出，具体问题输出到标准错误，标准输出保持为空；没有可发布集群时会在标准错误中列出每个候选集群的未入选原因。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
