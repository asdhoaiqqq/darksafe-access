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

### 完整示例：一次失败的替换发布为什么没有改变已有授权

`Publish` 是**整套替换、先校验后提交**：一次提交里只要有一条策略非法，整批都不会生效——集内那条本身合法的拒绝策略也不会"先应用一半"。完整程序是 [`examples/publish_atomicity/main.go`](examples/publish_atomicity/main.go)，只依赖本项目公开 API 与 Go 标准库，自行建立内存存储与请求，在本机离线即可复现：

```bash
go run ./examples/publish_atomicity
```

场景固定为**同一组织、同一启用主体对同一资源的读取**：组织 `acme factory`，主体 `svc-billing`（启用、不带任何角色），资源 `billing-2026`，作用域 `acme/factory/billing`。决策组织、主体组织、资源组织三者一致，作用域合法，因此结论完全由该组织**当前已发布策略**按组织级默认拒绝、拒绝优先的规则决定。程序连续展示五步：

1. **发布一组允许读取的策略，成为版本 1**；同一读取被允许，理由 `matched allow policy`、命中标识 `p-billing-read-allow`、实际版本 1。发布与这次决策各自留下一条审计记录（序号 1 策略变更、序号 2 决策）。
2. **提交一组打算把读取改为拒绝的新策略集**：一条本身合法的拒绝策略 `p-billing-read-deny`，加一条作用域含**连续斜杠**的非法策略（`acme//factory/billing`）。`Publish` 明确返回 `ErrInvalidPolicySet`（而不是 `ErrVersionConflict`），返回版本为 0：合法的拒绝策略不会被先应用，也不占用新版本号；当前版本仍是 1，版本 2 不存在，版本 1 的内容原样保留。
3. **为观察效果，在失败提交之后用完全相同的请求再做一次 `Decide`**：读取**仍然被允许**，理由、命中标识与实际版本全部来自原策略（版本 1），不会出现只存在于失败集合里的标识。这里必须区分两件事：**失败的发布本身不增加任何审计记录，检查点（截至序号与指纹）逐字节不变**；而这次为观察效果发起的决策是另一次独立操作，它按已有规则正常留下一条**新的决策记录（序号 3）**——不能把这条记录算作失败发布产生的变更。
4. **把非法策略修正后，以未变的当前版本（`expectedVersion=1`）重新提交整组策略**：生成紧接原版本的**版本 2**，同一读取转为拒绝，输出明确的拒绝理由 `matched deny policy`、命中策略 `p-billing-read-deny` 与实际版本 2。版本 2 的策略变更记录保存的是**完整新策略集**；发布是整组**替换**而非把本次策略追加到旧集——v1 的允许策略既不在新集里也不再出现在命中列表。历史不可改写，`Policies(org, 1)` 仍能查询版本 1 的原内容。
5. **另一个影响提交的边界：策略内容合法但预期版本已经过期**。版本 2 已存在后仍以 `expectedVersion=1` 提交，返回 `ErrVersionConflict`：不覆盖当前策略、不推进当前版本，也不留下变更记录。使用者需要先依据 `CurrentVersion`/`Policies` 看清当前内容，再决定是否再次提交。

输出刻意把三个容易被当成同一个数的数字分成不同字段：

- `Publish -> new version N` 是**发布成功返回的新版本号**（失败时为 0，不返回可用版本）；
- 决策行的 `applied-version=N` 是**这次访问实际使用的策略版本**——失败提交之后它仍然是 1，发布成功之后才变成 2；
- `[audit]` 行的 `head-seq=N` 是**审计链记录序号**，每次成功发布和每次决策各占一个连续序号，与策略版本号没有数值上的对应关系（如版本 2 的发布是审计序号 4）。

预期输出（确定性，重复运行逐字节一致；指纹由记录内容决定，可直接对照两次"checkpoint unchanged"）：

