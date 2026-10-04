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

同一对象内不允许重复键，且字段名是否重复以 JSON 转义还原后的字符为准：采样点同时直接书写 `name`、又把首字母 n 写成十六进制转义（`\u006eame`），还原后两个键都是 name，即重复指标名字段；`labels` 内的 `host` 与 `\u0068ost` 同理为重复标签键。两个值完全相同也必须失败，值不同也不会静默选用其中一个或解释成两个标签/采样值冲突。标签键的判重先于第二次值的类型校验：某个非空标签键第一次出现且值为合法字符串后再次出现，无论第二次的值是字符串、数字、布尔、null、对象还是数组，都报告重复标签键（原因指出转义还原后的键名），第二次值的类型不会把它掩盖成“标签值必须是字符串”；若该键第一次出现时值就不是字符串，仍报告那次值的类型错误。写入批次里出错采样点按数组中从 1 开始的位置给出 `index`，错误原因指出重复字段或标签键且不附带 `conflict`，该批次前面的合法新增点同样不提交，此前成功写入的数据仍可查询到原来的数量与均值；查询对象或其标签条件出现重复键时返回查询错误，不带 `index` 与 `conflict`，也不返回查询结果。判重只在同一个对象内进行：不同采样点各自携带同名字段、采样点的字段名与其标签键同名都合法；两个合法采样点表示同一序列、同一时间戳且值相等时仍按既有重复采样点规则忽略并计入 duplicates。字段名只以转义形式出现一次时与直接书写同名同义，可照常写入与查询。

查询对 `[start,end]` 闭区间内的点按序列返回 `count` 与算术平均 `average`；`labels` 省略或为 `{}` 时匹配该指标的全部序列，否则按子集匹配。详见 `go run ./cmd/darksafe help`。

## 重复与冲突：同一序列、同一时间戳的再次提交

一条序列由指标名加完整标签集合唯一确定；标签的书写顺序不影响身份，`{"host":"a","dc":"x"}` 与 `{"dc":"x","host":"a"}` 是同一条序列。对同一序列的同一时间戳再次提交时：

- **数值相等**（`1` 与 `1.0` 相等）：视为重复采样，成功忽略，计入该批结果的 `duplicates`，不改变已存数据。
- **数值不同**：视为冲突，整批被拒绝——已存在的值不会被覆盖，本批中排在前面的合法新增点也不会保留。写入不支持覆盖更新。

成功结果中的 `added` 与 `duplicates` 只统计当前这一批；`series` 则展示截至目前的全部已提交数据，其中此前批次写入的旧点也会列出，它们不是本批新增。

### 示例：冲突拒绝整批，改回等值后重提

写入与查询放在同一次命令调用内；数据只保存在本次进程的内存中，进程结束即丢弃，不写盘、也不跨进程保留。先向 `cpu`、`host=a` 写入时间戳 1000、值 2；再提交一批：时间戳 2000、值 4 的新增点在前，时间戳 1000、值 9 的冲突点在后；随后查询覆盖两者的区间；最后把冲突点改回与已存值相等的 2，原样重提同一批次并再次查询：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]' \
  '[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":9,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}' \
  '[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}' \
  | go run ./cmd/darksafe ingest
```

逐行输出：

```json
{"status":"ok","added":1,"duplicates":0,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2}]}]}
{"status":"error","line":2,"index":2,"error":"conflict: series cpu{host=a} at timestamp 1000 already has value 2, submitted 9","conflict":{"series":{"name":"cpu","labels":{"host":"a"}},"timestamp":1000,"existing":2,"submitted":9}}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":1,"average":2}]}
{"status":"ok","added":1,"duplicates":1,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2},{"timestamp":2000,"value":4}]}]}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":2,"average":3}]}
```

第二行失败：`index` 为 2，指出出错的是批内第 2 个采样点；`conflict` 给出序列身份、时间戳、已存在的值（`existing` 为 2）与本次提交的值（`submitted` 为 9）。整批被拒绝：时间戳 1000 上仍是原来的 2，批内排在前面、本身合法的新增点（时间戳 2000、值 4）也没有写入，所以随后的查询仍只有一个点、平均值为 2。第四行把冲突点改回 2 后重提同一批次：时间戳 2000 的点作为新增写入（`added` 为 1），时间戳 1000 的等值点被忽略（`duplicates` 为 1）；查询得到两个点、平均值为 3。

### 失败结果怎么读：line、index 与 conflict

- `line` 是原始输入的行号，`index` 是出错采样点在数组内的位置，两者都从 1 开始。空白行不输出任何结果，但仍占行号。
- 只有数值冲突才带 `conflict`。字段校验失败（缺字段、类型不对、重复键等）与整行解析失败都不附带 `conflict`——不要把所有 `error` 都当成数值冲突。
- `conflict` 中的 `existing` 不一定已经写入存储：同一批次内首次对同一位置提交两个不同值时，`existing` 指批内较早出现的那个值；失败后这两个点都不会留下。
- 某行失败后，后续各行仍按输入顺序继续处理；但只要出现过失败行，命令最终以非零退出码结束。

下面这次调用在第 2 行留了一个空白行：它不产生输出，但占用了行号，所以第 3 行的批内冲突报告 `line` 为 3。该批对同一时间戳 2000 先提交 4、再提交 5——这是同一位置在本批的首次提交，`existing` 是批内较早出现的 4，而不是已写入的数据；整批失败后这两个点都不存在，查询仍只有第 1 行写入的点：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}}]' \
  '' \
  '[{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":2000,"value":5,"labels":{"host":"a"}}]' \
  '{"op":"query","name":"cpu","start":0,"end":3000,"labels":{"host":"a"}}' \
  | go run ./cmd/darksafe ingest
```

