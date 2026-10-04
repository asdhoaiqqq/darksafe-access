# 零信任身份与授权决策平台

## 用途

组织/成员/服务身份、角色与策略、作用域与委托、会话与撤销、访问决策解释、不可篡改审计链。

本仓库是可持续演进的自托管 Go 应用。领域核心位于 `darksafe/`，命令入口位于 `cmd/darksafe/`。

```bash
go run ./cmd/darksafe demo
go run ./cmd/darksafe version
printf '%s\n' '[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]' | go run ./cmd/darksafe ingest
go test ./...
```

`ingest` 从标准输入按行读取，逐行输出 JSON 结果，数据仅保存在本次进程内存中；写入与查询可交替进行，数组行为写入批次，对象行为查询，例如：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":0.5,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":2000,"labels":{"host":"a"}}' \
  | go run ./cmd/darksafe ingest
```

单行内容（不含行分隔符）上限为 67,108,864 个原始字节（64 MiB），恰好达到上限仍正常处理。超过上限的行作为整行失败：不会拆成多条输入，合法的 JSON 前缀也不会被当成完整请求，因此不写入任何点、不执行查询；该行输出一条 `status` 为 `error`、带原始行号的结果（不含 `index` 与 `conflict`），此前已成功提交的数据完整保留，随后各行按输入顺序继续处理。只要出现过失败行，命令最终返回非零退出码。

每行 JSON 只接受合法文本：原始字节必须是合法 UTF-8，字符串内的 `\uXXXX` 转义必须表示真实字符。单独的高代理项、单独的低代理项、以及高代理项后没有紧接合法低代理项转义都一律拒绝——不会把它们修补成 `�`（U+FFFD）。该规则同时作用于写入数组与查询对象中的指标名、标签键和标签值，因此损坏文本不会借替换字符混入合法序列（不会被误判为重复或冲突，损坏查询也不会错误命中）。中文、补充平面字符（直接书写或合法成对代理项转义）、以及用户显式输入的 `�` 或 `\uFFFD` 均可正常使用且身份相同；被转义的反斜杠后的 `uD800`（`\\uD800`）只是普通文本。这类损坏统一作为整行解析失败：即使数组前面的采样点完全合法也不提交该行任何点，结果只含一条无 `index`、无 `conflict` 的 error 并保留从 1 开始的原始行号，错误原因区分“UTF-8 字节不合法”与“代理项转义不合法”。

同一对象内不允许重复键，且字段名是否重复以 JSON 转义还原后的字符为准：采样点同时直接书写 `name`、又把首字母 n 写成十六进制转义（`\u006eame`），还原后两个键都是 name，即重复指标名字段；`labels` 内的 `host` 与 `\u0068ost` 同理为重复标签键。两个值完全相同也必须失败，值不同也不会静默选用其中一个或解释成两个标签/采样值冲突。写入批次里出错采样点按数组中从 1 开始的位置给出 `index`，错误原因指出重复字段或标签键且不附带 `conflict`，该批次前面的合法新增点同样不提交，此前成功写入的数据仍可查询到原来的数量与均值；查询对象或其标签条件出现重复键时返回查询错误，不带 `index` 与 `conflict`，也不返回查询结果。判重只在同一个对象内进行：不同采样点各自携带同名字段、采样点的字段名与其标签键同名都合法；两个合法采样点表示同一序列、同一时间戳且值相等时仍按既有重复采样点规则忽略并计入 duplicates。字段名只以转义形式出现一次时与直接书写同名同义，可照常写入与查询。

查询对 `[start,end]` 闭区间内的点按序列返回 `count` 与算术平均 `average`；`labels` 省略或为 `{}` 时匹配该指标的全部序列，否则按子集匹配。详见 `go run ./cmd/darksafe help`。

## 冲突处理：同一序列同一时间戳的再次提交

一条序列由指标名和完整标签集合共同确定，标签的书写顺序不影响身份（`{"host":"a","dc":"x"}` 与 `{"dc":"x","host":"a"}` 是同一条序列）。对同一序列的同一时间戳再次提交时，按数值是否相等分两种结果：

- **数值相等**：视为重复采样，成功忽略，计入该批结果的 `duplicates`。相等按存储值判断，`1` 与 `1.0` 是同一个值。
- **数值不同**：整批拒绝——既不覆盖已有值，也不保留本批前面已经校验通过的合法新增点；此前各批成功提交的数据原样保留。失败结果带 `conflict`，给出序列、时间戳、已有值 `existing` 与本次提交值 `submitted`。

成功结果中的 `added` 与 `duplicates` 只统计当前这一批；`series` 展示的则是截至目前的全部已提交数据，其中的旧点并不是本批新增的。

下面是一个连贯示例，写入与查询放在同一次命令调用内（数据只保存在本次进程内存中，进程结束即丢弃）。先向 `cpu`、`host=a` 写入时间戳 1000、值 2；第 2 行是空白行；第 3 行提交一个批次，包含新增点（时间戳 2000、值 4）和冲突点（时间戳 1000、值 9）；随后查询覆盖两个时间戳的区间；最后把冲突点改回值 2 再提交同一批次并复查：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]' \
  '' \
  '[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}' \
  '[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}' \
  | go run ./cmd/darksafe ingest
```

