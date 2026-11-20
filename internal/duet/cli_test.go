package duet

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIWorkflowWithCodexProtocolDouble(t *testing.T) {
	repo := newTestRepository(t)
	fakeCodex := filepath.Join(t.TempDir(), "fake-codex")
	trace := filepath.Join(t.TempDir(), "codex-args.log")
	reviewCounter := filepath.Join(t.TempDir(), "review-counter")
	t.Setenv("DUET_TEST_TRACE", trace)
	t.Setenv("DUET_REVIEW_COUNTER", reviewCounter)
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$DUET_TEST_TRACE"
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-last-message)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
case "$output" in
  *sol-plan*)
    printf '%s\n' '{"title":"CLI flow","summary":"Create the result.","steps":["Write result.txt"],"risks":[]}' > "$output"
    thread="sol-thread"
    ;;
  *sol-review*)
	if [ -f "$DUET_REVIEW_COUNTER" ]; then
      printf '%s\n' '{"verdict":"approved","summary":"The result is ready.","findings":[]}' > "$output"
	else
	  : > "$DUET_REVIEW_COUNTER"
      printf '%s\n' '{"verdict":"changes_requested","summary":"One fix is required.","findings":[{"severity":"blocking","file":"result.txt","line":1,"message":"Run one correction round"}]}' > "$output"
	fi
    thread="sol-thread"
    ;;
  *luna-implement*)
    printf '%s\n' 'implemented' > "$output"
    printf '%s\n' 'done' > result.txt
    thread="luna-thread"
    ;;
  *)
    echo "unexpected invocation: $output" >&2
    exit 2
    ;;
esac
printf '{"type":"thread.started","thread_id":"%s"}\n' "$thread"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	startArgs := []string{
		"start", "--repo", repo, "--name", "cli-flow", "--check", "test -f result.txt",
		"--codex-bin", fakeCodex, "Create result.txt",
	}
	if code := RunCLI(context.Background(), startArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("start exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if code := RunCLI(context.Background(), []string{"status", "cli-flow", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("status exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if code := RunCLI(context.Background(), []string{"list", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("list exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if code := RunCLI(context.Background(), []string{"resume", "cli-flow", "--repo", repo, "--feedback", "keep the API stable"}, &stdout, &stderr); code != 0 {
		t.Fatalf("replan exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if code := RunCLI(context.Background(), []string{"approve", "cli-flow", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("approve exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if code := RunCLI(context.Background(), []string{"accept", "cli-flow", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("accept exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	task, err := LoadTask(repo, "cli-flow")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusAccepted || task.SolSession != "sol-thread" || task.LunaSession != "luna-thread" {
		t.Fatalf("final task state = %#v", task)
	}
	if !strings.Contains(stdout.String(), "original branch was not modified") {
		t.Fatalf("CLI did not print the handoff:\n%s", stdout.String())
	}
	invocations, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(invocations)), "\n")
	if len(lines) != 6 {
		t.Fatalf("Codex invocation count = %d\n%s", len(lines), invocations)
	}
	solReadOnly := `sandbox_mode="read-only"`
	lunaWrite := `sandbox_mode="workspace-write"`
	noApprovals := `approval_policy="never"`
	assertContainsAll(t, lines[0], "exec --json", solReadOnly, noApprovals, "--output-schema", "--output-last-message")
	assertContainsAll(t, lines[1], "exec resume --json", solReadOnly, noApprovals, "--output-schema", "sol-thread -")
	assertContainsAll(t, lines[2], "exec --json", lunaWrite, noApprovals)
	assertContainsAll(t, lines[3], "exec resume --json", solReadOnly, noApprovals, "--output-schema", "sol-thread -")
	assertContainsAll(t, lines[4], "exec resume --json", lunaWrite, noApprovals, "luna-thread -")
	assertContainsAll(t, lines[5], "exec resume --json", solReadOnly, noApprovals, "--output-schema", "sol-thread -")
	for _, line := range lines {
		for _, unsafe := range []string{"--approve-for-me", "--dangerously", "danger-full-access"} {
			if strings.Contains(line, unsafe) {
				t.Errorf("Codex invocation contains %s: %s", unsafe, line)
			}
		}
	}
	if _, err := os.Stat(lockPath(task.GitCommonDir, task.ID)); !os.IsNotExist(err) {
		t.Errorf("task lock was not released: %v", err)
	}
	for _, name := range []string{"sol-plan-01.jsonl", "sol-plan-02.jsonl"} {
		if _, err := os.Stat(filepath.Join(task.taskDir(), name)); err != nil {
			t.Errorf("missing preserved plan evidence %s: %v", name, err)
		}
	}
	if len(task.Rounds) != 2 || task.TotalRejections != 1 {
		t.Fatalf("automatic correction loop state: rounds=%d rejected=%d", len(task.Rounds), task.TotalRejections)
	}
}

func TestCommandHelpExitsSuccessfully(t *testing.T) {
	var output bytes.Buffer
	if code := RunCLI(context.Background(), []string{"start", "--help"}, &output, &output); code != 0 {
		t.Fatalf("help exit = %d\n%s", code, output.String())
	}
	if !strings.Contains(output.String(), "Usage: duet start") {
		t.Fatalf("missing command help:\n%s", output.String())
	}
}

func assertContainsAll(t *testing.T, value string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(value, part) {
			t.Errorf("%q does not contain %q", value, part)
		}
	}
}
