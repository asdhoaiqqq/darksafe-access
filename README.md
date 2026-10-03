# darksafe：容器应用离线发布计划

## 用途

darksafe 目前已交付、可独立使用的功能是**容器应用发布计划（release plan）的离线计算**：给定一份 JSON 发布配置（应用、修订、镜像、候选集群、标签筛选与分批参数），在本机算出按故障域打散的灰度批次，并逐个列出未入选集群及其原因。

计划全程在本机计算：**不连接 Kubernetes 或任何集群，不执行实际发布**，不需要外部服务或网络。

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

`include`/`exclude` 的每个元素都是一个条件对象，至少包含一个键值对，键不能为空字符串。集群 `id` 在全部候选（含停用和被筛掉的集群）中必须唯一。任何 JSON 对象（含嵌套对象）出现重复成员名都会在业务校验之前被拒绝。

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

**成功与失败的统一约定**：成功时退出状态为 0，标准输出仅包含完整计划 JSON（应用信息、从 1 开始连续编号的批次、按标识升序的未入选集群及原因），标准错误为空；失败时退出状态非零（配置或规划规则问题退出码为 1，命令用法错误退出码为 2），问题说明写入标准错误，标准输出为空。

## 在 Go 代码中调用（库方式）

命令行的 `plan` 子命令只是 `darksafe` 包的一层封装。调用方也可以不经过 JSON 文件，直接在内存中构造 `ReleasePlanInput` 并调用 `MakeReleasePlan` 计算计划。计算同样**全程在本机离线完成：不连接任何集群，不执行实际发布**，与命令行方式共享同一套校验、筛选与分批规则。

```go
import "github.com/asdhoaiqqq/darksafe-access/darksafe"
```

### 1. 完整示例：内存配置计算计划

下面是一个完整、可独立运行的程序：一个应用、一个修订、一份镜像配置；候选集群在代码中**刻意不按标识顺序**书写。`c-a`、`c-b` 属于 `east` 故障域，`c-c` 属于 `west`；`g0` 已停用、`g1` 同时命中包含与排除条件，两者都没有 `zone` 标签。

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
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
			{ID: "g1", Tags: map[string]string{"env": "prod", "quarantine": "true"}},
			{ID: "c-b", Tags: map[string]string{"env": "prod", "zone": "east"}},
			{ID: "g0", Disabled: true},
			{ID: "c-a", Tags: map[string]string{"env": "prod", "zone": "east"}},
		},
	}

	plan, err := darksafe.MakeReleasePlan(in)
	if err != nil {
		// 失败时 err 说明具体原因，返回的 plan 是零值，不能使用
		fmt.Fprintln(os.Stderr, "计算发布计划失败:", err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "序列化计划失败:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
```

运行后标准输出的完整内容（与输入逐项对应、不含省略）：

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
        "c-c"
      ]
    },
    {
      "index": 2,
      "clusters": [
        "c-b"
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
      "reason": "命中排除条件"
    }
  ]
}
```

### 2. 读取计划结果

`MakeReleasePlan` 成功时返回 `ReleasePlan`，调用方按字段读取：

- **`plan.App`**：`AppInfo{Name, Revision, Image}`，按输入的 `App`、`Revision`、`Image` 原样保留。
- **`plan.Batches`**：批次切片，`Index` **从 1 开始连续编号**，`Clusters` 是该批的集群标识。上例中入选集群按标识升序为 `c-a`(east)、`c-b`(east)、`c-c`(west)；`BatchSize: 2` 且 `SpreadBy: "zone"` 要求同一故障域每批最多一个，因此批次 1 取 `c-a` 后 `c-b` 冲突延后、`c-c` 照常进入，得到 `[c-a, c-c]`，批次 2 为 `[c-b]`。候选在代码中的书写顺序（`c-c` 写在最前）不影响结果。
- **`plan.Excluded`**：未入选列表，按标识**升序**排列，每项含 `ID` 和 `Reason`。`Reason` 是三种固定文案之一，对应包内常量 `ReasonDisabled`（集群已停用）、`ReasonExcludeMatched`（命中排除条件）、`ReasonIncludeNotMatched`（未命中包含条件）。判定优先级与命令行一致：**停用优先于排除，排除优先于包含**——`g1` 同时满足包含条件（`env=prod`）和排除条件（`quarantine=true`），以排除为准。`g0`、`g1` 都没有 `zone` 标签，但故障域标签只要求**入选**集群携带，因此它们照常进入未入选列表，不影响计划成功。

失败时 `MakeReleasePlan` 返回非空 `err` 和零值 `ReleasePlan`：**调用方必须先检查 `err`，不能把出错时的返回值当成成功计划使用**。错误文本直接说明具体原因（如 `集群 "c-b" 缺少故障域标签 "zone"`，或所有候选都被筛掉时按标识升序列出的全部未入选原因），与命令行写到标准错误的内容一致。

### 3. 用 ValidateReleaseInput 单独校验配置

`ValidateReleaseInput` 检查配置是否**合法**：应用、修订、镜像非空，`BatchSize` 为正整数，`SpreadBy` 为空或可用的标签键，集群标识唯一，标签键与条件键非空。`MakeReleasePlan` 内部会先调用它，因此直接计算计划时无需重复校验；它适合调用方在计算之前单独做一次配置检查。

需要注意：**校验成功并不保证有集群可发布**。`ValidateReleaseInput` 不检查候选列表是否为空，也不预测筛选结果。最典型的区别是空候选列表——应用信息与其他参数都合法时，单独校验成功，但计算计划会失败：

```go
in := darksafe.ReleasePlanInput{
	App:      "payments-gateway",
	Revision: "2026.10.0-r3",
	Image:    "registry.example.net/payments-gateway:2026.10.0-r3",
	BatchSize: 2,
	Include:  []darksafe.LabelCondition{{"env": "prod"}},
	// Clusters 为空
}

err := darksafe.ValidateReleaseInput(in)
// err == nil：配置本身合法

_, err = darksafe.MakeReleasePlan(in)
// err != nil："没有可发布的集群：未提供候选集群"
```

因此 `ValidateReleaseInput` 返回 `nil` 只表示"配置合法"，不能据此认为计划一定可算；是否可算仍以 `MakeReleasePlan` 的返回为准。

### 4. 集群标识的唯一性与精确性

- **重复标识必须报错**，且唯一性检查先于任何筛选：重复项即使已停用、或注定被包含/排除条件筛掉，也仍然报错。错误信息指明**后出现记录的位置**（从 0 开始的下标）和重复的标识，例如第 4 个候选重复了 `c-a`：`clusters[3]: 重复的集群标识 "c-a"`。
- 合法标识**按原字符串保留和比较**：不修剪、不归一化、不合并。`"c-a"`、`"C-A"`、`" c-a"`（带首尾空格）是三个不同的集群，可以同时出现在候选列表中。
- **空字符串和只含空白的标识不合法**，报 `clusters[<下标>]: 字段 "id" 不能为空或只含空白`。空白检查只用于判定非法，合法的带空格标识不会被改写。

## 长期产品方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

长期定位与演进规则见 `PRODUCT_GOAL.md`；当前版本已交付、可在命令行独立验收的能力，以本说明的离线发布计划和 `demo` 访问决策演示为准。
