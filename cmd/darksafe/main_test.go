package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

const validPlan = `{
  "application": "billing",
  "revision": "r-42",
  "image": "registry.example/billing:r-42",
  "batchSize": 2,
  "clusters": [
    {"id": "c3", "disabled": true},
    {"id": "c1", "labels": {"env": "prod"}},
    {"id": "c2", "labels": {"env": "prod"}},
    {"id": "c4", "labels": {"env": "dev"}}
  ],
  "include": [{"env": "prod"}],
  "exclude": []
}`

func TestPlanSuccessOutputsJSON(t *testing.T) {
	path := writeTemp(t, "plan.json", validPlan)
	code, stdout, stderr := runCLI(t, "plan", path)
	if code != 0 {
		t.Fatalf("退出码 = %d, stderr = %s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("成功时 stderr 应为空, 得到 %q", stderr)
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatalf("stdout 不是合法 JSON: %v\n%s", err, stdout)
	}
	if plan["application"] != "billing" || plan["revision"] != "r-42" || plan["image"] != "registry.example/billing:r-42" {
		t.Fatalf("应用信息错误: %v", plan)
	}
	batches := plan["batches"].([]any)
	if len(batches) != 1 {
		t.Fatalf("期望 1 个批次, 得到 %v", batches)
	}
	first := batches[0].(map[string]any)
	if int(first["batch"].(float64)) != 1 {
		t.Fatalf("批次应从 1 编号: %v", first)
	}
	ids := first["clusters"].([]any)
	if len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Fatalf("批次集群错误: %v", ids)
	}
	skipped := plan["skipped"].([]any)
	if len(skipped) != 2 {
		t.Fatalf("期望 2 个未入选集群, 得到 %v", skipped)
	}
	// 只有 JSON 与一个结尾换行。
	if strings.Count(stdout, "\n") < 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("输出应以换行结尾: %q", stdout)
	}
}

func TestPlanDeterministicAcrossRuns(t *testing.T) {
	path := writeTemp(t, "plan.json", validPlan)
	_, out1, _ := runCLI(t, "plan", path)
	_, out2, _ := runCLI(t, "plan", path)
	if out1 != out2 {
		t.Fatalf("重复预览输出不一致:\n%s\n%s", out1, out2)
	}
}

func TestPlanFailuresKeepStdoutEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	badJSON := writeTemp(t, "bad.json", `{not json`)
	invalid := writeTemp(t, "invalid.json", `{
	  "application": "app", "revision": "v", "image": "img",
	  "batchSize": 0, "clusters": [{"id": "a"}]
	}`)
	dup := writeTemp(t, "dup.json", `{
	  "application": "app", "revision": "v", "image": "img",
	  "batchSize": 2, "clusters": [{"id": "a"}, {"id": "a"}]
	}`)
	none := writeTemp(t, "none.json", `{
	  "application": "app", "revision": "v", "image": "img",
	  "batchSize": 2, "clusters": [{"id": "a", "disabled": true}],
	  "include": [{"env": "prod"}]
	}`)
	empty := writeTemp(t, "empty.json", `{
	  "application": "app", "revision": "v", "image": "img",
	  "batchSize": 2, "clusters": []
	}`)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"文件不存在", []string{"plan", missing}, "无法读取配置文件"},
		{"JSON 格式错误", []string{"plan", badJSON}, "配置 JSON 无法解析"},
		{"校验不通过", []string{"plan", invalid}, "批次容量必须是正整数"},
		{"重复标识", []string{"plan", dup}, "集群标识重复"},
		{"无可发布集群", []string{"plan", none}, "没有符合规则的可用集群"},
		{"无可发布集群-列出原因", []string{"plan", none}, "集群已停用"},
		{"候选为空", []string{"plan", empty}, "未提供候选集群"},
		{"缺少参数", []string{"plan"}, "需要恰好一个参数"},
		{"参数过多", []string{"plan", "a", "b"}, "需要恰好一个参数"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tc.args...)
			if code == 0 {
				t.Errorf("期望非零退出码")
			}
			if stdout != "" {
				t.Errorf("失败时 stdout 必须为空, 得到 %q", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, 期望包含 %q", stderr, tc.want)
			}
		})
	}
}

func TestExistingCommandsPreserved(t *testing.T) {
	// 默认无参数仍是 demo。
	code, stdout, stderr := runCLI(t)
	if code != 0 || stderr != "" {
		t.Fatalf("默认 demo 失败: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "summary:") {
		t.Fatalf("demo 输出异常: %s", stdout)
	}

	code, stdout, _ = runCLI(t, "demo")
	if code != 0 || !strings.Contains(stdout, "subject=u-1001 allowed=true") {
		t.Fatalf("demo 子命令行为改变: code=%d out=%s", code, stdout)
	}

	code, stdout, _ = runCLI(t, "version")
	if code != 0 || strings.TrimSpace(stdout) != "darksafe 0.1.0" {
		t.Fatalf("version 输出异常: code=%d out=%q", code, stdout)
	}

	for _, helpArg := range []string{"help", "-h", "--help"} {
		code, stdout, stderr = runCLI(t, helpArg)
		if code != 0 || stderr != "" {
			t.Fatalf("%s 失败: code=%d stderr=%s", helpArg, code, stderr)
		}
		if !strings.Contains(stdout, "plan") || !strings.Contains(stdout, "batchSize") {
			t.Fatalf("%s 未说明 plan 功能:\n%s", helpArg, stdout)
		}
	}

	code, stdout, stderr = runCLI(t, "bogus")
	if code != 2 {
		t.Fatalf("未知命令退出码 = %d, 期望 2", code)
	}
	if !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Fatalf("未知命令 stderr 异常: %s", stderr)
	}
	if !strings.Contains(stdout, "usage:") {
		t.Fatalf("未知命令应附带 usage, 得到 %s", stdout)
	}
}
