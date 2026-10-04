# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go test ./...
```

## 从演示转到组织级访问决策

顶部的 `demo`（`go run ./cmd/darksafe demo`）与组织级策略管理回答的都是“这个主体能不能访问”，但两者的**授权依据不同**，同一个主体对同一份资源完全可能得到相反结论：

| | 演示入口 `Access(subject, resource, action)` | 组织级入口 `store.Decide(org, OrgRequest)` |
| --- | --- | --- |
| 授权依据 | 主体**携带的角色** | 决策组织**当前已发布版本里的策略** |
| `owner` / 作用域角色（如 `acme/ledger:read`） | 唯一依据，`owner` 放行一切 | **完全不参与评估**，不能代替允许策略 |
| 命中列表 `Matched` 的含义 | **授予访问的角色名** | **命中的策略标识**（`Policy.ID`） |
| 版本 `Version` | 恒为 `0`：从不评估已发布策略 | 实际评估的发布版本；未发布或在信封阶段被拒时为 `0` |
| 策略存储与审计 | 不创建 `Store`、不发布策略、**不产生组织审计记录** | 需要 `NewStore()`；决策组织非空时，**允许和拒绝都记录**完整请求与结果 |

因此**不要把演示里的 `owner` 角色当成组织级管理权限**：`owner` 只在角色演示 `Access` 中放行；组织级 `Decide` 只认真正发布的策略，主体角色改什么都不影响组织策略——反过来，修改演示主体的角色也绝不会修改任何组织策略。

关于版本号要特别注意：**版本为 `0` 只表示这次决策没有使用任何已发布策略集**，不能据此判断请求允许与否。它既出现在“组织尚未发布策略”的拒绝上，也出现在主体停用、组织不一致等在评估策略之前就被信封拒绝的请求上。要区分“尚未发布”与“发布了但没有匹配允许策略”，应同时看理由与版本：对一个合法请求发布一条**空策略集**也会生成真实版本，结果仍默认拒绝（理由 `no matching allow policy`），但版本标明的是那个空集的版本号而不是 `0`。

两个直接影响访问结果的边界（都发生在策略评估之前，故**不使用策略、版本为 `0`、命中列表为空**）：

- **主体停用**：即使已存在与主体、动作、作用域完全匹配的允许策略，`Disabled` 的主体仍被拒绝，理由 `subject is disabled`。
- **组织不一致**：`SubjectOrg` 或 `ResourceOrg` 任一不等于决策组织即被拒绝，理由 `organization mismatch`。决策要求决策组织、主体组织、资源组织三者一致。

### 完整示例：同一个 `owner` 主体读同一份账本

下面程序 [`examples/org_access/main.go`](examples/org_access/main.go) 只用本项目公开 API 与 Go 标准库，无参数、无文件和网络访问，在本机离线运行：

```go
// Command org_access_example shows why the role-based demo Access and the
// organization-level Store.Decide can reach opposite conclusions for the very
// same subject and resource. One subject (carrying the owner role) reads one
// ledger (with a legal scope) through both entry points; the program then
// publishes the matching allow policy and replays the same request, and
// finally shows the two boundaries that reject even a "would match" request
// before any policy is consulted.
//
// Run with:
//
//	go run ./examples/org_access
//
// The program takes no arguments, performs no network or file access, and
// uses only this module's public API and the Go standard library. Every
// result is deterministic.
package main

