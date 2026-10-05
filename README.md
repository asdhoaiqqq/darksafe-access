# darksafe：容器应用离线发布计划

## 用途

darksafe 目前已交付、可独立使用的功能是**容器应用发布计划（release plan）的离线计算**：给定一份 JSON 发布配置（应用、修订、镜像、候选集群、标签筛选与分批参数），在本机算出按故障域打散的灰度批次，并逐个列出未入选集群及其原因。

计划全程在本机计算：**不连接 Kubernetes 或任何集群，不执行实际发布**，不需要外部服务或网络。

同一份规划能力也以 **Go 库**形式公开：不经过 JSON 文件和命令行，直接在内存中构造 `darksafe.ReleasePlanInput` 并调用 `darksafe.MakeReleasePlan` 即可得到计划，用法见下文[「作为 Go 库使用：用内存配置计算计划」](#作为-go-库使用用内存配置计算计划)。

仓库的长期产品方向是零信任身份与授权决策平台（见 `PRODUCT_GOAL.md`）：访问决策核心位于 `darksafe/`，由 `demo` 命令演示；发布计划的领域逻辑同样位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。

```bash
go run ./cmd/darksafe demo                    # 运行内置访问决策演示
go run ./cmd/darksafe version                 # 显示版本号
go run ./cmd/darksafe plan <JSON文件路径>      # 离线计算发布计划
go run ./cmd/darksafe help                    # 显示帮助
go test ./...                                 # 运行全部测试
```

- `demo`：真实运行内置的零信任访问决策示例，对三个主体逐一判定是否允许读取受保护资源，输出每个主体的决策原因与汇总。
- `version`：输出 `darksafe 0.1.0`。
- `plan <JSON文件路径>`：离线读取发布配置并输出发布计划（JSON），不连接集群、不执行发布；不带参数运行 darksafe 等同于 `demo`。
- `help`（或 `-h`、`--help`）：输出命令用法与 plan 字段一览。

`demo` 的真实输出：

```
subject=u-1001 allowed=true reason=role grants read:org/payments/ledger
subject=svc-batch allowed=true reason=role grants read:org/payments/ledger
subject=u-1002 allowed=false reason=subject is disabled
summary: 2 of 3 subjects allowed
```

## 离线发布计划使用说明

### 1. 准备配置

配置是一个 JSON 对象，字段如下：

| 字段 | 必填 | 说明 |
|---|---|---|
| `app` | 是 | 应用名称，非空字符串（不能只含空白） |
| `revision` | 是 | 待发布修订号，非空字符串（不能只含空白） |
| `image` | 是 | 容器镜像引用，非空字符串（不能只含空白） |
| `batchSize` | 是 | 每批集群数量的**上限**，正整数 |
| `clusters` | 是 | 候选集群数组，至少一项；每项含唯一的 `id`、可选的 `disabled`（布尔值）、可选的 `tags`（字符串到字符串的映射） |
| `include` | 否 | 包含条件数组；省略时允许所有未停用、未排除的集群 |
| `exclude` | 否 | 排除条件数组；省略时不排除任何集群 |
| `spreadBy` | 否 | 故障域标签键；省略或为 `""` 时使用普通分批 |

`include`/`exclude` 的每个元素都是一个条件对象，至少包含一个键值对，键不能为空字符串。集群 `id` 在全部候选（含停用和被筛掉的集群）中必须唯一。任何 JSON 对象（含嵌套对象）出现重复成员名都会在业务校验之前被拒绝。任何字符串（成员名或值，含未知附加字段中的嵌套内容）含有无效 UTF-8 字节或未配对代理项的 `\uXXXX` 转义时，整份配置同样在任何校验与计算之前被拒绝，错误会区分这两种情况并指出问题位置；合法 UTF-8（包括字面书写的“�”）、成对的代理项转义和 `\\uD800` 这类普通文字不受影响。

### 2. 完整配置示例

将以下内容原样保存为 `plan.json`：一个应用、一个修订、一个镜像；候选集群在文件中**刻意不按标识顺序**排列。

```json
{
  "app": "payments-gateway",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments-gateway:2026.10.0-r3",
  "batchSize": 3,
  "spreadBy": "zone",
  "include": [
    {"env": "prod"},
    {"tier": "edge", "scope": "public"}
  ],
  "exclude": [
    {"env": "temp"},
    {"quarantine": "true"}
  ],
  "clusters": [
    {"id": "c-c", "tags": {"env": "prod", "zone": "z2"}},
    {"id": "g2", "tags": {"env": "temp", "zone": "z9"}},
    {"id": "c-a", "tags": {"env": "prod", "zone": "z1"}},
    {"id": "g0", "disabled": true},
    {"id": "c-e", "tags": {"tier": "edge", "scope": "public", "zone": "z3"}},
    {"id": "g1", "tags": {"tier": "edge", "zone": "z1"}},
    {"id": "g3", "tags": {"env": "prod", "quarantine": "true"}},
    {"id": "c-d", "tags": {"env": "prod", "zone": "z2"}},
    {"id": "c-b", "tags": {"env": "prod", "zone": "z1"}}
  ]
}
```

### 3. 调用 plan 并取得成功输出

```bash
go run ./cmd/darksafe plan plan.json
```

也可以先 `go build -o darksafe ./cmd/darksafe`，再执行 `./darksafe plan plan.json`。成功时退出状态为 0，**标准输出只有完整计划**，标准错误为空。上述配置产生的完整输出如下，与配置逐项对应、不含任何省略：

```json
{
  "app": {
    "name": "payments-gateway",
    "revision": "2026.10.0-r3",
    "image": "registry.example.net/payments-gateway:2026.10.0-r3"
  },
  "batches": [
    {
      "index": 1,
      "clusters": [
        "c-a",
        "c-c",
        "c-e"
      ]
    },
    {
      "index": 2,
      "clusters": [
        "c-b",
        "c-d"
      ]
    }
  ],
  "excluded": [
    {
      "id": "g0",
      "reason": "集群已停用"
    },
    {
      "id": "g1",
      "reason": "未命中包含条件"
    },
    {
      "id": "g2",
      "reason": "命中排除条件"
    },
    {
      "id": "g3",
      "reason": "命中排除条件"
    }
  ]
}
```

### 4. 逐项核对结果

**应用信息**：输出的 `app.name`、`app.revision`、`app.image` 原样回显配置中的 `app`、`revision`、`image`。

**筛选结果**（判定优先级：停用 → 排除 → 包含）：

| 集群 | 标签情况 | 结果 |
|---|---|---|
| `g0` | `disabled: true` | **集群已停用**，不再参与排除/包含判定 |
| `g2` | `env=temp` | 命中第一个排除条件（`env=temp`） |
| `g3` | `env=prod` 且 `quarantine=true` | 同时命中包含与排除，**排除优先** → 命中排除条件 |
| `g1` | `tier=edge`、`scope` 缺失 | 两个包含条件都不满足（第二个条件要求 `tier` 与 `scope` 同时匹配）→ 未命中包含条件 |
| `c-a` `c-b` `c-c` `c-d` `c-e` | — | 入选，按标识升序排期：`c-a`(z1)、`c-b`(z1)、`c-c`(z2)、`c-d`(z2)、`c-e`(z3) |

**分批结果**（`batchSize: 3` 且 `spreadBy: "zone"`：每批最多 3 个集群，同一故障域每批最多 1 个）：

- 批次 1 从最小标识开始依次考察：`c-a` 占用故障域 z1；`c-b` 也是 z1，发生冲突，**延后到后续批次**；接下来的 `c-c` 属于另一个故障域 z2，**冲突不影响其他故障域，照常进入当前批次**；`c-d` 与 z2 冲突，延后；`c-e` 占用 z3。至此批次达到容量上限 3，关闭为 `[c-a, c-c, c-e]`。
- 批次 2 处理被延后的 `c-b`(z1)、`c-d`(z2)：两者故障域不同、互不冲突，全部进入，批次为 `[c-b, c-d]`；此后没有剩余集群，本批以 2 个关闭。
- 由此可见：**`batchSize` 是容量上限而不是配额，批次可以不足额**；批次编号**从 1 开始连续**；每个入选集群恰好出现在一个批次中，批内标识保持升序；集群在配置文件中的书写顺序（`c-c` 写在最前）不影响任何结果。

**未入选列表**：`excluded` 按集群标识升序给出（`g0`、`g1`、`g2`、`g3`），原因为以下三种固定文案之一：`集群已停用`、`命中排除条件`、`未命中包含条件`。

### 5. 筛选与分批的规则细节

**标签条件怎么匹配**

- 一个条件对象内的键值需要**全部匹配**（逻辑与）；条件数组里的多个条件满足**任意一个**即可（逻辑或）。
- 判定优先级固定为：**停用优先于排除，排除优先于包含**。停用集群永远不能进入计划；一个集群同时命中包含与排除时以排除为准（见 `g3`）。
- 标签键和值都按**原字符串精确匹配**：区分大小写，不做任何修剪或归一化。
- **缺少标签与标签值为空不是同一情况**：标签键不存在，则任何要求该键的等值条件都不匹配；标签存在且值为 `""`（如 `"zone": ""`）是一个真实的值，可以被条件 `{"zone": ""}` 精确匹配。

**故障域与 `spreadBy`**

- `spreadBy` 指定一个标签键，其值代表故障域。启用后每批仍受 `batchSize` 容量约束，且同一故障域每批最多一个集群；同域冲突的集群延后，排在其后的其他故障域集群仍可进入当前批次。
- 只有**入选**集群必须携带该标签；已被停用或被条件筛掉的集群不需要该标签（示例中的 `g0`、`g3` 都没有 `zone` 标签，不影响计划成功）。
- **空值可以代表一个故障域**：两个带 `"zone": ""` 的集群属于同一个空字符串故障域，不能同批，但可以与其他故障域的集群同批。
- 省略 `spreadBy` 或显式写成 `"spreadBy": ""` 时保持**普通分批**：仅按 `batchSize` 容量切分，同故障域集群允许同批，两种写法输出完全一致。只含空白的 `spreadBy` 会被直接判为配置错误。

### 6. 两种失败结果

**失败一：入选集群缺少指定的故障域标签 —— 整个计划失败**

在第 2 节配置的基础上，把 `c-b` 的 `"zone": "z1"` 删除（保留 `"env": "prod"`，使它仍能通过筛选），其余不动，再执行同样的命令。计划不会输出任何部分批次：退出状态非零，**标准输出为空**，标准错误指出缺失的标签名以及标识最小的问题集群（即使还有其他入选集群也缺该标签，也只报告标识最小的那一个）：

```
集群 "c-b" 缺少故障域标签 "zone"
```

**失败二：所有候选都被筛掉 —— 不会成功返回空计划**

在第 2 节配置的基础上，把 `include` 改成没有任何集群能满足的 `[{"env": "staging"}]`，其余不动。此时不会成功返回一个批次为空的计划，而是非零退出、**标准输出为空**，并在标准错误中按标识升序列出**全部候选**的未入选原因：

```
没有符合规则的可用集群，各候选集群未入选原因：
  c-a：未命中包含条件
  c-b：未命中包含条件
  c-c：未命中包含条件
  c-d：未命中包含条件
  c-e：未命中包含条件
  g0：集群已停用
  g1：未命中包含条件
  g2：命中排除条件
  g3：命中排除条件
```

这份逐行报告中的每条原因都对应一个真实候选：集群标识只要包含双引号、反斜线、Unicode 控制字符，或 U+2028/U+2029 分隔符，就会整体显示为带双引号的 JSON 字符串（控制字符与两个分隔符被转义，引号与反斜线保持可还原，例如标识 `c-a`+真实换行+`c-b` 显示为 `"c-a\nc-b"`，仍只占一条记录），报告中不会出现真实的换行、回车、制表等控制效果；从显示内容可以准确还原原始标识。其他标识（包括中文等普通可见字符）保持原样直接显示。这只是标识在报告中的显示方式：标识本身不会被修剪、替换或拒绝，排序与身份比较始终使用原始标识。

**成功与失败的统一约定**：成功时退出状态为 0，标准输出仅包含完整计划 JSON（应用信息、从 1 开始连续编号的批次、按标识升序的未入选集群及原因），标准错误为空；失败时退出状态非零（配置或规划规则问题退出码为 1，命令用法错误退出码为 2），问题说明写入标准错误，标准输出为空。

## 作为 Go 库使用：用内存配置计算计划

如果调用方已经在 Go 程序里持有应用与集群信息，就不必先写 JSON 文件再调命令行：`darksafe` 包把规划能力作为公开 API 提供，**直接用内存中的 `darksafe.ReleasePlanInput` 计算计划**。计算仍然全部在本机进程内完成，不连接集群、不执行实际发布，也不需要任何外部服务。

- `darksafe.ReleasePlanInput`：一份发布配置，字段与 JSON 配置一一对应——`App`、`Revision`、`Image`、`BatchSize`、`Clusters`、`Include`、`Exclude`、`SpreadBy`；候选集群是 `darksafe.Cluster{ID, Disabled, Tags}`，条件是 `darksafe.LabelCondition`（一个 `map[string]string`，键值需全部匹配）。
- `darksafe.MakeReleasePlan(in)`：校验配置并计算计划，成功返回 `darksafe.ReleasePlan`（含 `App`、`Batches`、`Excluded`）和 `nil` 错误；失败返回**零值计划**和非空错误。它不会修改传入的 `in`。
- `darksafe.ValidateReleaseInput(in)`：只检查配置是否合法，不做规划；`MakeReleasePlan` 在计算前会自动调用它，库调用方也可以单独调用（例如提前校验表单输入）。

内存配置里的每个字符串都必须是**合法 UTF-8**：`App`、`Revision`、`Image`、`SpreadBy`，以及**全部候选集群**（即使已停用或注定被筛掉）的标识、标签键和值，还有每条包含/排除条件的键和值。Go 字符串可以携带非法 UTF-8 字节；若放过，两个不同的非法标识在把计划写成 JSON 时会被编码层一起改写成“�”而变成同一身份，非法标签值也会悄悄参与匹配与分批。因此这类配置在任何业务校验、筛选和分批**之前**就会被拒绝：`ValidateReleaseInput` 返回明确错误，`MakeReleasePlan` 返回非空错误和**零值计划**（不会修补原字符串、删除问题候选，也不会修改调用方的配置）。错误会说明是非法 UTF-8 并定位到字段（候选与条件位置从 0 开始；标签或条件值出错时指出对应键，键本身出错时指出所属的标签映射或条件对象）；多处同时有问题时只报告一个固定的问题，结果与 map 遍历顺序无关。合法文本一律按原字符串使用：中文、补充平面字符（如 😀）、用户真正输入的“�”都保持原样，普通文字里的反斜线与 `uD800` 也不是非法 UTF-8；标签空值仍可精确匹配，大小写与首尾空白仍不做归一化。

下面是一个**完整、可独立运行**的示例（一个应用、一个修订、一份镜像配置），展示应用信息、候选集群、筛选条件与分批参数的传入方式，以及成功后如何读取批次和未入选原因：

```go
package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	// 候选在代码中的书写顺序刻意不同于标识顺序：c-c 写在最前。
	in := darksafe.ReleasePlanInput{
		App:       "payments-gateway",
		Revision:  "2026.10.0-r3",
		Image:     "registry.example.net/payments-gateway:2026.10.0-r3",
		BatchSize: 2,
		SpreadBy:  "zone",
		Include:   []darksafe.LabelCondition{{"env": "prod"}},
		Exclude:   []darksafe.LabelCondition{{"quarantine": "true"}},
		Clusters: []darksafe.Cluster{
			{ID: "c-c", Tags: map[string]string{"env": "prod", "zone": "west"}},
			{ID: "c-e", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "c-a", Tags: map[string]string{"env": "prod", "zone": "east"}},
			{ID: "c-d", Disabled: true},
			{ID: "c-b", Tags: map[string]string{"env": "prod", "zone": "east"}},
		},
	}

	plan, err := darksafe.MakeReleasePlan(in)
	if err != nil {
		log.Fatal(err) // 失败时 err 即具体原因；此时 plan 是零值，不能当成成功计划使用
	}

	fmt.Printf("应用: %s  修订: %s  镜像: %s\n", plan.App.Name, plan.App.Revision, plan.App.Image)
	for _, b := range plan.Batches {
		fmt.Printf("第 %d 批: %s\n", b.Index, strings.Join(b.Clusters, ", "))
	}
	for _, e := range plan.Excluded {
		fmt.Printf("未入选 %s: %s\n", e.ID, e.Reason)
	}
}
```

`c-a`、`c-b` 属于 `east` 故障域，`c-c` 属于 `west`；`BatchSize` 为 2、`SpreadBy` 为 `zone`。实际输出：

```
应用: payments-gateway  修订: 2026.10.0-r3  镜像: registry.example.net/payments-gateway:2026.10.0-r3
第 1 批: c-a, c-c
第 2 批: c-b
未入选 c-d: 集群已停用
未入选 c-e: 命中排除条件
```

对照输入逐条理解：

- **应用信息按输入保留**：`plan.App.Name`、`plan.App.Revision`、`plan.App.Image` 原样回显。
- **分批**：入选集群先按标识升序考察。第 1 批从 `c-a`（占用 east）开始；`c-b` 同为 east 而冲突，延后到后续批次；`c-c` 属于 west，不与 east 冲突，照常进入本批，批次达到容量 2 后关闭为 `[c-a, c-c]`。第 2 批只剩延后的 `c-b`，以 1 个集群关闭。批次编号**从 1 开始连续**，批内标识保持升序；候选在代码中的书写顺序（`c-c` 在最前）不影响任何结果。
- **未入选列表按标识升序排列**：`c-d` 已停用（`Disabled: true`，且没有 `zone` 标签），原因是「集群已停用」；`c-e` 同时命中包含条件（`env=prod`）与排除条件（`quarantine=true`），按「停用 → 排除 → 包含」的固定优先级，原因是「命中排除条件」。两者都没有 `zone` 标签，但因为在筛选阶段就已离开候选，**不需要故障域标签，仍按各自的筛选优先级进入未入选列表，不影响计划成功**。

### 单独校验与计算计划的区别

`ValidateReleaseInput` 只回答「这份配置**合不合法**」，**校验成功并不保证有集群可发布**——是否真的算出批次，要由 `MakeReleasePlan` 决定。用**空候选列表**最能说明这个区别：应用信息与其他参数都合法时，单独校验可以成功，而计算计划会失败：

```go
empty := in
empty.Clusters = nil

fmt.Println(darksafe.ValidateReleaseInput(empty)) // 配置合法：<nil>
plan, err := darksafe.MakeReleasePlan(empty)
fmt.Println(err)  // 没有可发布的集群：未提供候选集群
_ = plan          // 此时为零值计划，不能当成“空但成功”的计划使用
```

输出：

```
<nil>
没有可发布的集群：未提供候选集群
```

也就是说，**调用方不能把出错时返回的计划值当成成功计划**：`MakeReleasePlan` 一旦返回非空错误，返回的 `ReleasePlan` 就是零值，必须先判断 `err`。同样地，当所有候选都被筛掉、或入选集群缺少 `SpreadBy` 指定的标签时，`MakeReleasePlan` 也返回非空错误和零值计划，错误文案与命令行一节展示的完全一致（前者在错误中按标识升序列出全部候选的未入选原因，后者指出缺失的标签名和标识最小的问题集群）。

### 候选标识规则与重复标识错误

集群 `ID` 在**全部候选中必须唯一**，检查发生在任何停用/筛选判断之前：因此重复项**即使已停用、或注定会被包含/排除条件筛掉，也照样报错**，不会因为“反正要被丢弃”而被放过。错误会指明**后出现那条记录的位置（从 0 开始）以及重复的标识**，例如：

```go
dup := in
dup.Clusters = []darksafe.Cluster{
	{ID: "c-a", Tags: map[string]string{"env": "prod", "zone": "east"}},
	{ID: "c-a", Disabled: true}, // 与第 0 项重复，即使已停用也报错
}
_, err := darksafe.MakeReleasePlan(dup)
fmt.Println(err)
```

输出：

```
clusters[1]: 重复的集群标识 "c-a"
```

合法标识按**原字符串保留和比较**，不做大小写折叠或首尾修剪：`"c-a"`、`"C-A"`、`" c-a "` 是三个互不相同的集群，可以同时入选，回显和排期都保持原样；而**空字符串和只含空白的标识不合法**（`clusters[0]: 字段 "id" 不能为空或只含空白`）。标签键、标签值和条件同理，都按原字符串精确匹配。

### 库调用与命令行的关系

两种入口共用同一套规则与错误文案，差别只在配置来源和结果去向：

| | 命令行 `plan <JSON文件>` | Go 库 `MakeReleasePlan` |
|---|---|---|
| 配置来源 | JSON 文件（经 `ParseReleaseInput` 解析，含无效 UTF-8 或未配对 Unicode 转义的字符串、重复 JSON 成员名都会被拒绝） | 内存中的 `ReleasePlanInput` 结构体（应用信息、`spreadBy`、全部候选集群的标识与标签键值、包含/排除条件的键值都必须是合法 UTF-8，否则在筛选与分批之前被拒绝） |
| 成功 | 退出码 0，完整计划写到标准输出 | 返回 `ReleasePlan, nil`，由调用方读取 `App`/`Batches`/`Excluded` |
| 失败 | 非零退出，原因写到标准错误，标准输出为空 | 返回零值 `ReleasePlan` 和非空 `error`，错误说明具体原因 |
| 可选的预校验 | — | `ValidateReleaseInput` 只校验合法性，不保证有集群可发布 |

无论哪种入口，计算都在本机离线完成：**不连接 Kubernetes 或任何集群，不执行实际发布**。

## 长期产品方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

长期定位与演进规则见 `PRODUCT_GOAL.md`；当前版本已交付、可在命令行独立验收的能力，以本说明的离线发布计划和 `demo` 访问决策演示为准。
