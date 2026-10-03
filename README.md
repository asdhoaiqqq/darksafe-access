# darksafe — 离线容器应用发布计划

## 用途

darksafe 已实现的核心功能是**容器应用发布计划（release plan）的离线计算**：给定一次发布要部署的应用、修订与镜像，以及一批候选集群和筛选规则，`plan` 命令在本机算出集群应分成哪几个批次发布。

计划完全在本机计算：**不连接 Kubernetes 或任何集群，不执行实际发布，也没有任何网络副作用**。它只读取一份 JSON 配置，输出一份确定性的 JSON 计划；同一份配置重复运行、调整候选集群在文件中的书写顺序，结果都完全一致。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

## 命令

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
go run ./cmd/darksafe plan <JSON文件路径>
go run ./cmd/darksafe help
```

- `plan <JSON文件路径>`：离线读取发布配置，在标准输出写入完整发布计划（JSON）。这是本工具的主要功能，详见下文指南。
- `demo`：运行内置演示——对若干主体（用户、服务、停用用户）执行访问决策并逐个打印放行结果与原因，展示底层的身份与授权决策模型。
- `version`：打印版本号（`darksafe 0.1.0`）。
- `help`（或 `-h`、`--help`）：打印命令用法与 `plan` 输入字段概览。

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现，无需任何外部服务。

## 离线发布计划使用指南

按三步即可独立完成：准备配置 → 运行 `plan` → 核对标准输出中的计划或标准错误中的问题。

### 1. 准备配置

配置是一份 JSON 文档，字段如下：

- `app`、`revision`、`image`（必需，非空字符串）：应用名称、待发布修订号、容器镜像。计划会原样回显这三项。
- `batchSize`（必需，正整数）：每个批次最多容纳的集群数量，是**容量上限**而不是配额，批次可以不足额。
- `clusters`（必需，非空数组）：候选集群。每项含：
  - `id`（必需，唯一、非空字符串）：集群标识；
  - `disabled`（可选，布尔值）：为 `true` 表示已停用，始终不能进入计划；
  - `tags`（可选，对象）：集群标签，键和值都是字符串。
- `include`（可选，条件对象数组）：包含条件。提供时，只有命中至少一个条件的可用集群才能入选；不提供时允许所有可用集群。
- `exclude`（可选，条件对象数组）：排除条件。命中任一条件的集群被排除；不提供时不排除任何集群。
- `spreadBy`（可选，字符串）：一个集群标签键，其标签值代表故障域（如 `zone`）。启用后按故障域分批，详见下文。省略该字段或写成 `""` 时保持普通分批。

一个**条件对象**包含一组标签键值，例如 `{"env": "prod", "tier": "edge"}`：

- 同一个条件内的所有键值必须**全部**精确匹配（AND）；
- 条件数组中的多个条件**满足任意一个**即可（OR）；
- 条件对象不能为空（至少一个键值）。

### 2. 计算计划

直接用 `go run` 即可：

```bash
go run ./cmd/darksafe plan plan.json
```

如需逐字节核对退出码与标准错误（下文示例即如此），建议先构建再运行：

```bash
go build -o darksafe ./cmd/darksafe
./darksafe plan plan.json
```

### 3. 完整示例：一份配置与对应的成功输出

将下面的内容保存为 `plan.json`。注意候选集群在文件中**故意不按标识顺序排列**，这不会影响结果——计划始终按标识升序排期。

```json
{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 3,
  "spreadBy": "zone",
  "include": [
    {"env": "prod", "tier": "edge"},
    {"env": "prod", "tier": "core"}
  ],
  "exclude": [
    {"drain": "true"}
  ],
  "clusters": [
    {"id": "cl-az3-n1", "tags": {"env": "prod", "tier": "edge", "zone": "az3"}},
    {"id": "cl-dev", "tags": {"env": "dev", "zone": "az1"}},
    {"id": "cl-az1-n2", "tags": {"env": "prod", "tier": "core", "zone": "az1"}},
    {"id": "cl-off", "disabled": true},
    {"id": "cl-az2-n1", "tags": {"env": "prod", "tier": "edge", "zone": "az2"}},
    {"id": "cl-drain", "tags": {"env": "prod", "tier": "core", "drain": "true", "zone": "az3"}},
    {"id": "cl-az1-n1", "tags": {"env": "prod", "tier": "edge", "zone": "az1"}},
    {"id": "cl-sandbox", "tags": {"env": "prod", "tier": "sandbox", "zone": "az2"}},
    {"id": "cl-az2-n2", "tags": {"env": "prod", "tier": "core", "zone": "az2"}}
  ]
}
```

运行 `./darksafe plan plan.json`，退出状态为 0，标准输出**只有**下面这份完整计划（标准错误为空）：

```json
{
  "app": {
    "name": "payments",
    "revision": "2026.10.0-r3",
    "image": "registry.example.net/payments:2026.10.0-r3"
  },
  "batches": [
    {
      "index": 1,
      "clusters": [
        "cl-az1-n1",
        "cl-az2-n1",
        "cl-az3-n1"
      ]
    },
    {
      "index": 2,
      "clusters": [
        "cl-az1-n2",
        "cl-az2-n2"
      ]
    }
  ],
  "excluded": [
    {
      "id": "cl-dev",
      "reason": "未命中包含条件"
    },
    {
      "id": "cl-drain",
      "reason": "命中排除条件"
    },
    {
      "id": "cl-off",
      "reason": "集群已停用"
    },
    {
      "id": "cl-sandbox",
      "reason": "未命中包含条件"
    }
  ]
}
```

逐行核对这份结果：

- **应用信息**：`app.name`、`app.revision`、`app.image` 与配置中的 `app`、`revision`、`image` 完全一致。
- **入选集群**（通过全部筛选、按标识升序）：`cl-az1-n1`、`cl-az1-n2`、`cl-az2-n1`、`cl-az2-n2`、`cl-az3-n1`。
- **批次 1**：按升序逐个考察，`cl-az1-n1`（故障域 `az1`）入选；`cl-az1-n2` 同属 `az1`，与本批冲突，延后；随后的 `cl-az2-n1`（`az2`）属于其他故障域，**仍然进入当前批次**；`cl-az2-n2` 与 `az2` 冲突，延后；`cl-az3-n1`（`az3`）再进入。于是批次 1 为 `[cl-az1-n1, cl-az2-n1, cl-az3-n1]`，恰好达到 `batchSize = 3`。
- **批次 2**：只剩 `cl-az1-n2`（`az1`）和 `cl-az2-n2`（`az2`），分为 `[cl-az1-n2, cl-az2-n2]`。该批只有 2 个集群——**批次允许不足额**，`batchSize` 是容量上限；故障域限制不会被容量覆盖。
- 同一故障域的两个集群（如 `az1` 的 `cl-az1-n1` 与 `cl-az1-n2`）被拆到不同批次；批次编号从 1 开始连续编号（1、2），每个入选集群恰好出现一次，批内仍按标识升序。
- **未入选集群**在 `excluded` 中**按标识升序**给出，每项含 `id` 与确定的中文原因：
  - `cl-dev`：`env=dev`，两个包含条件都不满足 → `未命中包含条件`；
  - `cl-drain`：虽满足第二个包含条件，但命中排除条件 `{"drain": "true"}` → `命中排除条件`（排除优先于包含）；
  - `cl-off`：`disabled: true` → `集群已停用`（停用优先于一切筛选）；
  - `cl-sandbox`：`tier=sandbox`，不满足任一包含条件 → `未命中包含条件`。

### 筛选与分批的关系

**筛选决定哪些集群入选，分批只安排入选集群；先筛选、后分批。**

- 一个标签条件内的键值需要**全部**匹配；多个条件之间满足**任意一个**即可。
- 判定优先级固定：**停用优先于排除，排除优先于包含**。即停用集群一律不入选；同时命中包含与排除时以排除为准。
- 未提供 `include` 时允许所有可用集群；未提供 `exclude` 时不排除任何集群。
- 未入选集群一律按标识升序在 `excluded` 中给出原因。**已被筛掉的集群不参与故障域检查，不需要携带 `spreadBy` 标签**——停用或被条件筛掉的集群即使没有该标签，也不会导致计划失败，只会带着自己的筛选原因出现在 `excluded` 中。
- 标签按键和值的**原字符串精确匹配**：区分大小写、不做修剪或归一化。**缺少标签与标签存在但值为空字符串不是同一情况**：
  - `{"zone": ""}` 表示标签存在、值为空——空字符串本身可以代表一个故障域，多个空值集群同属一个故障域，不能同批；
  - 完全没有 `zone` 键才是“缺少故障域标签”，对入选集群会导致计划失败（见下文）。
- 省略 `spreadBy` 字段，或将其设为空字符串 `""`，则保持普通分批：仅按标识升序、每批最多 `batchSize` 个集群切分，同一故障域的集群可以同批。

### 两种失败结果

计划失败时**不会**产生部分计划：退出状态非零，标准错误说明具体问题，标准输出为空。

#### 失败一：入选集群缺少故障域标签

只要有任一**入选**集群缺少 `spreadBy` 指定的标签，整个计划失败。若有多个问题集群，错误只指出**标识最小**的那个，并带上缺失的标签名。保存以下配置为 `missing-tag.json`：

```json
{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 3,
  "spreadBy": "zone",
  "clusters": [
    {"id": "cl-az2-n1", "tags": {"zone": "az2"}},
    {"id": "cl-az1-n2", "tags": {"zone": "az1"}},
    {"id": "cl-nozone-a", "tags": {"region": "cn-north"}},
    {"id": "cl-az1-n1", "tags": {"zone": "az1"}},
    {"id": "cl-nozone-b", "tags": {"rack": "r7"}}
  ]
}
```

`cl-nozone-a` 与 `cl-nozone-b` 都缺少 `zone` 标签，标准错误为（指出标签 `zone` 和标识最小的 `cl-nozone-a`，不提及 `cl-nozone-b`）：

```text
集群 "cl-nozone-a" 缺少故障域标签 "zone"
```

#### 失败二：所有候选都被筛掉

当筛选后没有任何集群入选时，**不会成功返回一份空批次的计划**，而是失败并在标准错误中按标识升序列出每个候选集群的未入选原因。保存以下配置为 `all-filtered.json`：

```json
{
  "app": "payments",
  "revision": "2026.10.0-r3",
  "image": "registry.example.net/payments:2026.10.0-r3",
  "batchSize": 2,
  "include": [
    {"env": "prod"}
  ],
  "exclude": [
    {"drain": "true"}
  ],
  "clusters": [
    {"id": "cl-x", "disabled": true, "tags": {"env": "prod"}},
    {"id": "cl-a", "tags": {"env": "dev"}},
    {"id": "cl-b", "tags": {"env": "prod", "drain": "true"}}
  ]
}
```

标准错误为：

```text
没有符合规则的可用集群，各候选集群未入选原因：
  cl-a：未命中包含条件
  cl-b：命中排除条件
  cl-x：集群已停用
```

注意停用的 `cl-x` 没有任何故障域标签也无关紧要——它先被“停用”筛掉，不进入标签检查。

### 输出与退出状态小结

| 结果 | 退出状态 | 标准输出 | 标准错误 |
| --- | --- | --- | --- |
| 计划成功 | `0` | 只有完整计划（`app`、从 1 连续编号的 `batches`、按标识升序的 `excluded`） | 为空 |
| 计划失败（配置不合法、缺少故障域标签、无集群入选等） | 非零 | 为空 | 说明具体问题 |

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

长期产品定位（零信任身份与授权决策平台）见 `PRODUCT_GOAL.md`；当前已落地并可离线验收的功能是上文所述的容器应用发布计划，`demo` 命令保留了身份与授权决策模型的可运行演示。
