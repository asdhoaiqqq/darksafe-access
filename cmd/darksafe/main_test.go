package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runIngestInput(t *testing.T, input string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runIngest(strings.NewReader(input), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestIngestCLISuccess(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":0.5}]`,
		`[{"name":"cpu","timestamp":1000,"value":0.5},{"name":"cpu","timestamp":2000,"value":0.7}]`,
	}, "\n") + "\n"
	out, errOut, code := runIngestInput(t, input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output lines = %d, want 2: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], `"line":1`) || !strings.Contains(lines[0], `"ok":true`) {
		t.Fatalf("line 1 = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"new":1`) || !strings.Contains(lines[1], `"duplicate":1`) {
		t.Fatalf("line 2 = %s", lines[1])
	}
}

func TestIngestCLIFailureExitCode(t *testing.T) {
	input := strings.Join([]string{
		`[{"name":"cpu","timestamp":1000,"value":0.5}]`,
		`[{"name":"cpu","timestamp":1000,"value":0.9}]`,
	}, "\n") + "\n"
	out, _, code := runIngestInput(t, input)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output lines = %d, want 2", len(lines))
	}
	if !strings.Contains(lines[1], `"ok":false`) || !strings.Contains(lines[1], `"existing":0.5`) || !strings.Contains(lines[1], `"submitted":0.9`) {
		t.Fatalf("line 2 = %s", lines[1])
	}
}

func TestIngestCLIBlankLinesCounted(t *testing.T) {
	input := "\n\n[]\n\n"
	out, _, code := runIngestInput(t, input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("output lines = %d, want 1", len(lines))
	}
	if !strings.Contains(lines[0], `"line":3`) {
		t.Fatalf("line number = %s, want line 3", lines[0])
	}
}

func TestIngestCLIParseError(t *testing.T) {
	input := "[1,\n[]\n"
	out, _, code := runIngestInput(t, input)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output lines = %d, want 2", len(lines))
	}
	if !strings.Contains(lines[0], `"line":1`) || strings.Contains(lines[0], `"position"`) {
		t.Fatalf("parse error should report line only: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"line":2`) || !strings.Contains(lines[1], `"ok":true`) {
		t.Fatalf("line 2 should succeed: %s", lines[1])
	}
}

func TestIngestCLIEmptyInput(t *testing.T) {
	out, _, code := runIngestInput(t, "")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out != "" {
		t.Fatalf("output = %q, want empty", out)
	}
}

func TestIngestCLIEmptyBatch(t *testing.T) {
	out, _, code := runIngestInput(t, "[]\n")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"new":0`) || !strings.Contains(out, `"duplicate":0`) {
		t.Fatalf("output = %s", out)
	}
}

func TestUsageMentionsIngest(t *testing.T) {
	var buf bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	usage()
	w.Close()
	os.Stdout = old
	buf.ReadFrom(r)
	text := buf.String()
	if !strings.Contains(text, "ingest") {
		t.Fatalf("usage does not mention ingest: %s", text)
	}
	if !strings.Contains(text, "timestamp") || !strings.Contains(text, "labels") {
		t.Fatalf("usage does not describe input fields: %s", text)
	}
	if !strings.Contains(text, "exit") || !strings.Contains(text, "non-zero") {
		t.Fatalf("usage does not describe exit behavior: %s", text)
	}
}

func TestCommandSubprocess(t *testing.T) {
	if os.Getenv("DARKSAFE_SUBPROCESS") == "1" {
		main()
		return
	}

	run := func(args ...string) (string, int) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("executable: %v", err)
		}
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), "DARKSAFE_SUBPROCESS=1")
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err = cmd.Run()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("run %v: %v", args, err)
			}
		}
		return out.String(), code
	}

	out, code := run("version")
	if code != 0 || !strings.Contains(out, "0.1.0") {
		t.Fatalf("version: code=%d out=%q", code, out)
	}

	out, code = run("help")
	if code != 0 || !strings.Contains(out, "ingest") {
		t.Fatalf("help: code=%d out=%q", code, out)
	}

	out, code = run("demo")
	if code != 0 || !strings.Contains(out, "subject=") {
		t.Fatalf("demo: code=%d out=%q", code, out)
	}

	// No-arg behavior defaults to demo.
	out, code = run()
	if code != 0 || !strings.Contains(out, "subject=") {
		t.Fatalf("no-arg: code=%d out=%q", code, out)
	}

	_, code = run("unknown-command")
	if code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
}

func TestCommandSubprocessIngest(t *testing.T) {
	if os.Getenv("DARKSAFE_SUBPROCESS") == "1" {
		main()
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	cmd := exec.Command(exe, "ingest")
	cmd.Env = append(os.Environ(), "DARKSAFE_SUBPROCESS=1")
	cmd.Stdin = strings.NewReader(`[{"name":"cpu","timestamp":1000,"value":0.5}]` + "\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if !strings.Contains(out.String(), `"ok":true`) || !strings.Contains(out.String(), `"new":1`) {
		t.Fatalf("output = %s", out.String())
	}
}

func TestCommandSubprocessIngestFailure(t *testing.T) {
	if os.Getenv("DARKSAFE_SUBPROCESS") == "1" {
		main()
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	cmd := exec.Command(exe, "ingest")
	cmd.Env = append(os.Environ(), "DARKSAFE_SUBPROCESS=1")
	cmd.Stdin = strings.NewReader("not json\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	if err == nil {
		t.Fatal("ingest of invalid JSON should exit non-zero")
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want 1", err)
	}
	if !strings.Contains(out.String(), `"ok":false`) {
		t.Fatalf("output = %s", out.String())
	}
}
