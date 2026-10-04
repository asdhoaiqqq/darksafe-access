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

## 数值精度与区间平均值

写入值按十进制 JSON 数字书写，但进入系统时会转换为有限的 **float64**（IEEE 双精度二进制浮点数）后存储；超出 float64 范围、无穷或 `NaN` 一律按既有规则拒绝。float64 只有约 16 位十进制有效数字，两个不同的十进制写法可能落到同一个存储值上（如 `1` 与 `1.0000000000000001`），所以系统**不承诺逐位保留任意十进制精度**，查询依据的始终是实际存储下来的 float64 值。

查询对每条匹配序列**分别**计算平均值：只取该序列在闭区间 `[start,end]` 内、此前已成功写入的点，先求出它们的**精确算术平均**（求和与除以 `count` 都按精确有理数完成，不做 float64 连加，与点的顺序和批次划分无关），再把结果**舍入到最近的可表示 float64** 输出。

恰好处在两个相邻可表示值正中间时，选择其中**二进制有效数字末位为零（偶数）**的那一个。这是二进制层面的“中点取偶”规则，正数与负数完全相同；不要按“小数点后第几位数四舍五入”这类十进制舍入去理解它——数轴上的可表示值不是均匀刻度，越靠近零越密、越远越疏，中点落在格子之间时挑末位为偶的格子，只是为了让大量舍入不系统性地偏向同一侧。平均值精确为零时输出 `0`。

下面的输入可直接交给现有 `ingest` 入口；写入行与查询行在**同一次进程内**交替提交，字段与结果结构沿用既有约定。

```bash
printf '%s\n' \
'[{"name":"signal","timestamp":1000,"value":1e16,"labels":{"host":"a"}},{"name":"signal","timestamp":2000,"value":1,"labels":{"host":"a"}},{"name":"signal","timestamp":3000,"value":-1e16,"labels":{"host":"a"}}]' \
'{"op":"query","name":"signal","start":0,"end":4000,"labels":{"host":"a"}}' \
'[{"name":"huge","timestamp":1,"value":1e308},{"name":"huge","timestamp":2,"value":1e308}]' \
'{"op":"query","name":"huge","start":1,"end":2}' \
'[{"name":"near","timestamp":1,"value":1},{"name":"near","timestamp":2,"value":1.0000000000000002}]' \
'{"op":"query","name":"near","start":1,"end":2}' \
'{"op":"query","name":"absent","start":1,"end":2}' \
| go run ./cmd/darksafe ingest
```

逐行对应输出（写入成功的结果会列出当前全部序列的完整身份与点）：

```json
{"status":"ok","added":3,"duplicates":0,"series":[{"name":"signal","labels":{"host":"a"},"points":[{"timestamp":1000,"value":10000000000000000},{"timestamp":2000,"value":1},{"timestamp":3000,"value":-10000000000000000}]}]}
{"status":"ok","op":"query","series":[{"name":"signal","labels":{"host":"a"},"count":3,"average":0.3333333333333333}]}
{"status":"ok","added":2,"duplicates":0,"series":[{"name":"huge","labels":{},"points":[{"timestamp":1,"value":1e+308},{"timestamp":2,"value":1e+308}]},{"name":"signal","labels":{"host":"a"},"points":[{"timestamp":1000,"value":10000000000000000},{"timestamp":2000,"value":1},{"timestamp":3000,"value":-10000000000000000}]}]}
{"status":"ok","op":"query","series":[{"name":"huge","labels":{},"count":2,"average":1e+308}]}
{"status":"ok","added":2,"duplicates":0,"series":[{"name":"huge","labels":{},"points":[{"timestamp":1,"value":1e+308},{"timestamp":2,"value":1e+308}]},{"name":"near","labels":{},"points":[{"timestamp":1,"value":1},{"timestamp":2,"value":1.0000000000000002}]},{"name":"signal","labels":{"host":"a"},"points":[{"timestamp":1000,"value":10000000000000000},{"timestamp":2000,"value":1},{"timestamp":3000,"value":-10000000000000000}]}]}
{"status":"ok","op":"query","series":[{"name":"near","labels":{},"count":2,"average":1}]}
{"status":"ok","op":"query","series":[]}
```