import (
	"fmt"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	// One subject, one ledger, one action, reused byte-for-byte in every
	// call below. The subject carries "owner": the broadest role the
	// role-based demo core understands. The ledger scope is a legal
	// slash-separated path.
	const (
		org         = "acme"
		ledgerID    = "ledger-main"
		ledgerScope = "acme/ledger"
		subjectID   = "svc-ledger-owner"
		action      = "read"
	)
	subject := darksafe.Subject{
		ID:    subjectID,
		Kind:  "service",
		Roles: []string{"owner"}, // grants everything under Access; ignored by Decide
	}
	ledger := darksafe.Resource{ID: ledgerID, Scope: ledgerScope}

	// --- Step 1: the demo entry point ------------------------------------
	// Access is role-based and store-free. owner matches every action/scope,
	// so the read is allowed. The matched entry is the ROLE that granted
	// access, not a policy id. Access creates no Store, has no policy
	// versions and leaves no organization audit record.
	fmt.Println("1) demo: darksafe.Access (role-based, no Store)")
	d := darksafe.Access(subject, ledger, action)
	printDecision("   Access", d)
	fmt.Println("   matched entry = the role that grants access: owner")
	fmt.Println()

	// --- Step 2: the organization-level entry point, no policy yet --------
	// A fresh Store has never had a policy published for the org. The
	// decision organization, the subject organization and the resource
	// organization are all the same "acme", so the request envelope is
	// legal; roles (including owner) are simply not consulted. With no
	// published version the result is a denial whose version is 0.
	store := darksafe.NewStore()
	baseReq := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     subject,
		Resource:    ledger,
		Action:      action,
	}
	fmt.Println("2) organization: Store.Decide before any policy is published")
	fmt.Printf("   current version before publish: %d\n", store.CurrentVersion(org))
	d = store.Decide(org, baseReq)
	printDecision("   Decide", d)
	fmt.Println("   version 0 means no published policy set was evaluated;")
	fmt.Println("   it does not by itself say the request is allowed or denied.")
	fmt.Println()

	// --- Step 3: a published but EMPTY policy set -------------------------
	// Publishing an empty set still creates a real version (1). A legal
	// request evaluated against it is denied by default ("no matching allow
	// policy"), but the decision now reports that empty set's version, so the
	// caller can tell "denied by the published empty set" apart from "nothing
	// published yet".
	v1, err := store.Publish(org, 0, nil)
	if err != nil {
		panic(fmt.Sprintf("publish empty set as version 1: %v", err))
	}
	fmt.Printf("3) published an EMPTY policy set; it became version %d\n", v1)
	d = store.Decide(org, baseReq)
	printDecision("   Decide", d)
	fmt.Println("   owner is not consulted: an empty published set still denies by default,")
	fmt.Println("   and the decision names the empty set's version (1), not 0.")
	fmt.Println()

	// --- Step 4: publish the matching allow policy, then replay ----------
	// Version 2 holds one allow policy matching this exact subject, action
	// and ledger scope. Re-submitting the identical request is now allowed.
	// The matched entry is the POLICY IDENTIFIER, and the version is the one
	// actually evaluated (2). The subject still carries owner; it plays no
	// part in this result.
	v2, err := store.Publish(org, v1, []darksafe.Policy{{
		ID:      "p-ledger-read",
		Subject: subjectID,
		Action:  action,
		Scope:   ledgerScope,
		Effect:  darksafe.EffectAllow,
	}})
	if err != nil {
		panic(fmt.Sprintf("publish allow policy as version 2: %v", err))
	}
	fmt.Printf("4) published allow policy %q; it became version %d\n", "p-ledger-read", v2)
	d = store.Decide(org, baseReq)
	printDecision("   Decide", d)
	fmt.Println("   matched entry = the policy identifier p-ledger-read, not a role.")
	fmt.Println()

	// --- Boundary A: a matching allow policy does not save a disabled ----
	// subject. The envelope rejects before the policy set is evaluated, so no
	// policy is used: version stays 0, the matched list is empty, and the
	// reason distinguishes the cause.
	fmt.Println("5) boundary A: subject disabled even though the allow policy matches")
	disabledReq := baseReq
	disabledReq.Subject = darksafe.Subject{
		ID:       subjectID,
		Kind:     "service",
		Roles:    []string{"owner"}, // still present, still irrelevant
		Disabled: true,
	}
	d = store.Decide(org, disabledReq)
	printDecision("   Decide", d)
	fmt.Println("   rejected before policy evaluation: version 0, no matched policy.")
	fmt.Println()

	// --- Boundary B: subject and resource must belong to the deciding ----
	// org. Moving either one to another organization rejects at the envelope,
	// again with version 0 and an empty matched list. Two one-field changes
	// are shown: subject org first, then resource org.
	other := "globex"
	fmt.Println("6) boundary B: subject org or resource org differs from the decision org")
	subjectOtherReq := baseReq
	subjectOtherReq.SubjectOrg = other
	d = store.Decide(org, subjectOtherReq)
	printDecision("   subjectOrg=globex", d)

	resourceOtherReq := baseReq
	resourceOtherReq.ResourceOrg = other
	d = store.Decide(org, resourceOtherReq)
	printDecision("   resourceOrg=globex", d)
	fmt.Println("   neither request reaches the policies: version 0, no matched policy.")
	fmt.Println()

	// --- Audit behavior --------------------------------------------------
	// Access never created a Store and left nothing. On this Store, every
	// Decide with the non-empty decision org "acme" was recorded (allowed and
	// denied alike), as was each successful Publish. Exporting the chain makes
	// that visible; Review/RecheckDecisionOffline stay read-only and add none.
	records, _, err := store.AuditExport(org, 0)
	if err != nil {
		panic(fmt.Sprintf("export audit chain: %v", err))
	}
	fmt.Printf("7) audit: %d records for %q from this run\n", len(records), org)
	fmt.Println("   (2 policy publishes + 6 Decide calls; the demo Access left none)")
	for _, r := range records {
		switch r.Kind {
		case darksafe.AuditPolicyChange:
			fmt.Printf("   seq %d: %s version=%d\n", r.Seq, r.Kind, r.Change.Version)
		case darksafe.AuditDecision:
			fmt.Printf("   seq %d: %s allowed=%v version=%d\n",
				r.Seq, r.Kind, r.Decision.Decision.Allowed, r.Decision.Decision.Version)
		}
	}
}