```text
step 1: publish the allow-read policy set
  Publish -> new version 1
    policy id="p-billing-read-allow" subject="svc-billing" action="read" scope="acme/factory/billing" effect="allow" recursive=false resource-id=""
  Decide  -> allowed=true reason="matched allow policy" matched=["p-billing-read-allow"] applied-version=1
  [audit] after v1 publish + allow decision            records=2 head-seq=2 head-fingerprint=825ce1048b4fb3f1f120c9e2ef16421e090ff7d0a192b3c622c4d3d24671f60b

step 2: replace the set with a batch containing an illegal policy
    policy id="p-billing-read-deny" subject="svc-billing" action="read" scope="acme/factory/billing" effect="deny" recursive=false resource-id=""
    policy id="p-billing-read-extra" subject="svc-billing" action="read" scope="acme//factory/billing" effect="allow" recursive=false resource-id=""
  Publish -> returned version=0 err=darksafe: invalid policy set: policy "p-billing-read-extra" has invalid scope: scope "acme//factory/billing" contains consecutive slashes
  [audit] immediately after failed publish             records=2 head-seq=2 head-fingerprint=825ce1048b4fb3f1f120c9e2ef16421e090ff7d0a192b3c622c4d3d24671f60b
  [audit] checkpoint unchanged vs "after v1 publish + allow decision": head-seq=2 fingerprint=825ce1048b4fb3f1f120c9e2ef16421e090ff7d0a192b3c622c4d3d24671f60b

step 3: observe with the same read after the failed publish
  Decide  -> allowed=true reason="matched allow policy" matched=["p-billing-read-allow"] applied-version=1
  [audit] after the observation decision               records=3 head-seq=3 head-fingerprint=50cee1631861b4a4385bb702980088264b95874c0fe5b70dafb6fca30527c685
  the failed publish added no record; seq 3 comes from this separate Decide, not from the failure

step 4: fix the scope and resubmit the complete set against current version 1
    policy id="p-billing-read-deny" subject="svc-billing" action="read" scope="acme/factory/billing" effect="deny" recursive=false resource-id=""
    policy id="p-billing-read-extra" subject="svc-billing" action="read" scope="acme/factory/billing/audit" effect="allow" recursive=false resource-id=""
  Publish -> new version 2 (immediate successor of 1)
  Decide  -> allowed=false reason="matched deny policy" matched=["p-billing-read-deny"] applied-version=2
  version-2 change record stores the complete new set ["p-billing-read-deny" "p-billing-read-extra"] (nothing appended from v1)
  Policies(org, 1) still returns the original set ["p-billing-read-allow"]
  [audit] after v2 publish + deny decision             records=5 head-seq=5 head-fingerprint=b0462b288c2ec2cda884e63ee580879c329d77810de3517977d1db02b9ae68a2

step 5: submit legal content against a stale expected version
    policy id="p-billing-read-allow" subject="svc-billing" action="read" scope="acme/factory/billing" effect="allow" recursive=false resource-id=""
  Publish -> returned version=0 err=darksafe: version conflict: expected 1, current is 2
  current version stays 2 and its set is untouched; re-read it before resubmitting
  [audit] immediately after stale submit               records=5 head-seq=5 head-fingerprint=b0462b288c2ec2cda884e63ee580879c329d77810de3517977d1db02b9ae68a2
  [audit] checkpoint unchanged vs "after v2 publish + deny decision": head-seq=5 fingerprint=b0462b288c2ec2cda884e63ee580879c329d77810de3517977d1db02b9ae68a2
```

