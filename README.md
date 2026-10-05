# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go test ./...
```

## 从演示到组织级访问决策

演示入口（`go run ./cmd/darksafe demo`，即 `darksafe.Access`）与组织级决策（`Store.Decide`）是**两套不同的授权依据**，同一主体在两边得到不同结论是正常的，不要把其中一边的结论当成另一边的依据：

- `Access` 只看主体携带的**角色**：角色为 `owner` 或 `<作用域>:<动作>` 即允许。它不读取任何已发布策略，不创建策略存储，也不产生组织审计记录；结果中的命中项（`Matched`）是**授予访问的角色名**。
- `Decide` 只看决策组织**当前已发布的策略**：请求中的主体角色——即使包含 `owner` 或与作用域对应的角色——完全不参与评估，不能代替允许策略；结果中的命中项是**策略标识**。每次指定非空决策组织的 `Decide`，无论允许还是拒绝，都会在该组织的审计链追加一条记录请求与结果的决策记录。
- 两边的 `Version` 含义一致：实际用于评估的策略版本。`0` 表示**没有使用任何已发布策略**（`Access` 恒为 0；信封检查失败或组织尚未发布策略的 `Decide` 也是 0），不能仅凭版本 0 判断请求是否被允许。
- 已发布的**空策略集**同样默认拒绝（理由 `no matching allow policy`），但合法请求的结果会标明该空集的版本号——这与"尚未发布任何策略"的版本 0 可以区分。
- 更改演示主体的角色只影响 `Access` 的结论，不会修改任何组织策略；反之，发布或回滚组织策略也不会改变 `Access` 的结果。

### 完整示例：同一主体、同一账本、两种依据

[`examples/org_decision/main.go`](examples/org_decision/main.go) 可在本机离线运行，只依赖本项目公开 API 与 Go 标准库。一个携带 `owner` 角色的主体 `svc-audit-reader` 读取账本 `ledger-2026`（作用域 `acme/factory/ledger`，决策组织、主体组织、资源组织均为 `acme factory`）：

```bash
go run ./examples/org_decision
```

输出（每行依次给出允许与否、理由、命中列表与实际版本）：

```text
access  (role owner, no policy store): allowed=true reason="role grants read:acme/factory/ledger" matched=["owner"] version=0
decide  (no published policy)      : allowed=false reason="organization has no published version" matched=[] version=0
published allow policy p-ledger-read-2026 as version 1
decide  (allow policy published)   : allowed=true reason="matched allow policy" matched=["p-ledger-read-2026"] version=1
decide  (subject disabled)         : allowed=false reason="subject is disabled" matched=[] version=0
decide  (subject org mismatch)     : allowed=false reason="organization mismatch" matched=[] version=0
```

逐行理解：

1. **`Access` 允许**：`owner` 角色足以通过演示核心，命中项是角色名 `owner`；它从不评估策略，版本恒为 0。
2. **`Decide` 拒绝（尚未发布策略）**：请求信封完全合法、三个组织一致，但组织级决策只认已发布策略，此时一条都没有，默认拒绝；版本 0 表示没有使用任何已发布策略，命中列表为空。
3. **发布匹配策略后 `Decide` 允许**：发布的 `p-ledger-read-2026`（主体 `svc-audit-reader`、动作 `read`、作用域 `acme/factory/ledger`、效果 allow）成为版本 1；同一请求再次提交后命中该策略，命中项是策略标识，实际版本为 1。注意允许来自策略而非 `owner` 角色——把请求中的角色全部去掉，结论不变。
4. **主体停用**：即使版本 1 的允许策略仍然匹配，信封检查先拒绝，理由 `subject is disabled`；未评估任何策略，版本 0、无命中项。
5. **组织不一致**：主体组织（或资源组织）改为其他组织即与决策组织不一致，信封检查拒绝，理由 `organization mismatch`；同样未评估策略，版本 0、无命中项。

这两条边界（第 4、5 行）说明：组织级请求先过信封检查再谈策略，版本 0 的拒绝与"策略评估后拒绝"是两类情况。示例中的四次 `Decide` 都指定了非空决策组织，因此无论允许还是拒绝，都会在 `acme factory` 的审计链各留下一条决策记录（加上发布记录共 5 条）；而第 1 行的 `Access` 不产生任何审计记录。

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

### 跨作用域示例：上级递归拒绝为什么压过下级精确允许

`Decide` 的冲突规则不是"更具体者胜"，而是**拒绝优先（deny-overrides）**：先按主体、动作、作用域（含 `Recursive` 子作用域）与可选资源限定找出**全部**命中策略；只要其中有一条拒绝，结果就是拒绝（理由 `matched deny policy`），作用域更精确的允许不会把它"覆盖"掉。命中列表保留**所有**命中策略的标识并按标识升序排列——包括被压过的那条允许，它是冲突可复核的证据，而不是被最终决定抹掉。完整跨层级示例是 [`examples/scope_deny_override/main.go`](examples/scope_deny_override/main.go)，只依赖本项目公开 API 与 Go 标准库，自行创建内存存储并真实发布策略：

```bash
go run ./examples/scope_deny_override
```

这里描述的是**组织级已发布策略**对 `Store.Decide` 的判断规则；演示入口 `Access` 的主体角色（包括 `owner`）在这里完全不参与评估，示例主体因此不带任何角色。

场景固定为一个组织、一个启用主体、一份资源的读取操作：决策组织、主体组织、资源组织均为 `acme`；主体 `u1`（启用）、资源 `doc-1`、动作 `read`。同一程序连续发布三个完整策略集（版本 1、2、3），每个结果都打印允许与否、理由、命中策略与**此次实际评估的已发布版本**。

**情形 1（版本 1，拒绝）**：同一次发布包含两条策略，都针对主体 `u1`、动作 `read`，均不限定具体资源标识（`resource-id=""`）：

- `a-child-allow`：作用域 `org/a/b`、效果 allow、`Recursive=false`（精确允许下级读取）；
- `z-parent-deny`：作用域 `org/a`、效果 deny、`Recursive=true`（递归拒绝覆盖整个子树）。

请求资源位于 `org/a/b`。子作用域精确命中允许，递归的上级拒绝也命中该子作用域——拒绝优先，`Decide` 返回拒绝；命中列表按标识升序同时包含两条策略（`a-child-allow` 在 `z-parent-deny` 之前是按标识排序，与发布顺序无关），版本为实际评估的 1。

**情形 2（版本 2，对照一：上级拒绝改为不递归）**：把上级拒绝重新发布为 `Recursive=false`，下级允许不变，请求完全相同。不递归策略只覆盖自身作用域 `org/a`，对 `org/a/b` 的读取它**不命中**；结果只命中下级允许 `a-child-allow` 并通过，版本 2。注意：起作用的是这条命中的允许——拒绝未命中**不等于自动允许**，若此时没有任何允许策略命中，结果仍是默认拒绝（理由 `no matching allow policy`）。

**情形 3（版本 3，对照二：`org/ab` 不是 `org/a` 的下级）**：恢复 `org/a` 的递归拒绝，另为 `org/ab` 配置一条精确允许 `ab-exact-allow`，请求读取 `org/ab` 下的 `doc-1`。作用域层级按**斜杠分隔的段**匹配：递归覆盖要求请求作用域等于策略作用域或以"策略作用域 + `/`"开头，所以 `org/ab` 与 `org/a` 只是名称前缀相近的**同级**，不是后代；递归拒绝不能命中，读取只命中 `org/ab` 上这条精确允许并通过，版本 3。不能把作用域当字符串前缀理解（否则会误以为 `org/a` 管到 `org/ab`），也不能把未命中拒绝当作放行依据。

预期输出（确定性，重复运行逐字节一致；策略集与请求紧邻决策打印，可直接对照）：

```text
case 1: recursive parent deny vs more specific child allow
  published as version 1 (2 policies):
    policy id="a-child-allow" subject="u1" action="read" scope="org/a/b" effect="allow" recursive=false resource-id=""
    policy id="z-parent-deny" subject="u1" action="read" scope="org/a" effect="deny" recursive=true resource-id=""
  request: decision-org="acme" subject-org="acme" resource-org="acme" subject="u1" disabled=false resource="doc-1" scope="org/a/b" action="read"
  decide : allowed=false reason="matched deny policy" matched=["a-child-allow" "z-parent-deny"] version=1