// printDecision renders the four fields every decision carries, so the
// differences between Access and Decide are visible at a glance. The matched
// list is quoted element by element to stay readable when empty.
func printDecision(label string, d darksafe.Decision) {
	fmt.Printf("%s: allowed=%v reason=%q matched=%v version=%d\n",
		label, d.Allowed, d.Reason, quoteList(d.Matched), d.Version)
}

// quoteList renders a hit list as Go-quoted elements: [] for none,
// ["owner"] / ["p-ledger-read"] otherwise. Access hits are role names;
// Decide hits are policy identifiers.
func quoteList(items []string) string {
	out := "["
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", s)
	}
	return out + "]"
}
```

在模块根目录运行（输出确定，重复运行逐字节相同）：

```bash
go run ./examples/org_access
```

对应输出：

```text
1) demo: darksafe.Access (role-based, no Store)
   Access: allowed=true reason="role grants read:acme/ledger" matched=["owner"] version=0
   matched entry = the role that grants access: owner

2) organization: Store.Decide before any policy is published
   current version before publish: 0
   Decide: allowed=false reason="organization has no published version" matched=[] version=0
   version 0 means no published policy set was evaluated;
   it does not by itself say the request is allowed or denied.

3) published an EMPTY policy set; it became version 1
   Decide: allowed=false reason="no matching allow policy" matched=[] version=1
   owner is not consulted: an empty published set still denies by default,
   and the decision names the empty set's version (1), not 0.

4) published allow policy "p-ledger-read"; it became version 2
   Decide: allowed=true reason="matched allow policy" matched=["p-ledger-read"] version=2
   matched entry = the policy identifier p-ledger-read, not a role.

5) boundary A: subject disabled even though the allow policy matches
   Decide: allowed=false reason="subject is disabled" matched=[] version=0
   rejected before policy evaluation: version 0, no matched policy.

6) boundary B: subject org or resource org differs from the decision org
   subjectOrg=globex: allowed=false reason="organization mismatch" matched=[] version=0
   resourceOrg=globex: allowed=false reason="organization mismatch" matched=[] version=0
   neither request reaches the policies: version 0, no matched policy.

7) audit: 8 records for "acme" from this run
   (2 policy publishes + 6 Decide calls; the demo Access left none)
   seq 1: decision allowed=false version=0
   seq 2: policy_change version=1
   seq 3: decision allowed=false version=1
   seq 4: policy_change version=2
   seq 5: decision allowed=true version=2
   seq 6: decision allowed=false version=0
   seq 7: decision allowed=false version=0
   seq 8: decision allowed=false version=0