逐行输出（每个非空输入行对应一行结果）：

```json
{"status":"ok","added":1,"duplicates":0,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2}]}]}
{"status":"error","line":3,"index":2,"error":"conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9","conflict":{"series":{"name":"cpu","labels":{"host":"a"}},"timestamp":1000,"existing":2,"submitted":9}}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":1,"average":2}]}
{"status":"ok","added":1,"duplicates":1,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2},{"timestamp":2000,"value":4}]}]}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":2,"average":3}]}
```

读到失败行时可以这样定位与推断：

- `line` 是原始输入的行号，`index` 是出错采样点在数组内的位置，两者都从 1 开始。这里的 `line` 是 3 而不是 2：第 2 行的空白行不产生任何输出，但仍占一个行号。`index` 为 2 指向批内的第二个采样点，即时间戳 1000、值 9 的那一条。
- `conflict` 说明该位置已有值 2（`existing`）、本次提交的是 9（`submitted`）。整批被拒绝：时间戳 2000、值 4 这个合法新增点也没有写入，时间戳 1000 上仍是原来的 2，不会被覆盖成 9。紧随其后的查询证实了这一点：区间内仍只有一个点，`count` 为 1、`average` 为 2。
- 把冲突点改回与已有值相等的 2 后，同一批次成功：时间戳 2000 的点新增（`added` 为 1），时间戳 1000 的点作为同值重复被忽略（`duplicates` 为 1）。注意该批结果的 `series` 里时间戳 1000 的点来自第一批，不是本批新增；`added`/`duplicates` 也只反映本批。再次查询得到两个点，`count` 为 2、`average` 为 3。
- 失败不中断后续处理：失败行之后的各行仍按输入顺序正常执行，但只要出现过失败行，命令最终以非零退出码结束（本例退出码为 1）。

`conflict` 中的 `existing` 不一定来自此前已写入的数据。同一批次内首次向同一序列同一时间戳提交两个不同的值时，`existing` 指本批中较早出现的那个值；整批失败后这两个点都不会留下：

```bash
printf '%s\n' \
  '[{"name":"mem","timestamp":500,"value":7},{"name":"mem","timestamp":500,"value":8}]' \
  '{"op":"query","name":"mem","start":0,"end":1000}' \
  | go run ./cmd/darksafe ingest
```

```json
{"status":"error","line":1,"index":2,"error":"conflict: series mem{} at timestamp 500 already has value 7, submitted 8","conflict":{"series":{"name":"mem","labels":{}},"timestamp":500,"existing":7,"submitted":8}}
{"status":"ok","op":"query","series":[]}
```

`existing` 的 7 来自本批第一个采样点，并非已提交的数据；失败后查询返回空 `series`，说明 7 和 8 都没有写入。

并非所有 `status` 为 `error` 的结果都是数值冲突：字段校验失败（类型不对、缺字段、重复键、数值不合法等）或整行解析失败（非法 JSON、损坏文本、超长行）不附带 `conflict`，整行解析失败也不带 `index`。例如把 `value` 写成字符串：

```bash
printf '%s\n' '[{"name":"cpu","timestamp":1000,"value":"2"}]' | go run ./cmd/darksafe ingest
```

```json
{"status":"error","line":1,"index":1,"error":"field \"value\" must be a JSON number"}
```

## 数值结果的解读

### 存储与计算的精度约定

写入的 `value` 会被转换为一个有限的 float64（IEEE 754 双精度二进制浮点数）后存储，查询的 `average` 也以实际存储的值为准。这不等于承诺保留任意十进制精度：像 `0.1` 这样的常见十进制小数在二进制下无法精确表示，存储的是离它最近的可表示值；反过来，`1e16` 这样的大整数可以精确表示。超出 float64 有限范围（约 ±1.8e308）的写入值会被拒绝。

计算平均值时，先把区间内各点实际存储的值按有理数精确求和、精确除以点数，得到数学上精确的算术平均，再把这个精确结果舍入到最近的可表示 float64 输出。因此求和的中间过程不受 float64 范围限制，也不受点的写入顺序影响。

