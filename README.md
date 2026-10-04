# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go run ./examples/save-decision   # 保存一条真实决策的归档与检查点，供离线复核
go test ./...
```

## 按组织的策略发布、回滚与复核

`darksafe.NewStore()` 提供组织级策略管理（纯内存，随服务实例结束而销毁）：

- `Publish(org, expectedVersion, policies)`：校验并整套发布，生成连续递增版本；版本不一致或策略非法则整次失败，不占用版本号。
- `Rollback(org, expectedVersion, targetVersion)`：把本组织历史版本的完整内容发布为新版本，历史不可改写。
- `Policies(org, version)` / `CurrentVersion(org)`：查询历史版本完整策略（返回副本）与当前版本。
- `Decide(org, OrgRequest)`：用当前版本决策，要求主体组织与资源组织均等于决策组织；默认拒绝，拒绝策略优先，结果注明实际版本。
- `Review(org, version, OrgRequest)`：按指定历史版本复核，结论不受后续发布/回滚影响；版本不存在则拒绝并说明原因。

### 按具体资源限定策略

策略可填写字符串字段 `ResourceID`，把作用域级授权收窄到一份具体资源（如某一份账本）：

- 未填写或为空时保持原有匹配规则：主体、动作、作用域相符即命中。
- 填写后，资源限定是附加条件而非替代：必须同时满足主体、动作、作用域（含 `Recursive` 子作用域）且请求资源 ID 与字段值**完全相同**才命中；资源 ID 不符时该策略既不授权也不出现在命中列表中，没有其他允许策略时返回既有的“无匹配允许策略”默认拒绝。
- 资源 ID 按原始字节比较：区分大小写、保留首尾及内部空格、`*` 只是普通字符、不按作用域路径解释，也不要求资源预先登记。
- 允许与拒绝策略均可限定资源，也可与未限定策略同时发布；拒绝优先、命中标识去重排序、实际版本解释等规则不变。作用域级允许配合只针对账本甲的拒绝会拒绝甲、允许同范围的乙；作用域级拒绝仍压过只针对甲的允许。
- 请求缺少资源 ID、主体停用或组织不一致时，仍按既有信封规则拒绝，资源限定不能绕过任何检查。
- 限定作为策略内容随版本保存：历史查询、回滚、`Review`、`RecheckDecision` 与离线复核都使用当时的限定；事后把策略从甲改到乙不影响旧决策的复核结论。限定字段纳入审计指纹（为空时沿用历史指纹编码，旧记录与旧导出的指纹、检查点不变），修改导出材料中的限定而保留原指纹与检查点会导致校验失败；含非 UTF-8 字节时按原始字节保存与校验。

## 组织审计链

每次成功的发布、回滚，以及每次指定了非空决策组织的 `Decide`，都会在该组织追加一条不可变记录。失败的发布/回滚不改变策略状态、版本号与审计记录；缺少决策组织的 `Decide`、以及 `Review` 和按审计记录复核均为只读，不产生记录。

每条记录含组织、组织内从 1 连续递增的序号、类别和 SHA-256 指纹；指纹覆盖记录完整内容并链接上一条，根指纹绑定组织名，因此不同组织即使使用相同标识，其记录、序号与校验依据也相互独立。

- 策略变更记录（`AuditPolicyChange`）：保留新版本号与该版本完整策略；回滚记录额外以 `SourceVersion`/`RolledBack` 说明来源版本。
- 决策记录（`AuditDecision`）：保留完整请求以及实际返回的允许与否、理由、命中策略和实际版本（含未发布策略、主体停用、字段缺失、组织不一致等拒绝）。
- `AuditQuery(org, startSeq, pageSize, kind, subject)`：按序号升序分页，可按类别或决策主体筛选（指定主体时只返回其决策记录）。首次查询固定“截至序号 + 指纹”检查点，后续用 `AuditPage(checkpoint, nextSeq, ...)` 翻页；查询期间新增的记录不会混入。无记录组织返回空页与序号 0 的根指纹。页大小非正、起始序号非法、范围倒置、截至序号超出当前记录或指纹不符均返回明确错误。
- `AuditExport(org, endSeq)`：导出截至序号内的完整记录（空组织导出空集与根检查点），返回的副本与内部状态完全脱离。
- `VerifyAudit(org, records, checkpoint)`：纯函数，不依赖 Store，可离线校验。修改字段、删除中间或尾部记录、交换顺序、拼入其他组织记录都会验证失败。
- `RecheckDecision(org, seq)`：按决策记录里保存的请求与其实际使用的策略版本恢复结论，后续发布/回滚不影响结果；非决策记录返回 `ErrAuditNotADecision`，序号不存在返回 `ErrAuditNotFound`。
- `RecheckDecisionOffline(org, records, checkpoint, seq)`：纯函数，不创建或恢复 Store，服务实例结束后仅凭完整导出、单独保存的检查点和目标序号离线复核一条决策。整份输入先通过 `VerifyAudit` 链校验（目标之后的记录损坏、按主体筛选的记录、缺少开头的片段均失败），再以目标之前同组织策略变更记录携带的完整内容重放该请求实际使用的版本；版本缺失或只在目标之后出现返回 `ErrVersionNotFound`，绝不改用较新版本。返回原决策、重算决策及是否一致（允许与否、理由、命中策略、实际版本全字段比较，nil 与空命中列表视为一致）；链合法但两决策不同时明确标为不一致。该入口只读，不修改输入材料也不追加审计记录。
- 审计材料无损归档：`EncodeAuditArchive(org, records, checkpoint)` 把一次完整导出（截至某历史序号的前缀亦可，只要与检查点对应；无记录组织用序号 0 的根检查点）序列化为可保存到文件的字节；`DecodeAuditArchive(archive, org, checkpoint)` 在原服务实例结束后重新读取，返回可直接交给 `RecheckDecisionOffline` 的记录。格式为手写的长度前缀二进制编码（魔数 + 载荷长度 + 载荷 + SHA-256），所有字符串按原始字节还原——组织名、策略/资源标识、角色、命中列表与理由中的中文、控制字符、空格及非 UTF-8 字节均不替换不规整，单字节 0xFF、0xFE 与真正的 U+FFFD 仍可区分；nil 与非 nil 空列表、不存在的记录载荷均保留原有形态，因此重新读取不会改变指纹。编码与读取都先做整份材料校验：缺组织返回 `ErrMissingOrganization`，记录缺失、乱序、混入其他组织、内容改动、检查点不符返回 `ErrInvalidRange`；无法识别、截断、尾部拼接另一份材料或校验和失败返回 `ErrInvalidArchive`，失败不交付任何记录或字节。即使只复核较早决策，后续记录损坏也整份失败。归档内携带的检查点仅供参考，校验一律以调用方另行保留的检查点为准；读取成功只表示材料与检查点一致，原决策与重算决策的差异仍由离线复核报告。两个入口均只读、结果与输入相互脱离，不改变服务状态、不追加审计记录，仅用 Go 标准库且可离线使用。
- 新的查询、导出与复核入口缺少组织时一律返回 `ErrMissingOrganization` 且不改变状态。

## 命令行离线复核：`darksafe review`

`review` 子命令在原服务实例结束后，仅凭一份保存的审计归档复核其中**一条已有访问决策**，不联系运行中的服务，也不创建或恢复策略存储，更不会把该决策的请求重新提交。它顺序调用 `DecodeAuditArchive` 与 `RecheckDecisionOffline`：先按另行保留的检查点校验整份材料，再用决策记录里保存的请求与其实际使用的历史策略版本重算。

调用参数（均为必填）：

| 参数 | 含义 |
| --- | --- |
| `--archive FILE` | `EncodeAuditArchive` 写出的归档文件路径；读取完整导出，不能是按主体筛选的分页或缺失开头的片段。 |
| `--org ORG` | 归档所属组织名；必须与记录绑定的组织一致，可包含空格等任意原始字节。 |
| `--seq N` | 要复核的决策记录在该组织内的序号，正整数（从 1 开始）。 |
| `--end-seq N` | **另行保留**的检查点截至序号（无记录组织为 0），不能为负。 |
| `--fingerprint HEX` | **另行保留**的检查点指纹（64 个十六进制字符）。支持 `--flag value` 与 `--flag=value` 两种写法；`darksafe review --help` 可查看完整说明。

要点：

- 归档内嵌的检查点仅供参考，校验一律以命令行另行提供的 `--end-seq/--fingerprint` 为准。即使 `--seq` 指向靠前的记录，归档中的**全部**记录都会校验；后续记录损坏、记录缺失或外部检查点不符都会整次失败，不输出部分决策。
- 重算严格使用该决策记录的请求与其当时版本；归档中即使存在后续发布或回滚，也绝不改用较新策略。
- 材料合法但原决策与重算决策不一致时，仍完整打印双方并明确标注 `consistent: no`，退出码为 **0**——不一致不是文件损坏。
- 成功时标准输出依次给出目标序号、原决策、重算决策；两份决策都包含允许与否（`allowed`）、理由（`reason`）、命中策略（`matched policies`）与实际策略版本（`policy version`），并以 `consistent: yes/no` 明确结论。
- 组织名、命中策略标识与理由可能含空格、控制字符或非 UTF-8 字节；输出使用 Go 风格引号（`%q`）逐字节表示，单个 `0xFF` 显示为 `\xff`、`0xFE` 显示为 `\xfe`、真正的 U+FFFD 仍显示为 `�`，不同非法字节不会被替换成同一个字符。
- 该命令只读指定归档，不改写归档或检查点，也不追加审计记录。

退出码：`0` 已得到完整复核结果（含不一致）；`1` 归档校验失败（`ErrInvalidArchive`/`ErrInvalidRange`）、目标序号不存在（`ErrAuditNotFound`）、目标不是决策记录（`ErrAuditNotADecision`）或历史策略版本无法取得（`ErrVersionNotFound`），错误信息可区分且只写标准错误；`2` 文件无法读取、必填输入缺失、序号无法解析为整数、目标序号非正或截至序号为负，具体原因写标准错误。

归档、目标序号与指纹来自哪里、成功与失败输出如何解读，见下一节的端到端示例。

## 端到端示例：保存一条决策并离线复核

下面把前面各节串成一次完整流程，只围绕一个组织（`acme payments`，组织名含空格）、一份账本（资源标识 `ledger-2026-q3`）和一次读取请求：发布一条只允许指定主体读取这份账本的策略，通过组织级 `Decide` 得到允许结果，再把完整审计归档和与它对应的检查点分成两个文件保存。仓库内 `examples/save-decision/main.go` 就是这个可直接运行的程序（`go run ./examples/save-decision`），仅依赖项目公开功能与 Go 标准库，这里完整列出，关键步骤无一省略：

```go
// Command save-decision is a complete walkthrough of the offline review
// material flow. Around one organization, one ledger and one read request,
// it publishes a policy that allows only one subject to read that ledger,
// takes the allowed decision through the organization-level Decide entry
// point, then saves the full audit archive and its checkpoint as two
// separate files. Once the program exits, nothing needs to stay in memory:
// "darksafe review" recomputes the saved decision from those two files
// alone, without a Store and without re-submitting the request.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	// The organization name contains a space on purpose: every later call
	// must repeat it byte for byte, including the space.
	org = "acme payments"
	// The policy pins the ledger by its exact resource identifier; the
	// request below must use the same identifier and scope.
	ledgerID    = "ledger-2026-q3"
	ledgerScope = "org/payments/ledger"
	subjectID   = "u-1001"

	archivePath    = "acme-payments.audit"
	checkpointPath = "acme-payments.checkpoint"
)