```

逐段对应：

1. **`Access` 允许**：主体携带 `owner`，命中项是**授予访问的角色** `owner`；没有 `Store`，版本恒为 `0`，也没有任何审计记录。
2. **未发布时 `Decide` 拒绝**：新建 `Store`，决策组织、主体组织、资源组织都是 `acme`，请求合法，但该组织当前版本为 0、无策略可评估，理由 `organization has no published version`，实际版本 `0`。同一主体在第 1 步被允许、这里被拒绝，差异完全来自授权依据而非主体变化。
3. **空策略集仍拒绝**：发布空集成为版本 1；默认拒绝理由变为 `no matching allow policy`，而版本标为 **1**——这是“按已发布空集评估后拒绝”，与第 2 步的“尚未发布”可由版本区分。
4. **发布匹配允许策略后允许**：版本 2 的 `p-ledger-read` 与主体 `svc-ledger-owner`、动作 `read`、作用域 `acme/ledger` 精确匹配；命中项是**策略标识** `p-ledger-read`，实际发布版本为 2。主体身上的 `owner` 在这一结论中不起作用。
5. **停用边界**：即便版本 2 已有匹配允许策略，停用主体仍被拒，理由 `subject is disabled`，版本 `0`、命中为空——请求未进入策略评估。
6. **组织边界**：把主体组织或资源组织改成 `globex` 均拒绝，理由 `organization mismatch`，同样版本 `0`、命中为空。
7. **审计差异**：演示 `Access` 不留痕迹；而这个 `Store` 上 2 次成功发布与 6 次非空组织的 `Decide`（含全部拒绝）共留下 8 条记录，按发生顺序交错编号（第 1 条决策早于第 2 条发布，因此决策记录可以排在策略变更记录之前）。只读的 `Review` 与离线复核不会新增记录。

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

## 完整示例：先保存一条真实决策，再离线复核

下面的流程只围绕**一个组织、一份账本、一次读取请求**，全部材料都可在本机离线生成；完整程序是 [`examples/offline_review/main.go`](examples/offline_review/main.go)，只依赖本项目公开 API 与 Go 标准库。场景取值在后续每个调用中都保持逐字节一致：

- 组织名 `acme factory`（**包含空格**，命令行必须加引号）。
- 账本资源 ID `ledger-2026`，作用域 `acme/factory/ledger`。
- 策略 `p-ledger-read-2026` 只允许主体 `svc-audit-reader` 读取**这份**账本：动作 `read`、作用域精确匹配，且 `ResourceID` 明确限定为 `ledger-2026`。
- 通过组织级 `Decide` 做一次读取决策得到允许结果，随后导出完整审计链、编码归档，并把检查点保存到**另一个文件**。

### 第一步：保存访问决策、归档与独立检查点

```go
// Command offline_review_example is the end-to-end worked example for
// `darksafe review`. It makes one real access decision, saves the complete
// audit archive and a separately retained checkpoint, and prints the exact
// review command to run next.
package main

import (
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

const (
	archivePath    = "acme-factory.audit"
	checkpointPath = "acme-factory.checkpoint"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "offline review example: failed:", err)
		os.Exit(1)
	}
}

func run() error {
	const org = "acme factory" // 组织名含空格，所有调用必须逐字节一致
	const (
		ledgerID    = "ledger-2026"
		ledgerScope = "acme/factory/ledger"
		subjectID   = "svc-audit-reader"
	)

	store := darksafe.NewStore()

	// 发布一条只允许指定主体读取这份账本的策略；0 表示该组织此前未发布过。
	published, err := store.Publish(org, 0, []darksafe.Policy{{
		ID:         "p-ledger-read-2026",
		Subject:    subjectID,
		Action:     "read",
		Scope:      ledgerScope,
		Effect:     darksafe.EffectAllow,
		ResourceID: ledgerID, // 明确限定账本的资源标识
	}})
	if err != nil {
		return fmt.Errorf("publish ledger-read policy as version 1: %w", err)
	}

	// 通过组织级决策功能发起一次读取请求；两个组织字段都等于决策组织。
	req := darksafe.OrgRequest{
		SubjectOrg:  org,
		ResourceOrg: org,
		Subject:     darksafe.Subject{ID: subjectID, Kind: "service"},
		Resource:    darksafe.Resource{ID: ledgerID, Scope: ledgerScope},
		Action:      "read",
	}
	decision := store.Decide(org, req)
	if !decision.Allowed {
		return fmt.Errorf("expected the read to be allowed, got denial: %+v", decision)
	}
	fmt.Printf("online decision: allowed=%v reason=%q matched=%v policy version=%d\n",
		decision.Allowed, decision.Reason, decision.Matched, decision.Version)
	fmt.Printf("published policy became version %d\n", published)

	// 导出完整审计链（序号 1 的发布记录 + 序号 2 的决策记录）与对应检查点；
	// 返回的副本与内存 Store 完全脱离。
	records, cp, err := store.AuditExport(org, 0)
	if err != nil {
		return fmt.Errorf("export complete audit chain: %w", err)
	}
	if len(records) != 2 ||
		records[0].Kind != darksafe.AuditPolicyChange ||
		records[1].Kind != darksafe.AuditDecision {
		return fmt.Errorf("expected seq 1 publish + seq 2 decision, got %+v", records)
	}
	targetSeq := records[1].Seq // 决策记录序号：发布占用 1，决策是 2

	// 保存完整审计归档。
	archive, err := darksafe.EncodeAuditArchive(org, records, cp)
	if err != nil {
		return fmt.Errorf("encode audit archive: %w", err)
	}
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		return fmt.Errorf("write archive %s: %w", archivePath, err)
	}

	// 检查点与归档分开保存。归档内部也携带一份检查点，但仅供参考，
	// 永远不能代替这份外部材料。
	content := fmt.Sprintf("org=%q\nend_seq=%d\nfingerprint=%s\n", cp.Org, cp.EndSeq, cp.Fingerprint)
	if err := os.WriteFile(checkpointPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write checkpoint %s: %w", checkpointPath, err)
	}

	fmt.Printf("saved archive %s and separately retained checkpoint %s\n", archivePath, checkpointPath)
	fmt.Printf("review: --seq %d --end-seq %d --fingerprint %s\n", targetSeq, cp.EndSeq, cp.Fingerprint)
	return nil
}
```

在模块根目录运行（材料写入当前工作目录；输出是确定性的，重复运行得到相同字节与指纹）：

```bash
go run ./examples/offline_review
```

第一步结束后磁盘上有两份**相互独立**的材料：

- `acme-factory.audit`：完整审计归档。链中两条记录——**序号 1 是策略发布记录，序号 2 才是读取决策记录**。
- `acme-factory.checkpoint`：另行保留的检查点（组织、截至序号、指纹）。复核一律以它为准；归档内嵌的检查点只是参考信息，不能代替这份外部材料。

检查点文件内容：

```text
org="acme factory"
end_seq=2
fingerprint=04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299
```

### 第二步：仅凭保存的材料离线复核

材料生成后，原来的内存存储不再需要（进程结束也没关系），也**不需要重新提交访问请求**。在保存材料的目录直接执行：

```bash
go run ./cmd/darksafe review \
  --archive acme-factory.audit \
  --org 'acme factory' \
  --seq 2 \
  --end-seq 2 \
  --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299