对照审计序号看这次过程（五步结束后共 5 条记录）：序号 1 是版本 1 的策略变更；序号 2 是第 1 步的允许决策；序号 3 是第 3 步观察决策——它与序号 2 的结论完全相同（同为版本 1 下的允许），但它是失败发布**之后**一次独立 `Decide` 留下的新记录；序号 4 是版本 2 的策略变更，记录里是完整新策略集；序号 5 是版本 2 下的拒绝决策。第 2 步的失败发布在 2 与 3 之间没有留下任何序号，第 5 步的过期提交在 5 之后也没有——两次失败后 `head-seq` 与指纹都保持不变，正是"失败提交不改变已有授权、也不改变审计链"的直接证据。

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
- `AuditQuery(org, startSeq, pageSize, kind, subject, resourceID...)`：按序号升序分页，可按类别、决策主体筛选（指定主体时只返回其决策记录），并可在末尾可选地传入**一个**资源标识条件（按决策记录中请求的资源标识筛选，见下节）；不传或传空字符串表示不设资源筛选，与旧调用完全一致。首次查询固定“截至序号 + 指纹”检查点，后续用 `AuditPage(checkpoint, nextSeq, ..., resourceID...)` 翻页（后续页须传入相同条件）；查询期间新增的记录不会混入。无记录组织返回空页与序号 0 的根指纹。页大小非正、起始序号非法、范围倒置、截至序号超出当前记录或指纹不符均返回明确错误；传入两个及以上资源条件返回 `ErrInvalidPage`。
- `AuditExport(org, endSeq)`：导出截至序号内的完整记录（空组织导出空集与根检查点），返回的副本与内部状态完全脱离。
- `VerifyAudit(org, records, checkpoint)`：纯函数，不依赖 Store，可离线校验。修改字段、删除中间或尾部记录、交换顺序、拼入其他组织记录都会验证失败。
- `RecheckDecision(org, seq)`：按决策记录里保存的请求与其实际使用的策略版本恢复结论，后续发布/回滚不影响结果；非决策记录返回 `ErrAuditNotADecision`，序号不存在返回 `ErrAuditNotFound`。
- `RecheckDecisionOffline(org, records, checkpoint, seq)`：纯函数，不创建或恢复 Store，服务实例结束后仅凭完整导出、单独保存的检查点和目标序号离线复核一条决策。整份输入先通过 `VerifyAudit` 链校验（目标之后的记录损坏、按主体筛选的记录、缺少开头的片段均失败），再以目标之前同组织策略变更记录携带的完整内容重放该请求实际使用的版本；版本缺失或只在目标之后出现返回 `ErrVersionNotFound`，绝不改用较新版本。返回原决策、重算决策及是否一致（允许与否、理由、命中策略、实际版本全字段比较，nil 与空命中列表视为一致）；链合法但两决策不同时明确标为不一致。该入口只读，不修改输入材料也不追加审计记录。
- 审计材料无损归档：`EncodeAuditArchive(org, records, checkpoint)` 把一次完整导出（截至某历史序号的前缀亦可，只要与检查点对应；无记录组织用序号 0 的根检查点）序列化为可保存到文件的字节；`DecodeAuditArchive(archive, org, checkpoint)` 在原服务实例结束后重新读取，返回可直接交给 `RecheckDecisionOffline` 的记录。格式为手写的长度前缀二进制编码（魔数 + 载荷长度 + 载荷 + SHA-256），所有字符串按原始字节还原——组织名、策略/资源标识、角色、命中列表与理由中的中文、控制字符、空格及非 UTF-8 字节均不替换不规整，单字节 0xFF、0xFE 与真正的 U+FFFD 仍可区分；nil 与非 nil 空列表、不存在的记录载荷均保留原有形态，因此重新读取不会改变指纹。编码与读取都先做整份材料校验：缺组织返回 `ErrMissingOrganization`，记录缺失、乱序、混入其他组织、内容改动、检查点不符返回 `ErrInvalidRange`；无法识别、截断、尾部拼接另一份材料或校验和失败返回 `ErrInvalidArchive`，失败不交付任何记录或字节。即使只复核较早决策，后续记录损坏也整份失败。归档内携带的检查点仅供参考，校验一律以调用方另行保留的检查点为准；读取成功只表示材料与检查点一致，原决策与重算决策的差异仍由离线复核报告。两个入口均只读、结果与输入相互脱离，不改变服务状态、不追加审计记录，仅用 Go 标准库且可离线使用。
- 单次校验的读取加复核组合：从归档出发一次性复核时使用 `DecodeVerifiedAuditArchive(archive, org, checkpoint)` 与 `RecheckVerifiedDecisionOffline(material, seq)`。前者在读取阶段对整份归档完成唯一一次完整链校验并返回已校验材料，后者直接按该材料复核目标决策、不再重复走链，因此一次命令调用只校验一次完整审计链。组合的失败原因、错误类型、报告字段、只读与脱离保证与两个独立入口完全一致；但 `VerifiedAuditMaterial` 只能由包内的校验入口产生，外部程序无法凭空构造，且不跨调用缓存——修改过材料或换过检查点后必须重新经 `DecodeVerifiedAuditArchive` 校验。单独使用 `DecodeAuditArchive` 或 `RecheckDecisionOffline` 的其他程序无需改动，二者仍各自独立完成完整校验，不要求先调用另一个入口。
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

