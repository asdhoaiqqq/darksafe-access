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

## 审计链：查询、导出与复核

`darksafe.NewStore()` 在内存中为每个组织维护一条不可篡改的审计链。每条记录包含组织、组织内从 1 开始连续递增的序号、类别，以及衔接前后记录的 SHA-256 指纹。记录保存的是请求与策略的独立副本，调用方事后改写提交内容、策略或查询返回的数据都不会影响已有记录。

- `Publish` / `Rollback`：成功时在同一把锁内追加一条策略变更记录，记录新版本号与完整策略，回滚还记录来源版本；失败时状态、版本号与审计记录均不变化。
- `Decide`：只要指定了非空决策组织，每次调用（包括未发布策略、主体停用、字段缺失、组织不一致造成的拒绝）都追加一条决策记录，保存完整请求与实际返回的是否允许、理由、命中策略及版本；决策与记录在同一临界区内完成，记录在操作返回前已可查，且准确对应实际使用的策略版本。缺少决策组织时按现有行为拒绝，不创建记录。
- `Review` 与记录复核均保持只读，不产生访问记录。
- `AuditQuery(org, AuditQuery{PageSize, UpToSeq, Fingerprint, AfterSeq, Category, Subject})`：按序号升序分页。首次查询（`UpToSeq` 为 0）冻结当前尾部范围并返回截至序号与指纹；后续翻页沿用该范围，途中追加的记录不混入。支持按类别（`AuditPolicyChange` / `AuditDecision`）和决策主体筛选，指定主体时只返回其决策记录。没有记录的组织返回空页与序号 0 的校验依据。页大小非正、范围倒置、截至序号超出现有记录或指纹不符均返回明确错误；无匹配项返回空结果。
- `AuditExport(org, upToSeq)`：导出 1..截至序号的完整记录（含空记录）及尾部指纹，用于脱离 Store 离线核对。
- `VerifyAuditExport(AuditExport)`：离线校验记录数、序号连续性、组织归属、指纹链，以及每条决策记录与所用策略版本的一致性；调用方另行保管组织、截至序号与指纹作为核对依据。修改字段、删除中间或尾部记录、交换顺序、拼入另一组织记录都将验证失败。
- `ReverifyDecision(org, seq)`：按组织和决策记录序号，用记录中的请求与策略版本恢复原结果，后续发布或回滚不影响结论；非决策记录返回 `ErrAuditNotDecision`，不存在的序号返回 `ErrAuditNotFound`。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