```

其中文件路径、组织、目标序号、截至序号与指纹全部对应第一步刚保存的材料。`--seq 2` 是目标记录在**该组织审计链中的序号**：策略发布也占用序号，所以读取决策落在 2。它**既不是策略版本号**（决策实际使用的策略版本是输出中的 `policy version: 1`），**也不是第几次访问**（本例只访问了一次，序号仍然是 2）。

成功时退出码为 0，标准输出依次为：

```text
target sequence: 2
original decision:
  allowed: true
  reason: "matched allow policy"
  matched policies: ["p-ledger-read-2026"]
  policy version: 1
recomputed decision:
  allowed: true
  reason: "matched allow policy"
  matched policies: ["p-ledger-read-2026"]
  policy version: 1
consistent: yes
```

各部分表达的含义：

- `target sequence`：复核目标在审计链中的序号（本例为 2，即决策记录）。
- **原决策（original decision）**：决策记录中保存的、当时在线实际返回的结果。
- **重算决策（recomputed decision）**：复核时仅凭归档——用记录里保存的请求、以及目标之前策略变更记录携带的版本 1 完整策略——重新计算的结果；归档中即使存在更新版本也绝不使用。
- **命中策略（matched policies）**：实际命中的策略标识；本例是明确限定本账本资源 ID 的 `p-ledger-read-2026`。
- **实际版本（policy version）**：决策实际使用的策略版本，本例为 1。
- **一致性结论（consistent）**：原决策与重算决策在允许与否、理由、命中策略、实际版本上逐字段比较的结果。

本例应得到**允许结果**（两份决策均为 `allowed: true`，理由 `matched allow policy`，命中 `p-ledger-read-2026`）与 **`consistent: yes`**：历史材料重算出的结论与当时记录完全一致。

### 两种容易选错材料的情况

以下两种错误都以退出码 **1** 结束，**只在标准错误**报告可区分的原因，标准输出为空、不输出任何部分复核结果：

1. **目标序号指向策略发布记录**：把 `--seq 2` 错写成 `--seq 1`。序号 1 是 `policy_change` 记录，不是决策记录，对应 `ErrAuditNotADecision`：

   ```bash
   go run ./cmd/darksafe review --archive acme-factory.audit \
     --org 'acme factory' --seq 1 --end-seq 2 \
     --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299
   # 退出码 1；标准输出为空；标准错误：
   # review: darksafe: audit record is not a decision: sequence 1 is policy_change
   ```

2. **提供与归档不符的外部检查点**：例如指纹被改动，或拿了另一份归档的检查点。整份材料链校验失败，对应 `ErrInvalidRange`，不会打印任何决策：

   ```bash
   go run ./cmd/darksafe review --archive acme-factory.audit \
     --org 'acme factory' --seq 2 --end-seq 2 \
     --fingerprint 0000000000000000000000000000000000000000000000000000000000000000
   # 退出码 1；标准输出为空；标准错误：
   # review: archive validation failed: darksafe: invalid audit range: embedded checkpoint does not match the retained checkpoint
   ```

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