## 完整示例：把某一份资源的访问记录逐页取完

查看**一份具体资源**（例如某一份账本）的访问历史时，按主体筛选仍要人工从各主体的记录里挑选。`AuditQuery`/`AuditPage` 因此支持一个可选的资源标识条件：在原有参数末尾多传一个资源标识即可，首次查询与后续翻页都使用同一个值。完整程序是 [`examples/audit_resource_pagination/main.go`](examples/audit_resource_pagination/main.go)：

```bash
go run ./examples/audit_resource_pagination
```

程序在组织 `acme factory` 里造出一条 9 记录的链：目标账本 `ledger-2026` 的允许（序号 2、8）与拒绝（序号 4 默认拒绝、5 主体停用、6 组织不一致），另一份账本 `ledger-2025` 的决策（序号 3、9），以及策略内容中出现 `ledger-2026` 的两次发布（序号 1、7）；另一个组织 `globex` 还使用了完全相同的资源标识。

用法要点（与按主体筛选同一套分页语义）：

- **发起查询**：`AuditQuery(org, 1, 2, "", "", "ledger-2026")` —— 末尾的资源标识是可选条件；不传或传 `""` 就是原来的未筛选查询，返回内容与分页行为逐字节不变。后续页用 `AuditPage(checkpoint, next, 2, "", "", "ledger-2026")`，条件须与首次一致。
- **匹配的是决策记录里请求的资源标识**：允许与拒绝都保留，包括主体停用（序号 5）与组织不一致（序号 6）这类信封拒绝——它们没有评估策略，但同样记录了请求的资源标识。同一标识位于不同作用域的决策也会返回：条件只针对资源标识，不解释作用域路径。
- **策略变更永不匹配**：即使某次发布的策略里写了相同的 `ResourceID`（序号 1、7），资源条件也不会把变更记录捞出来。指定 `AuditPolicyChange` 类别并同时筛选资源时结果为空（合取，不退回为该资源的决策）。资源条件与类别、主体条件同时生效，记录必须满足全部已设置条件。
- **按原始字节比较**：区分大小写、不去掉首尾或内部空格、`*` 只是普通字节、不做路径或通配符解释；中文、控制字符与非 UTF-8 字节各自保持含义，单个非法字节（如 0xFF）、另一个非法字节（0xFE）与真正的 U+FFFD 替换字符是三个不同标识，互不可见。
- **严格组织隔离**：只返回查询组织内属于这份资源的决策；同一作用域中的另一份资源不混入，其他组织即使使用相同资源标识也不混入（示例中 `globex` 的视图只有它自己的序号 2）。
- **分页与检查点规则不变**：页大小限制匹配记录数，夹在中间的其他资源记录与策略变更不占名额；按返回的 `Next` 继续，按原序号递增取到固定范围内全部匹配记录，不重复、不遗漏；第一次查询钉住的截至序号与指纹贯穿后续页，之后新增的访问记录（示例序号 10）留给新查询；没有匹配记录时返回空页并结束，不退回完整历史；检查点指纹不符仍返回 `ErrInvalidRange`，不交付部分记录。
- **只是新增查询视图**：返回记录保持原始序号、完整请求与决策解释，且仍是与内部历史脱离的副本；策略、审计链内容、完整导出（`AuditExport`）与离线复核所需材料均不改变，查询与翻页只读。

## 命令行离线复核：`darksafe review`

