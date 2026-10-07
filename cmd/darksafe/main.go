// Command darksafe is the 零信任身份与授权决策平台 entry point.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/asdhoaiqqq/darksafe-access/darksafe"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("darksafe 0.1.0")
	case "plan":
		runPlan(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: darksafe [demo|version|plan <JSON文件路径>|help]")
	fmt.Println()
	fmt.Println("  demo             运行内置演示")
	fmt.Println("  version          显示版本号")
	fmt.Println("  plan <JSON路径>  离线读取发布配置并输出发布计划（JSON），不连接集群、不执行发布")
	fmt.Println("  help             显示本帮助")
	fmt.Println()
	fmt.Println("plan 输入字段：app, revision, image, batchSize, clusters[], include[], exclude[], spreadBy（可选）, firstBatchSize（可选）")
	fmt.Println("plan 成功时向标准输出写入完整发布计划；失败时以非零状态退出，并在标准错误说明具体问题。")
}

func runPlan(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "用法: darksafe plan <JSON文件路径>")
		os.Exit(2)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法读取文件: %v\n", err)
		os.Exit(1)
	}
	in, err := darksafe.ParseReleaseInput(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	plan, err := darksafe.MakeReleasePlan(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "序列化发布计划失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func runDemo() {
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
		fmt.Printf("subject=%s allowed=%v reason=%s\n", subject.ID, decision.Allowed, decision.Reason)
	}
	fmt.Printf("summary: %d of %d subjects allowed\n", allowed, len(subjects))
}