case 2: parent deny made non-recursive, same child read
  published as version 2 (2 policies):
    policy id="a-child-allow" subject="u1" action="read" scope="org/a/b" effect="allow" recursive=false resource-id=""
    policy id="z-parent-deny" subject="u1" action="read" scope="org/a" effect="deny" recursive=false resource-id=""
  request: decision-org="acme" subject-org="acme" resource-org="acme" subject="u1" disabled=false resource="doc-1" scope="org/a/b" action="read"
  decide : allowed=true reason="matched allow policy" matched=["a-child-allow"] version=2

case 3: recursive deny on org/a cannot reach the sibling scope org/ab
  published as version 3 (2 policies):
    policy id="z-parent-deny" subject="u1" action="read" scope="org/a" effect="deny" recursive=true resource-id=""
    policy id="ab-exact-allow" subject="u1" action="read" scope="org/ab" effect="allow" recursive=false resource-id=""
  request: decision-org="acme" subject-org="acme" resource-org="acme" subject="u1" disabled=false resource="doc-1" scope="org/ab" action="read"
  decide : allowed=true reason="matched allow policy" matched=["ab-exact-allow"] version=3
```

归纳：拒绝优先是**命中集合内**的效果裁决，不按作用域深浅或发布顺序比较；`Recursive` 决定一条策略的作用域是否覆盖斜杠分段意义上的后代；命中列表始终是全部命中策略标识的升序去重列表；`version` 是这次评估实际使用的已发布版本（三个情形分别为 1、2、3）。

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

## 完整示例：把某个主体的决策逐页取完

审计员常见的需求是：从一个组织的审计链里，把**某一个主体**的决策记录（允许与拒绝都算）按原顺序全部取出来。`AuditQuery` + `AuditPage` 就是为此设计的分页视图。完整程序是 [`examples/audit_subject_pagination/main.go`](examples/audit_subject_pagination/main.go)，只依赖本项目公开 API 与 Go 标准库，在本机离线运行、自行生成全部材料：

```bash
go run ./examples/audit_subject_pagination
```

示例围绕组织 `acme factory` 里的目标主体 `svc-audit-reader`。程序先用现有公开功能造出一条 10 记录的链：目标主体的允许（序号 2、9）与拒绝（序号 4、7）决策，中间穿插着策略发布（序号 1、5）、回滚（序号 8）和另一主体 `svc-billing` 的决策（序号 3、6、10）。然后演示一次完整的分页过程。

### 用法要点

- **发起查询**：`AuditQuery(org, 1, 2, "", "svc-audit-reader")` —— 从序号 1 开始、每页最多 2 条**匹配**记录、不限类别、按主体筛选。目标主体在链里有 4 条决策，页大小 2 使查询确实需要翻页。
- **页大小限制的是匹配记录数**，不是扫描过的链位置数：两条匹配记录之间隔着多少策略变更或其他主体的记录都不影响，它们只是被扫过，不会提前占满一页。
- **记录保留原始序号**：输出中的 `seq` 是记录在组织审计链里的真实序号，按递增顺序出现，不会重新编号成 1、2、3，也不会混入其他主体或策略变更记录。每条记录完整保留请求主体、允许与否、理由、命中策略和实际策略版本。
- **检查点钉住这次查询**：第一次 `AuditQuery` 返回的 `Checkpoint`（截至序号 + 指纹）把这次查询固定在当时的链尾。后续翻页把**同一个检查点**和上一页返回的 `Next` 一起交给 `AuditPage`，各页显示的截至序号与指纹始终相同。取到第一页后再为同一主体新增决策（示例中的序号 11），它**不会**进入这次查询；只有重新发起 `AuditQuery` 钉住新链尾，才能看到它。
- **翻页只看 `Next`**：`Next` 为 0 表示这次查询结束。不能靠已显示的条数推算下一位置——匹配记录在链上的间隔是任意的。若最后一条匹配记录之后仍有不匹配记录（示例中序号 10 是另一主体的决策），继续翻页会得到一页**没有记录的结束页**，这是正常结果，不能解释为材料丢失。
- **两个边界**：指定一个没有决策记录的主体时返回空页并结束（`Next` 为 0），绝不退回成未筛选的完整历史；用改动过指纹的检查点继续翻页时返回 `ErrInvalidRange`，不交付任何部分记录。
- **这只是查询视图**：按主体分页取出的是链上记录的筛选子集，不能代替离线复核所需的材料。离线复核必须用 `AuditExport` 导出**完整**链并另行保留检查点（见上文离线复核一节与 `examples/offline_review`）；按主体筛选的分页结果无法通过 `VerifyAudit` 的链校验。
- **查询与翻页是只读的**：不改变任何组织的当前策略版本，也不追加审计记录；决策、归档与命令行复核等现有功能的用法不受影响。

### 预期输出

输出是确定性的，重复运行完全一致。先列出自造的完整链（便于对照哪些记录会被筛掉），再逐页展示两次查询与两个边界：

```text
audit chain of "acme factory" (10 records):
  seq=1  policy_change version=1 policies=2 rolled_back=false
  seq=2  decision     subject=svc-audit-reader action=read allowed=true
  seq=3  decision     subject=svc-billing action=read allowed=false
  seq=4  decision     subject=svc-audit-reader action=write allowed=false
  seq=5  policy_change version=2 policies=0 rolled_back=false
  seq=6  decision     subject=svc-billing action=read allowed=false
  seq=7  decision     subject=svc-audit-reader action=read allowed=false
  seq=8  policy_change version=3 policies=2 rolled_back=true
  seq=9  decision     subject=svc-audit-reader action=read allowed=true
  seq=10 decision     subject=svc-billing action=read allowed=false