`review` 子命令在原服务实例结束后，仅凭一份保存的审计归档复核其中**一条已有访问决策**，不联系运行中的服务，也不创建或恢复策略存储，更不会把该决策的请求重新提交。它通过 `DecodeVerifiedAuditArchive` 与 `RecheckVerifiedDecisionOffline` 组成一次调用：读取归档时按另行保留的检查点对**整份材料做且只做一次**完整链校验，随后直接以这份已校验材料中决策记录保存的请求与其实际使用的历史策略版本重算，复核阶段不再对同一批记录重复走一遍链校验。独立入口 `DecodeAuditArchive` 与 `RecheckDecisionOffline` 被其他程序单独调用时仍各自完成完整校验（见上文），无需先调用另一个入口。

调用参数（`--archive` 与 `--seq` 必填；检查点三字段可以逐项给出，也可以整体来自文件，二选一）：

| 参数 | 含义 |
| --- | --- |
| `--archive FILE` | `EncodeAuditArchive` 写出的归档文件路径；读取完整导出，不能是按主体筛选的分页或缺失开头的片段。 |
| `--checkpoint FILE` | 另行**独立保存的检查点文件**；给出后组织、截至序号与指纹都从该文件读取，无需再给 `--org/--end-seq/--fingerprint`。文件即 `examples/offline_review` 写出的文本格式：每行一个 `键=值`，`org`、`end_seq`、`fingerprint` 各恰好出现一次，行序任意，允许一个末尾换行；`org` 为 Go 风格双引号字符串（转义逐字节还原，含中文、首尾空格、控制字符、非 UTF-8 字节），`end_seq` 为非负十进制整数（无记录组织为 0，且须在本机 `int` 可表示范围内），`fingerprint` 为 64 个十六进制字符。 |
| `--org ORG` | 归档所属组织名；必须与记录绑定的组织一致，可包含空格等任意原始字节。与 `--checkpoint` 互斥。 |
| `--seq N` | 要复核的决策记录在该组织内的序号，正整数（从 1 开始）。 |
| `--end-seq N` | **另行保留**的检查点截至序号（无记录组织为 0），不能为负。与 `--checkpoint` 互斥。 |
| `--fingerprint HEX` | **另行保留**的检查点指纹（64 个十六进制字符）。与 `--checkpoint` 互斥。支持 `--flag value` 与 `--flag=value` 两种写法；`darksafe review --help` 可查看完整说明。
| `--json` | 可选开关。加上后标准输出改为**一份完整的 JSON 对象**，便于把单条离线复核结果交给其他程序读取；不加则保持上面的文字报告不变。也接受 `--json=true/false`，不接受其后的位置值（`--json --seq 2` 照常工作）。 |

要点：

- 检查点只能二选一提供：要么 `--checkpoint FILE`，要么 `--org/--end-seq/--fingerprint` 三字段。`--checkpoint` 与三字段中任何一个同时出现都按**参数冲突**处理（退出码 2），即使值完全相同、无论参数出现先后，也不会以出现顺序互相覆盖。
- 检查点文件本身无法读取，或缺项、重复项、未知字段、非空杂项行、组织为空或引号/转义非法、截至序号非法或超出可表示范围、指纹不是 64 个十六进制字符，均以退出码 **2** 结束，标准输出为空，标准错误指明出错的文件与具体行/项。文件格式正确但其中组织或检查点与归档不符时，属于下面的材料校验失败（退出码 1），不会退回使用归档内嵌的检查点。
- 归档内嵌的检查点仅供参考，校验一律以另行提供的检查点（文件或三字段）为准。即使 `--seq` 指向靠前的记录，归档中的**全部**记录都会校验；后续记录损坏、记录缺失或外部检查点不符都会整次失败，不输出部分决策。
- 重算严格使用该决策记录的请求与其当时版本；归档中即使存在后续发布或回滚，也绝不改用较新策略。
- 材料合法但原决策与重算决策不一致时，仍完整打印双方并明确标注 `consistent: no`，退出码为 **0**——不一致不是文件损坏。
- 成功时标准输出依次给出目标序号、原决策、重算决策；两份决策都包含允许与否（`allowed`）、理由（`reason`）、命中策略（`matched policies`）与实际策略版本（`policy version`），并以 `consistent: yes/no` 明确结论。
- 组织名、命中策略标识与理由可能含空格、控制字符或非 UTF-8 字节；输出使用 Go 风格引号（`%q`）逐字节表示，单个 `0xFF` 显示为 `\xff`、`0xFE` 显示为 `\xfe`、真正的 U+FFFD 仍显示为 `�`，不同非法字节不会被替换成同一个字符。
- 该命令只读指定归档，不改写归档或检查点，也不追加审计记录。