func main() {
	log.SetFlags(0)
	store := darksafe.NewStore()

	// Step 1: publish version 1 — a single policy allowing only subjectID
	// to read this exact ledger. ResourceID narrows the allow to the one
	// ledger; it never replaces the subject, action and scope conditions.
	// A successful publish appends audit record 1 (a policy change record).
	version, err := store.Publish(org, 0, []darksafe.Policy{{
		ID:         "allow-u1001-read-ledger",
		Subject:    subjectID,
		Action:     "read",
		Scope:      ledgerScope,
		Effect:     darksafe.EffectAllow,
		ResourceID: ledgerID,
	}})
	if err != nil {
		log.Fatalf("publish policy set: %v", err)
	}

	// Step 2: take one real access decision at the organization level. Both
	// organizations must equal the decision organization. The allowed
	// decision appends audit record 2 (a decision record).
	decision := store.Decide(org, darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "user"},
		Resource:    darksafe.Resource{ID: ledgerID, Scope: ledgerScope},
		Action:      "read",
	})
	if !decision.Allowed {
		log.Fatalf("expected an allowed decision, got: %+v", decision)
	}

	// Step 3: export the complete audit chain together with its checkpoint.
	// The checkpoint (end sequence + fingerprint) is the independently
	// retained evidence the offline review validates against.
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		log.Fatalf("export audit chain: %v", err)
	}

	// The review target is the decision record's sequence in the audit
	// chain — not the policy version, not the number of access requests.
	// Here record 1 is the publish and record 2 is the decision; locate it
	// from the export instead of assuming a position.
	targetSeq := 0
	for _, r := range records {
		if r.Kind == darksafe.AuditDecision {
			targetSeq = r.Seq
		}
	}
	if targetSeq == 0 {
		log.Fatal("export contains no decision record")
	}

	// Step 4: serialize the export and save the archive and the checkpoint
	// as two separate files. The archive carries a copy of the checkpoint
	// for reference only; the review always validates against the
	// separately retained one, so the two files must both be kept.
	archive, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		log.Fatalf("encode audit archive: %v", err)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		log.Fatalf("write %s: %v", archivePath, err)
	}
	checkpointLine := fmt.Sprintf("%d %s\n", cp.EndSeq, cp.Fingerprint)
	if err := os.WriteFile(checkpointPath, []byte(checkpointLine), 0o600); err != nil {
		log.Fatalf("write %s: %v", checkpointPath, err)
	}

	fmt.Printf("published policy version %d in organization %q\n", version, org)
	fmt.Printf("decision: allowed=%v reason=%q matched=%q version=%d\n",
		decision.Allowed, decision.Reason, decision.Matched, decision.Version)
	fmt.Printf("saved %d audit records to %s\n", len(records), archivePath)
	fmt.Printf("saved checkpoint (end seq %d) separately to %s\n", cp.EndSeq, checkpointPath)
	fmt.Println()
	fmt.Println("the store can now be discarded; review the saved decision offline with:")
	fmt.Printf("  darksafe review --archive %s --org '%s' --seq %d --end-seq %d --fingerprint %s\n",
		archivePath, org, targetSeq, cp.EndSeq, cp.Fingerprint)
}
```

程序依次完成四步：

1. `NewStore` 建立内存存储，`Publish` 发布版本 1：唯一一条策略只允许主体 `u-1001` 读取资源标识为 `ledger-2026-q3` 的账本（`ResourceID` 把允许精确限定到这一份账本，主体、动作、作用域条件仍须同时满足）。发布成功在审计链上追加序号 1（策略变更记录）。
2. `Decide` 按当前版本对一次读取请求决策，得到允许结果，决策记录追加为序号 2。组织名（含空格）、账本资源标识与作用域在策略和请求中逐字节一致，后续每个调用也重复使用同样的值。
3. `AuditExport` 导出完整审计链，并返回与之对应的检查点（截至序号 + 指纹）。
4. `EncodeAuditArchive` 把导出序列化，归档写入 `acme-payments.audit`；检查点另行写入 `acme-payments.checkpoint`，两个文件**分开保存**。

运行后程序打印本次材料对应的完整 review 命令，其中的截至序号和指纹就是刚保存的检查点的实际值：

```text
$ go run ./examples/save-decision
published policy version 1 in organization "acme payments"
decision: allowed=true reason="matched allow policy" matched=["allow-u1001-read-ledger"] version=1
saved 2 audit records to acme-payments.audit
saved checkpoint (end seq 2) separately to acme-payments.checkpoint