```json
{"status":"ok","added":1,"duplicates":0,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2}]}]}
{"status":"error","line":3,"index":2,"error":"conflict: series cpu{host=a} at timestamp 2000 already has value 4, submitted 5","conflict":{"series":{"name":"cpu","labels":{"host":"a"}},"timestamp":2000,"existing":4,"submitted":5}}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":1,"average":2}]}
```

再对比两类不带 `conflict` 的失败：下面第 1 行批内第 2 个采样点的 `timestamp` 是字符串，属于字段校验失败，带 `index` 但不带 `conflict`；第 2 行整体不是合法 JSON，属于整行解析失败，`index` 与 `conflict` 都没有。两行失败后第 3 行仍正常处理并写入：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":2},{"name":"cpu","timestamp":"soon","value":3}]' \
  'not json' \
  '[{"name":"cpu","timestamp":2000,"value":4}]' \
  | go run ./cmd/darksafe ingest
```

```json
{"status":"error","line":1,"index":2,"error":"field \"timestamp\" must be a JSON number"}
{"status":"error","line":2,"error":"invalid JSON: invalid character 'o' in literal null (expecting 'u')"}
{"status":"ok","added":1,"duplicates":0,"series":[{"name":"cpu","labels":{},"points":[{"timestamp":2000,"value":4}]}]}
```

以上三次调用都包含失败行，因此命令均以非零退出码结束；用 `go run` 运行时它会在标准错误额外打印一行 `exit status 1`，那是 `go` 工具对非零退出码的转述，不是 ingest 的输出行。

## 查询失败：一次只返回一个原因

ingest 中的查询是一行独立的 JSON 对象。一条查询同时存在几个问题时，结果里只有一条 `error`：按下面确定的次序选出当前要报告的那一个原因，其余问题不合并成错误列表，也不返回任何部分统计结果（没有 `series`）。因此修正查询的方式是：按本次返回的原因修掉它指出的问题，重发查询，再看下一条原因。修掉一个问题后再次失败是正常现象——说明还有尚未修正的问题，新原因会指出下一个。

**整行解析先于一切字段校验。** 只有整行是唯一、完整且文本合法的 JSON 值（合法 UTF-8、代理项转义合法、对象闭合、对象之后没有第二个值或非空白内容）时，才进入字段校验。整行不满足时，即使对象前面已经写出有类型错误的字段，返回的仍是整行解析失败（`invalid JSON: ...`），前面的字段问题不会被当作最终原因。

**结构完整的对象按字段原始书写次序校验。** 已出现的字段按书写先后逐个检查，遇到未知字段、重复字段或类型错误时立即报告该问题。因此把未知字段和类型错误交换位置，可能得到不同原因——谁在前就先报告谁（见下文对照示例）。

**缺字段与倒置区间排在已出现字段之后，且两者也不混为一谈。** 已出现字段全部合法后，才检查必填项：同时缺少多个必填字段时按 op、name、start、end 的顺序报告第一个缺项，与字段在对象中的书写位置无关。四个必填项齐全且全部合法后，才可能报告 `start` 大于 `end` 的倒置区间，原因中给出两个边界的实际取值。

查询失败一律不带 `index` 与 `conflict`（`index` 只标记写入批内出错采样点的位置，`conflict` 只属于数值冲突），失败查询不改变已存数据，此前写入的点在后续合法查询中原样可见。

### 示例：同一次 ingest 中逐步修正一条查询

先向 `cpu`、`host=a` 写入三个点，再连续修正一条同时含有未知字段、类型错误与倒置区间的查询。第 2 行是空白行：它不产生输出，但仍占行号，所以第一条查询报告 `line` 为 3：

```bash
printf '%s\n' \
  '[{"name":"cpu","timestamp":1000,"value":2,"labels":{"host":"a"}},{"name":"cpu","timestamp":2000,"value":4,"labels":{"host":"a"}},{"name":"cpu","timestamp":3000,"value":6,"labels":{"host":"a"}}]' \
  '' \
  '{"op":"query","bogus":1,"name":7,"start":5000,"end":1000}' \
  '{"op":"query","name":7,"start":5000,"end":1000}' \
  '{"op":"query","name":"cpu","start":5000,"end":1000}' \
  '{"op":"query","name":"cpu","start":1000,"end":3000,"labels":{"host":"a"}}' \
  | go run ./cmd/darksafe ingest