退出码：`0` 完整复核报告已无错误地全部写出（含不一致）；`1` 归档校验失败（`ErrInvalidArchive`/`ErrInvalidRange`，**含检查点文件格式正确但其组织或检查点与归档不符**）、目标序号不存在（`ErrAuditNotFound`）、目标不是决策记录（`ErrAuditNotADecision`）、历史策略版本无法取得（`ErrVersionNotFound`），**或复核报告未能完整写到标准输出**（接收方明确拒绝写入，或只接收了部分字节且未报错），错误信息可区分且只写标准错误；`2` 文件无法读取（归档或 `--checkpoint` 文件）、必填输入缺失、检查点三字段与 `--checkpoint` 同时出现（参数冲突，即使值相同）、检查点文件内容形状非法（缺项/重复项/额外字段/非空杂项行、组织为空或引号转义非法、截至序号非法或超出可表示范围、指纹格式错误）、序号无法解析为整数、目标序号非正或逐项传参时截至序号为负，具体原因写标准错误。注意区分两类失败：**检查点文件读不出或格式坏了是退出码 2（调用本身的问题）；文件格式正确、只是其中组织/检查点与归档对不上，是退出码 1（材料校验失败）**，二者都保证标准输出为空、不输出部分复核结果。报告输出失败时保留具体写入错误原因；仅字节数不足而无错误时，标准错误说明报告未写完整（写出/应写字节数），不把它描述成归档损坏、目标不存在或决策不一致。已被接收方收下的报告前缀保持原样留在标准输出，不撤回、不继续追加、也不换另一种格式补写；报告开始写出之前发生的失败仍保证标准输出为空。

### `--json`：把单条复核结果作为 JSON 交付

加上 `--json` 后，复核过程完全不变（仍然对整份归档做且只做一次完整链校验、再按当时版本重算，不重新提交访问请求、不改用较新策略、不改写归档、不追加审计记录），只是标准输出变成**且仅变成**一份完整的 JSON 对象：没有标题、说明，也不混入文字版报告。归档、目标或历史版本方面的失败（退出码 1）与参数/文件失败（退出码 2）都发生在报告开始写出之前，标准输出保持为空，原因只写标准错误；即使目标序号靠前，只要归档后面的记录损坏，整次失败且不交付部分 JSON。若标准输出接收方拒绝写入或只接收部分字节（即使没有同时返回错误），已接收的前缀原样保留、不撤回，但以退出码 1 结束并在标准错误说明复核报告输出失败，此后不再追加内容、也不退回文字格式补写——因此只有完整对象无错误写出时退出码才是 0。

对象字段与文字报告一一对应，目标序号与策略版本是两个独立字段：

| JSON 字段 | 对应文字报告 | 含义 |
| --- | --- | --- |
| `target_sequence` | `target sequence` | 被复核的审计记录序号（不是策略版本号）。 |
| `original` | `original decision` | 记录中保存的原决策。 |
| `recomputed` | `recomputed decision` | 按归档材料与当时版本重算的决策。 |
| `consistent` | `consistent: yes/no` | 两份决策是否逐字段一致；材料合法但结论不一致时为 `false`，退出码仍是 0。 |

每份决策对象包含：`allowed`（允许与否，布尔）、`reason`（理由）、`matched_policies`（命中策略标识列表，**顺序与文字报告一致**）、`policy_version`（实际策略版本；与 `target_sequence` 分开表示）。判断规则不因选择格式而改变：`nil` 与空命中列表仍视为一致，二者在 JSON 中都输出 `[]`。

**字符串表示规则（公开约定）。** 理由与命中策略标识可以包含中文、首尾空格、换行、引号、反斜杠、控制字符以及非法 UTF-8 字节：

