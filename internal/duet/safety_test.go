package duet

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// repoSnapshot captures everything about the user's checkout that duet must never change.
type repoSnapshot struct {
	head, symbolicHead, status, branches string
}

func snapshotRepository(t *testing.T, repo string) repoSnapshot {
	t.Helper()
	symbolic, _ := gitOutput(repo, "symbolic-ref", "-q", "HEAD")
	head, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	status, err := gitOutput(repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	branches, err := gitOutput(repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	if err != nil {
		t.Fatal(err)
	}
	// Task branches are expected to appear; only the user's own refs must be unchanged.
	var own []string
	for _, line := range strings.Split(branches, "\n") {
		if !strings.HasPrefix(line, "refs/heads/duet/") {
			own = append(own, line)
		}
	}
	return repoSnapshot{head: head, symbolicHead: symbolic, status: status, branches: strings.Join(own, "\n")}
}

func startOptions(repo, name string) StartOptions {
	return StartOptions{
		Repo: repo, Name: name, Request: "Create result.txt", Checks: []string{"test -f result.txt"},
		CheckTimeout: 5 * time.Second, MaxRejections: 2,
	}
}

func TestDirtyTreeAndDetachedHeadAreLeftUntouched(t *testing.T) {
	repo := newTestRepository(t)
	if err := os.WriteFile(filepath.Join(repo, "second.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "second.txt")
	runGit(t, repo, "commit", "-q", "-m", "second")
	runGit(t, repo, "checkout", "-q", "--detach", "HEAD~1")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("uncommitted edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotRepository(t, repo)

	var output bytes.Buffer
	app := NewApp(&output)
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Ready."}}
	task, err := app.Start(context.Background(), startOptions(repo, "dirty-detached"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "uncommitted changes") {
		t.Errorf("no warning about uncommitted changes:\n%s", output.String())
	}
	readme, err := os.ReadFile(filepath.Join(task.Worktree, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(readme) != "test\n" {
		t.Errorf("worktree did not start from the base commit: README.md = %q", readme)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
	if err := app.Accept(task); err != nil {
		t.Fatal(err)
	}
	if after := snapshotRepository(t, repo); after != before {
		t.Fatalf("user checkout changed\nbefore: %#v\nafter:  %#v", before, after)
	}
	if before.symbolicHead != "" {
		t.Fatalf("test setup: expected detached HEAD, got %s", before.symbolicHead)
	}
}

func TestExistingBranchOrWorktreePathIsRefused(t *testing.T) {
	repo := newTestRepository(t)
	runGit(t, repo, "branch", "duet/taken")
	before := snapshotRepository(t, repo)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{}
	if _, err := app.Start(context.Background(), startOptions(repo, "taken")); err == nil || !strings.Contains(err.Error(), "branch already exists") {
		t.Fatalf("existing branch error = %v", err)
	}
	repoInfo, err := discoverRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(taskDir(repoInfo.CommonDir, "taken")); !os.IsNotExist(err) {
		t.Errorf("failed start left a task directory behind: %v", err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := startOptions(repo, "occupied")
	options.WorktreeRoot = root
	if _, err := app.Start(context.Background(), options); err == nil || !strings.Contains(err.Error(), "worktree path already exists") {
		t.Fatalf("existing worktree path error = %v", err)
	}
	if after := snapshotRepository(t, repo); after != before {
		t.Fatalf("user checkout changed\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func TestSecondProcessCannotDriveALockedTask(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Ready."}}
	task, err := app.Start(context.Background(), startOptions(repo, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireTaskLock(task.GitCommonDir, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := RunCLI(context.Background(), []string{"approve", "locked", "--repo", repo}, &stdout, &stderr); code == 0 {
		t.Fatalf("approve ran while another process held the lock\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "in use by another duet process") {
		t.Fatalf("unexpected lock error: %s", stderr.String())
	}
	release()
	if _, err := os.Stat(lockPath(task.GitCommonDir, task.ID)); !os.IsNotExist(err) {
		t.Fatalf("lock not removed by release: %v", err)
	}
}

func TestStaleLockFromDeadProcessIsReplaced(t *testing.T) {
	repo := newTestRepository(t)
	repoInfo, err := discoverRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(taskDir(repoInfo.CommonDir, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(repoInfo.CommonDir, "stale"), []byte(strconv.Itoa(dead.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := acquireTaskLock(repoInfo.CommonDir, "stale")
	if err != nil {
		t.Fatalf("stale lock was not replaced: %v", err)
	}
	release()
}

type blockingAgent struct{ fakeAgent }

func (blockingAgent) Implement(ctx context.Context, _ *Task, _ string) (string, error) {
	<-ctx.Done()
	return "luna-session", ctx.Err()
}

func TestInterruptPausesAndCrashedRunCanBeResumed(t *testing.T) {
	repo := newTestRepository(t)
	before := snapshotRepository(t, repo)
	app := NewApp(&bytes.Buffer{})
	app.Agent = blockingAgent{}
	task, err := app.Start(context.Background(), startOptions(repo, "interrupted"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := app.Approve(ctx, task, ""); err == nil {
		t.Fatal("Approve() did not report the interruption")
	}
	loaded, err := LoadTask(repo, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusPaused {
		t.Fatalf("status after interrupt = %s", loaded.Status)
	}

	// Simulate a crash (kill -9) in the middle of a batch: state says running, no lock.
	loaded.Status = StatusRunning
	if err := saveTask(loaded); err != nil {
		t.Fatal(err)
	}
	var statusOut bytes.Buffer
	if code := RunCLI(context.Background(), []string{"status", task.ID, "--repo", repo}, &statusOut, &statusOut); code != 0 || !strings.Contains(statusOut.String(), "interrupted") {
		t.Fatalf("status did not report the interrupted run (exit %d):\n%s", code, statusOut.String())
	}
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Ready."}}
	if err := app.Resume(context.Background(), loaded, ""); err != nil {
		t.Fatalf("Resume() after crash: %v", err)
	}
	if loaded.Status != StatusAwaitingAcceptance {
		t.Fatalf("status after resume = %s", loaded.Status)
	}
	if after := snapshotRepository(t, repo); after != before {
		t.Fatalf("user checkout changed\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func writeFakeCodex(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHangingCodexTimesOutAndPauses(t *testing.T) {
	repo := newTestRepository(t)
	options := startOptions(repo, "hang")
	options.CodexBin = writeFakeCodex(t, "exec sleep 30")
	options.AgentTimeout = 200 * time.Millisecond
	started := time.Now()
	task, err := NewApp(&bytes.Buffer{}).Start(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Start() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("hung Codex was not stopped promptly: %s", elapsed)
	}
	if task.Status != StatusPaused || !strings.Contains(task.PauseReason, "timed out") {
		t.Fatalf("state after timeout: %s %q", task.Status, task.PauseReason)
	}
}

func TestFailingCodexIsReportedWithStderr(t *testing.T) {
	repo := newTestRepository(t)
	options := startOptions(repo, "failing")
	options.CodexBin = writeFakeCodex(t, "echo 'not signed in' >&2\nexit 3")
	task, err := NewApp(&bytes.Buffer{}).Start(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("Start() error = %v", err)
	}
	if task.Status != StatusPaused {
		t.Fatalf("status = %s", task.Status)
	}
}

func TestCodexArgsPinSandboxOnResume(t *testing.T) {
	for _, test := range []struct{ role, sandbox string }{{"sol", "read-only"}, {"luna", "workspace-write"}} {
		for _, session := range []string{"", "thread-1"} {
			args := strings.Join(codexArgs("/w", test.role, session, "", "", "/out.json"), " ")
			want := `--config sandbox_mode="` + test.sandbox + `" --config approval_policy="never"`
			if !strings.Contains(args, want) {
				t.Errorf("%s session=%q: %s does not contain %s", test.role, session, args, want)
			}
			if !strings.HasSuffix(args, " -") {
				t.Errorf("prompt must come from stdin: %s", args)
			}
		}
	}
}