```

逐行输出（第 2 行空白行无输出）：

```json
{"status":"ok","added":3,"duplicates":0,"series":[{"name":"cpu","labels":{"host":"a"},"points":[{"timestamp":1000,"value":2},{"timestamp":2000,"value":4},{"timestamp":3000,"value":6}]}]}
{"status":"error","line":3,"error":"unknown field \"bogus\""}
{"status":"error","line":4,"error":"field \"name\" must be a string"}
{"status":"error","line":5,"error":"invalid range: \"start\" must not be greater than \"end\" (5000 > 1000)"}
{"status":"ok","op":"query","series":[{"name":"cpu","labels":{"host":"a"},"count":3,"average":4}]}
```

第 3 行的查询同时有三个问题（未知字段 `bogus`、`name` 应为字符串、区间倒置），但只报告书写位置最靠前的未知字段。每次只改当前原因指出的问题：删掉 `bogus` 后，第 4 行暴露出 `name` 的类型错误；把 `name` 改成字符串后，第 5 行才报告倒置区间，并给出实际边界 `5000 > 1000`；把区间改为 `[1000,3000]` 后，第 6 行成功，返回本次 ingest 开头写入的三个点的统计（`count` 为 3、`average` 为 4）。注意本次调用中间出现过失败行，即使最后一行成功，命令仍以非零退出码结束。

### 对照：交换两个错误字段的位置

同样的两个问题——未知字段 `bogus` 与 `name` 的类型错误——书写顺序不同，报告的原因就不同：

```bash
printf '%s\n' \
  '{"op":"query","bogus":1,"name":7,"start":0,"end":1}' \
  '{"op":"query","name":7,"bogus":1,"start":0,"end":1}' \
  | go run ./cmd/darksafe ingest
```

```json
{"status":"error","line":1,"error":"unknown field \"bogus\""}
{"status":"error","line":2,"error":"field \"name\" must be a string"}
```

### 对照：整行解析失败与缺字段的报告顺序

```bash
printf '%s\n' \
  '{"op":"query","name":7,"start":0' \
  '{"end":100}' \
  '{"op":"query","end":100}' \
  '{"op":"query","name":"cpu"}' \
  '{"op":"query","name":"cpu","start":0}' \
  | go run ./cmd/darksafe ingest
```

```json
{"status":"error","line":1,"error":"invalid JSON: unexpected EOF"}
{"status":"error","line":2,"error":"missing required field \"op\""}
{"status":"error","line":3,"error":"missing required field \"name\""}
{"status":"error","line":4,"error":"missing required field \"start\""}
{"status":"error","line":5,"error":"missing required field \"end\""}
```

第 1 行的对象未闭合，不是唯一、完整的 JSON 值：即使 `name` 的类型错误写在前面，返回的仍是整行解析失败。后四行结构完整、已出现字段全部合法，进入必填项检查：第 2 行同时缺 op、name、start，按固定顺序报告 op；之后每补上一个字段，下一条原因按 op、name、start、end 的顺序指出下一个缺项，与字段书写位置无关。

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