舍入只有一条规则：取离精确结果最近的可表示值；当精确结果恰好落在两个相邻可表示值的正中间时，选择二进制有效数字末位为零（“偶数”）的那一边。这是二进制层面的居中取偶，不是按十进制小数位做四舍五入——是否“居中”取决于二进制表示，往往对应一串很长的十进制数字。正数与负数遵循同一条规则；精确平均值为零时输出 `0`。

### 示例：正负大数抵消后，小值仍然参与结果

同一次进程内先写入再查询。下面向同一序列（`cpu`，标签 `host=a`）的三个时间戳分别写入 `1e16`、`1`、`-1e16`：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":1e16,"labels":{"host":"a"}},{"name":"cpu","timestamp":2000,"value":1,"labels":{"host":"a"}},{"name":"cpu","timestamp":3000,"value":-1e16,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":4000,"labels":{"host":"a"}}' \
  | go run ./cmd/darksafe ingest
```

逐行输出（每个非空输入行对应一行结果）：

```json
{"status":"ok","added":3,"duplicates":0,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":10000000000000000},{"timestamp":2000,"value":1},{"timestamp":3000,"value":-10000000000000000}]}]}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":3,"average":0.3333333333333333}]}
```

精确总和是 `1e16 + 1 + (-1e16) = 1`，精确平均是 `1/3`，输出 `0.3333333333333333` 是离 `1/3` 最近的可表示值。中间那个 `1` 没有被两个大数“吞掉”：求和是精确完成的，正负大数抵消后剩下的小余量完整保留，`count` 也如实为 3。

### 示例：总和超出 float64 范围，平均值仍然有限

两个不同时间戳上各写入 `1e308`：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":1e308},{"name":"cpu","timestamp":2000,"value":1e308}]' \
  '{"op":"query","name":"cpu","start":0,"end":3000}' \
  | go run ./cmd/darksafe ingest
```

输出：

```json
{"status":"ok","added":2,"duplicates":0,"series":[{"name":"cpu","labels":{},"points":[{"timestamp":1000,"value":1e+308},{"timestamp":2000,"value":1e+308}]}]}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{},"count":2,"average":1e+308}]}
```

两数之和约 `2e308`，已经超过 float64 能表示的最大有限值；如果先求和再除就会得到无穷大。这里求和在有理数上精确完成，平均值仍是有限的 `1e+308`。

### 示例：居中取偶

写入 `1` 与 `1.0000000000000002`（这是比 1 大的下一个可表示值）：

```bash
printf '%s\n' \
  '[{"name":"m","timestamp":10,"value":1},{"name":"m","timestamp":20,"value":1.0000000000000002}]' \
  '{"op":"query","name":"m","start":0,"end":100}' \
  | go run ./cmd/darksafe ingest
```

输出（第一行是写入结果，第二行是查询结果）：

```json
{"status":"ok","added":2,"duplicates":0,"series":[{"name":"m","labels":{},"points":[{"timestamp":10,"value":1},{"timestamp":20,"value":1.0000000000000002}]}]}
{"status":"ok","op":"query","series":[{"name":"m","labels":{},"count":2,"average":1}]}
```

精确平均恰好是 `1` 与 `1.0000000000000002` 正中间的值，两个候选一样近；`1` 的二进制有效数字末位是零，按居中取偶选中它，所以输出 `1` 而不是 `1.0000000000000002`。

### 哪些点参与计算

平均值按序列分别计算，每条命中的序列各自给出 `count` 与 `average`；只有闭区间 `[start,end]` 内已成功写入的点参与。区间外的点、以及因同序列同时间戳同值而被忽略的重复采样，既不增加 `count`，也不改变平均值。没有任何点的区间落在查询范围内时，返回的 `series` 是空数组——这表示“没有可统计的数据”，不能把它解释成平均值为零：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":2},{"name":"cpu","timestamp":5000,"value":100}]' \
  '[{"name":"cpu","timestamp":1000,"value":2}]' \
  '{"op":"query","name":"cpu","start":0,"end":2000}' \
  '{"op":"query","name":"cpu","start":2001,"end":4999}' \
  | go run ./cmd/darksafe ingest
```

```json
{"status":"ok","added":2,"duplicates":0,"series":[{"name":"cpu","labels":{},"points":[{"timestamp":1000,"value":2},{"timestamp":5000,"value":100}]}]}
{"status":"ok","added":0,"duplicates":1,"series":[{"name":"cpu","labels":{},"points":[{"timestamp":1000,"value":2},{"timestamp":5000,"value":100}]}]}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{},"count":1,"average":2}]}
{"status":"ok","op":"query","series":[]}
```

第二行写入是同值重复采样，被忽略（`duplicates` 计 1），不影响后续统计；第一条查询只覆盖时间戳 1000，`count` 为 1、`average` 为 2，区间外的 100 不参与；第二条查询的区间内没有任何点，返回空 `series` 数组。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