the store can now be discarded; review the saved decision offline with:
  darksafe review --archive acme-payments.audit --org 'acme payments' --seq 2 --end-seq 2 --fingerprint 873b5fed3e3b27cd0852ddc0a7169238c2e4535f9a066d06ed4b852f92f74845
```

生成材料之后即可丢弃内存存储，也不需要重新提交访问请求；直接执行打印出的命令即可完成离线复核。示例程序的组织、策略与请求内容固定，因此指纹可以逐字节复现：

```bash
darksafe review \
  --archive acme-payments.audit \
  --org 'acme payments' \
  --seq 2 \
  --end-seq 2 \
  --fingerprint 873b5fed3e3b27cd0852ddc0a7169238c2e4535f9a066d06ed4b852f92f74845
```

### 目标序号、检查点与输出含义

- `--seq 2` 指向审计链中的**决策记录**。策略发布同样占用序号（本例序号 1 是策略变更记录），所以第一条决策记录的序号是 2；它既不是策略版本号（本例实际版本为 1），也不是第几次访问。
- `--end-seq 2` 与 `--fingerprint` 取自与归档**分开保存**的 `acme-payments.checkpoint`。归档内部也携带一份检查点，但仅供参考，复核时一律以这份独立保存的值为准，归档内嵌的检查点不能代替它。
- 成功时退出码为 0，标准输出依次是：
  - `target sequence`：被复核的决策记录序号；
  - `original decision`：决策记录里保存的、当时实际返回的决策；
  - `recomputed decision`：不创建存储、不重新提交请求，仅凭归档材料用该决策实际使用的历史版本重算出的决策；
  - 每份决策都含 `allowed`（允许与否）、`reason`（理由）、`matched policies`（命中策略标识）与 `policy version`（实际使用的策略版本）；
  - `consistent: yes/no`：两份决策全字段（允许与否、理由、命中策略、实际版本）比较后的一致性结论。
- 本例材料完整且未被改动，应得到允许结果与一致结论：

```text
target sequence: 2
original decision:
  allowed: true
  reason: "matched allow policy"
  matched policies: ["allow-u1001-read-ledger"]
  policy version: 1
recomputed decision:
  allowed: true
  reason: "matched allow policy"
  matched policies: ["allow-u1001-read-ledger"]
  policy version: 1
consistent: yes
```

### 两种容易选错材料的情况

以下两种情况都以退出码 **1** 结束，可区分的原因只写标准错误，且不输出任何部分复核结果：

- **把目标序号指向策略发布记录**：`--seq 1` 命中的是策略变更记录而不是决策记录，标准错误为 `review: darksafe: audit record is not a decision: sequence 1 is policy_change`。
- **提供与归档不符的外部检查点**：例如把 `--fingerprint` 改动任意一个字符，或 `--end-seq` 与保存的检查点不一致，整份材料校验失败，标准错误为 `review: archive validation failed: darksafe: invalid audit range: embedded checkpoint does not match the retained checkpoint`。




## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
