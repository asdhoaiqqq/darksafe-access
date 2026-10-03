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

查询对 `[start,end]` 闭区间内的点按序列返回 `count` 与算术平均 `average`；`labels` 省略或为 `{}` 时匹配该指标的全部序列，否则按子集匹配。详见 `go run ./cmd/darksafe help`。

## 技术方向

identity, authorization, rbac, audit-log, account-abstraction, zk-identity, wallet-security

## 运行要求

Go 1.26，仅使用标准库，全部行为可在本机 CPU 上离线复现。