- 字符串是**合法 UTF-8** 时，按普通 JSON 字符串输出：引号、反斜杠与控制字符按 JSON 规则转义（如 `"` → `\"`、换行 → `\n`、制表符 → `\t`），普通中文与真正的 U+FFFD 直接以可读 UTF-8 出现，控制字符不会破坏对象边界。
- 字符串**不是合法 UTF-8** 时，输出对象 `{"base64": "<标准 base64>"}`，base64 载荷是该字符串**原始字节**的 RFC 4648 标准编码。两种表示形状互斥（字符串不可能是对象），接收方据此可无歧义还原每个字符串的原始字节：单个 `0xFF` 为 `{"base64": "/w=="}`、`0xFE` 为 `{"base64": "/g=="}`，而真正的 U+FFFD 是合法 UTF-8，直接输出为字符串里的 `�`（JSON 转义形式 `"�"`），三者始终可区分，不会被静默替换成同一个字符。

一份实际输出（在上面的 `acme factory` 材料上运行 `darksafe review --json ...`）：

```json
{
  "target_sequence": 2,
  "original": {
    "allowed": true,
    "reason": "matched allow policy",
    "matched_policies": [
      "p-ledger-read-2026"
    ],
    "policy_version": 1
  },
  "recomputed": {
    "allowed": true,
    "reason": "matched allow policy",
    "matched_policies": [
      "p-ledger-read-2026"
    ],
    "policy_version": 1
  },
  "consistent": true
}
```

接收方用任意标准 JSON 解析器读取即可；还原字符串时：字段是 JSON 字符串就按其 UTF-8 字节取用，字段是 `{"base64": ...}` 对象则对载荷做标准 base64 解码得到原始字节。

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

检查点文件内容（也就是第二步 `--checkpoint` 直接读取的格式；字段顺序可变、允许一个末尾换行）：

```text
org="acme factory"
end_seq=2
fingerprint=04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299
```

### 第二步：仅凭保存的材料离线复核

材料生成后，原来的内存存储不再需要（进程结束也没关系），也**不需要重新提交访问请求**。在保存材料的目录直接执行（逐项传参）：

```bash
go run ./cmd/darksafe review \
  --archive acme-factory.audit \
  --org 'acme factory' \
  --seq 2 \
  --end-seq 2 \
  --fingerprint 04b274dbb4cf039bbb4b78f5ee5aae03278d2c34833fe87fecb13ade51ef5299
```

也可以把第一步独立保存的检查点文件直接交给 `--checkpoint`，组织、截至序号、指纹都从文件读取，不必再手抄一遍：

```bash
go run ./cmd/darksafe review \
  --archive acme-factory.audit \
  --seq 2 \
  --checkpoint acme-factory.checkpoint
```

两种写法互斥：`--checkpoint` 不能与 `--org/--end-seq/--fingerprint` 任何一个同时出现（即使值相同、无论先后，均以退出码 2 报参数冲突）。同一份检查点内容下，两种写法的文字报告、`--json` 报告与一致性结论逐字节相同，且都不会额外打印“读取了检查点文件”之类的提示；归档与检查点文件都只读不改。

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

2. **提供与归档不符的外部检查点**：例如指纹被改动，或拿了另一份归档的检查点。整份材料链校验失败，对应 `ErrInvalidRange`，不会打印任何决策。用 `--checkpoint` 给出文件时同样如此——**文件本身读得出且格式正确，只是其中内容与归档对不上，仍是退出码 1**，且不会退回使用归档内嵌的检查点：

   ```bash
   go run ./cmd/darksafe review --archive acme-factory.audit --seq 2 \
     --checkpoint wrong-org.checkpoint
   # 退出码 1；标准输出为空；标准错误：
   # review: archive validation failed: darksafe: invalid audit range: archive organization "..." does not match "..."
   ```

   要与退出码 **2** 的文件/参数问题区分开：文件不存在或无法读取、`--checkpoint` 与 `--org/--end-seq/--fingerprint` 同时出现（即使值相同）、文件缺项/重复项/有额外字段或非空杂项行、`org` 为空或引号转义非法、`end_seq` 非法或超出可表示范围、指纹不是 64 个十六进制字符——这些是调用本身有误，退出码 2，标准输出为空，标准错误点名出错的文件与具体行/项。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