pinned query: subject="svc-audit-reader" start=1 pageSize=2 (page size counts matching records only)
page 1 (pinned range 1-10, checkpoint end=10 fingerprint=2fb55d31cd2a598d9f729cb774321aa6b62005e0f7b00f8278dd2ce5f70bd878):
  seq=2 subject=svc-audit-reader allowed=true reason="matched allow policy" matched=["p-ledger-read-2026"] version=1
  seq=4 subject=svc-audit-reader allowed=false reason="matched deny policy" matched=["p-ledger-write-deny"] version=1
  next=5
appended after page 1: svc-audit-reader read -> allow (new seq 11)
page 2 (pinned range 5-10, checkpoint end=10 fingerprint=2fb55d31cd2a598d9f729cb774321aa6b62005e0f7b00f8278dd2ce5f70bd878):
  seq=7 subject=svc-audit-reader allowed=false reason="no matching allow policy" matched=[] version=2
  seq=9 subject=svc-audit-reader allowed=true reason="matched allow policy" matched=["p-ledger-read-2026"] version=3
  next=10
page 3 (pinned range 10-10, checkpoint end=10 fingerprint=2fb55d31cd2a598d9f729cb774321aa6b62005e0f7b00f8278dd2ce5f70bd878):
  (no matching records)
  next=0
