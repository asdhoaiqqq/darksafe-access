# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go run ./cmd/darksafe help
go test ./...
```

## 指标批量写入

`ingest` 从标准输入按行接收 JSON 数组，每个非空行是一批采样点，结果按行输出 JSON：

```bash
echo '[{"name":"cpu","timestamp":1700000000000,"value":0.5}]' | go run ./cmd/darksafe ingest
```

- 每个采样点：`name`（非空字符串）、`timestamp`（int64 范围内的 JSON 整数，毫秒）、`value`（可表示为有限 float64 的 JSON 数值）、`labels`（可选，字符串到字符串的对象，省略与 `{}` 均表示无标签）。
- 同序列同时间戳只允许一个值：相同值视为重复并忽略，不同值拒绝整批且不覆盖旧值；批内同样适用，`1` 与 `1.0` 视为相同。
- 任一点无效或冲突时整批不生效，此前批次保留；失败结果指出行号、点位置（从 1 开始）与原因，冲突给出序列、时间戳、已有值与提交值。
- 空白行不产生结果但计入行号；任一批失败则命令以非零状态退出。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