- **大数抵消后仍有很小的平均值**：`signal{host=a}` 三个点的存储值是 10000000000000000、1、-10000000000000000。两个大数都能被 float64 精确表示，精确求和后恰好剩下中间那个 `1`，再除以 3 得到 0.3333…，舍入为 `0.3333333333333333`。中间的 `1` 确实参与了结果：`count` 为 3，平均值不是 0。若用普通 float64 连加，`1e16+1` 会直接等于 `1e16`，这个 `1` 会被大数吞掉；本系统先精确求和、最后只舍入一次，不会发生这种丢失。
- **总和超出 float64 范围，平均值仍有限**：`huge` 两点之和 2e308 已超过 float64 的上限（约 1.7977e308，普通连加会变成 `+Inf`），但精确平均就是 `1e308`，所以 `average` 仍是有限的 `1e+308`。
- **中点取偶**：`1` 与 `1.0000000000000002` 是两个相邻的可表示值，它们的精确平均 1.0000000000000001 正好落在两者正中；`1` 的二进制有效数字末位为零，因此结果舍入为 `1`，而不是较大的那一侧。
- **没有区间内点时 `series` 为空数组**：对从未写入过的 `absent` 的查询返回 `"series":[]`，意思是没有任何序列在该区间内有点。它**不表示平均值为零**——没有点就没有平均值，结果中既不会出现 `count:0` 的序列，也不能把空数组解读成 0。

闭区间之外的点与被忽略的重复采样都不进入平均；平均值按序列各自计算，不跨序列求总平均：

```bash
printf '%s\n' \
'[{"name":"signal","timestamp":1000,"value":1e16,"labels":{"host":"a"}},{"name":"signal","timestamp":2000,"value":1,"labels":{"host":"a"}},{"name":"signal","timestamp":3000,"value":-1e16,"labels":{"host":"a"}},{"name":"signal","timestamp":2000,"value":1,"labels":{"host":"a"}},{"name":"signal","timestamp":9000,"value":100,"labels":{"host":"a"}}]' \
'{"op":"query","name":"signal","start":0,"end":4000,"labels":{"host":"a"}}' \
'{"op":"query","name":"signal","start":9000,"end":9000,"labels":{"host":"a"}}' \
'[{"name":"balance","timestamp":1,"value":1},{"name":"balance","timestamp":2,"value":-1}]' \
'{"op":"query","name":"balance","start":1,"end":2}' \
| go run ./cmd/darksafe ingest
```

```json
{"status":"ok","added":4,"duplicates":1,"series":[{"name":"signal","labels":{"host":"a"},"points":[{"timestamp":1000,"value":10000000000000000},{"timestamp":2000,"value":1},{"timestamp":3000,"value":-10000000000000000},{"timestamp":9000,"value":100}]}]}
{"status":"ok","op":"query","series":[{"name":"signal","labels":{"host":"a"},"count":3,"average":0.3333333333333333}]}
{"status":"ok","op":"query","series":[{"name":"signal","labels":{"host":"a"},"count":1,"average":100}]}
{"status":"ok","added":2,"duplicates":0,"series":[{"name":"balance","labels":{},"points":[{"timestamp":1,"value":1},{"timestamp":2,"value":-1}]},{"name":"signal","labels":{"host":"a"},"points":[{"timestamp":1000,"value":10000000000000000},{"timestamp":2000,"value":1},{"timestamp":3000,"value":-10000000000000000},{"timestamp":9000,"value":100}]}]}
{"status":"ok","op":"query","series":[{"name":"balance","labels":{},"count":2,"average":0}]}
```

- 时间戳 2000 的第二个采样与已有值完全相同，按重复采样忽略，只计入 `duplicates`：区间查询的 `count` 仍为 3，平均值不变。
- 时间戳 9000 的点在区间 `[0,4000]` 之外，不增加 `count`、不改变平均值；它没有丢失，用 `[9000,9000]` 单独查询仍能得到 `count` 为 1、`average` 为 100。
- `balance` 的 `1` 与 `-1` 精确抵消，平均值精确为零，输出 `"average":0`。

本节只说明数值与结果的含义：公开入口、请求与结果字段、写入/查询/错误处理行为均与上文完全一致，没有兼容性变化。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