next=0 ends the walk; the empty last page is normal: non-matching records remained after the last match

fresh query after the append (only a new query pins the new head):
page 1 (pinned range 1-11, checkpoint end=11 fingerprint=60112f84bdb4b40a531a1635b09ef5fc12a866d360e1bfeb86d6464ce26c0adc):
  seq=2 subject=svc-audit-reader allowed=true reason="matched allow policy" matched=["p-ledger-read-2026"] version=1
  seq=4 subject=svc-audit-reader allowed=false reason="matched deny policy" matched=["p-ledger-write-deny"] version=1
  next=5
page 2 (pinned range 5-11, checkpoint end=11 fingerprint=60112f84bdb4b40a531a1635b09ef5fc12a866d360e1bfeb86d6464ce26c0adc):
  seq=7 subject=svc-audit-reader allowed=false reason="no matching allow policy" matched=[] version=2
  seq=9 subject=svc-audit-reader allowed=true reason="matched allow policy" matched=["p-ledger-read-2026"] version=3
  next=10
page 3 (pinned range 10-11, checkpoint end=11 fingerprint=60112f84bdb4b40a531a1635b09ef5fc12a866d360e1bfeb86d6464ce26c0adc):
  seq=11 subject=svc-audit-reader allowed=true reason="matched allow policy" matched=["p-ledger-read-2026"] version=3
  next=0

boundary 1: subject with no decisions
  subject="svc-nobody": records=0 next=0 (empty page ends the walk; the unfiltered history is NOT returned)
boundary 2: checkpoint with an altered fingerprint
  AuditPage -> darksafe: invalid audit range: checkpoint fingerprint does not match the chain at sequence 10 (ErrInvalidRange; no partial records delivered)

all querying was read-only: chain still 11 records, current version still 3
```

对照输出理解这次分页：

1. **第一页**取到序号 2、4：序号 3 是另一主体的决策，被扫过但不占页内名额；`next=5` 是下一段扫描的起始链位置（不是"第几条匹配"）。检查点把这次查询钉在截至序号 10。
2. **追加序号 11 后继续这次查询**：第二、三页沿用第一页的检查点，截至序号与指纹逐字节相同，新增的序号 11 不出现。第二页取到序号 7（空策略集版本 2 下的默认拒绝）与 9（回滚后版本 3 下的允许）。
3. **空结束页**：序号 9 之后只剩不匹配的序号 10，第三页没有记录、`next=0`，查询正常结束——4 条目标记录（2、4、7、9）已按原序号递增取完，一条不缺。
4. **重新发起的查询**钉在截至序号 11（指纹随之不同），这次才在第三页看到序号 11。
5. **边界**：`svc-nobody` 没有决策记录，返回空页并结束；指纹被改动的检查点报 `ErrInvalidRange`，不交付部分记录。
6. 全部查询结束后链仍是 11 条记录、当前版本仍是 3：查询与翻页没有追加记录、没有改变策略版本。

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
