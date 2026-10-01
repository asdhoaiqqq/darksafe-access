# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go test ./...
```

## 按组织的策略发布、回滚与复核

`darksafe.NewStore()` 提供组织级策略管理（纯内存，随服务实例结束而销毁）：

- `Publish(org, expectedVersion, policies)`：校验并整套发布，生成连续递增版本；版本不一致或策略非法则整次失败，不占用版本号。
- `Rollback(org, expectedVersion, targetVersion)`：把本组织历史版本的完整内容发布为新版本，历史不可改写。
- `Policies(org, version)` / `CurrentVersion(org)`：查询历史版本完整策略（返回副本）与当前版本。
- `Decide(org, OrgRequest)`：用当前版本决策，要求主体组织与资源组织均等于决策组织；默认拒绝，拒绝策略优先，结果注明实际版本。
- `Review(org, version, OrgRequest)`：按指定历史版本复核，结论不受后续发布/回滚影响；版本不存在则拒绝并说明原因。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
