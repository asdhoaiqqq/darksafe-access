// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	os.Exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

// execute runs one command and returns the process exit code. All output is
// routed through the given writers so command behavior is testable.
func execute(args []string, stdout, stderr io.Writer) int {
	command := "demo"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "demo":
		runDemo(stdout)
		return 0
	case "plan":
		return runPlan(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "darksafe 0.1.0")
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		usage(stdout)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: darksafe [demo|plan <配置文件>|version|help]

命令:
  demo   运行内置访问决策演示（默认命令）
  plan   离线预览发布计划，不连接 Kubernetes，不执行实际发布
  version 打印版本号
  help   显示本帮助

plan 输入（本地 JSON 文件）:
  application          应用名称（非空）
  revision             待发布修订号（非空）
  image                容器镜像（非空，不校验远端是否存在）
  clusters[]           候选集群: id（唯一非空）、disabled（缺省为可用）、
                       labels（字符串键值，缺省为空集合）
  batchSize            每批最多容纳的集群数量（正整数）
  include[] / exclude[] 标签条件列表；一个条件内所有键值精确相等才匹配，
                       多个条件满足任意一个即可；包含缺省表示允许所有可用
                       集群，排除优先于包含，停用集群始终不入选

plan 输出（标准输出 JSON，零状态退出）:
  application/revision/image 本次发布标识
  batches[]             从 1 开始连续编号的批次及每批集群标识（按标识升序）
  skipped[]             未入选集群及原因（按标识升序）:
                        集群已停用 / 命中排除条件 / 未命中包含条件

文件无法读取、JSON 非法、字段类型或校验不通过、没有可发布集群时，
以非零状态退出，具体问题输出到标准错误，标准输出保持为空。
`)
}

func runPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "plan 需要恰好一个参数: 配置文件的 JSON 路径")
		fmt.Fprintln(stderr, "用法: darksafe plan <配置文件>")
		return 2
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "无法读取配置文件 %q: %v\n", args[0], err)
		return 1
	}
	spec, err := darksafe.ParsePlanSpec(raw)
	if err != nil {
		fmt.Fprintf(stderr, "发布计划配置无效: %v\n", err)
		return 1
	}
	plan, err := darksafe.BuildPlan(spec)
	if err != nil {
		fmt.Fprintf(stderr, "无法生成发布计划: %v\n", err)
		return 1
	}
	out, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "发布计划序列化失败: %v\n", err)
		return 1
	}
	stdout.Write(append(out, '\n'))
	return 0
}

func runDemo(w io.Writer) {
	subjects := []darksafe.Subject{
		{ID: "u-1001", Kind: "user", Roles: []string{"org/payments/ledger:read"}},
		{ID: "svc-batch", Kind: "service", Roles: []string{"owner"}},
		{ID: "u-1002", Kind: "user", Disabled: true},
	}
	resource := darksafe.Resource{ID: "ledger-main", Scope: "org/payments/ledger"}
	allowed := 0
	for _, subject := range subjects {
		decision := darksafe.Access(subject, resource, "read")
		if decision.Allowed {
			allowed++
		}
		fmt.Fprintf(w, "subject=%s allowed=%v reason=%s\n", subject.ID, decision.Allowed, decision.Reason)
	}
	fmt.Fprintf(w, "summary: %d of %d subjects allowed\n", allowed, len(subjects))
}
