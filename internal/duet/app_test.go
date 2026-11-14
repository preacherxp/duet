package duet

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeAgent struct {
	review Review
}

type sequenceAgent struct {
	fakeAgent
	reviews []Review
	next    int
}

func (agent *sequenceAgent) Review(_ context.Context, _ *Task, _ Round) (Review, string, error) {
	review := agent.reviews[agent.next]
	agent.next++
	return review, "sol-session", nil
}

func (fakeAgent) Plan(_ context.Context, _ *Task, _ string) (Plan, string, error) {
	return Plan{
		Title:   "Add generated result",
		Summary: "Create the requested result file.",
		Steps:   []string{"Create result.txt", "Run the declared checks"},
		Risks:   []string{"None beyond the isolated worktree"},
	}, "sol-session", nil
}

func (fakeAgent) Implement(_ context.Context, task *Task, _ string) (string, error) {
	err := os.WriteFile(filepath.Join(task.Worktree, "result.txt"), []byte("done\n"), 0o644)
	return "luna-session", err
}

func (agent fakeAgent) Review(_ context.Context, _ *Task, _ Round) (Review, string, error) {
	return agent.review, "sol-session", nil
}

func TestLifecyclePreservesOriginalBranchAndCreatesHandoff(t *testing.T) {
	repo := newTestRepository(t)
	var output bytes.Buffer
	app := NewApp(&output)
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Implementation is complete."}}
	app.Now = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }

	task, err := app.Start(context.Background(), StartOptions{
		Repo:          repo,
		Name:          "result-file",
		Request:       "Create result.txt",
		Checks:        []string{"test -f result.txt"},
		CheckTimeout:  time.Second,
		MaxRejections: 3,
		CodexBin:      "codex",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if task.Status != StatusAwaitingPlanApproval {
		t.Fatalf("status after Start = %s", task.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("original branch was modified before approval")
	}

	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if task.Status != StatusAwaitingAcceptance {
		t.Fatalf("status after Approve = %s, want %s", task.Status, StatusAwaitingAcceptance)
	}
	if len(task.Rounds) != 1 || !task.Rounds[0].GatePassed {
		t.Fatalf("rounds = %#v", task.Rounds)
	}
	if _, err := os.Stat(filepath.Join(repo, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("original branch was modified by implementation")
	}

	if err := app.Accept(task); err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	if task.Status != StatusAccepted {
		t.Fatalf("status after Accept = %s", task.Status)
	}
	handoff, err := os.ReadFile(task.HandoffPath)
	if err != nil {
		t.Fatalf("read handoff: %v", err)
	}
	if !strings.Contains(string(handoff), task.Branch) || !strings.Contains(string(handoff), "original branch was not modified") {
		t.Fatalf("handoff missing isolation evidence:\n%s", handoff)
	}
	loaded, err := LoadTask(repo, task.ID)
	if err != nil {
		t.Fatalf("LoadTask() error = %v", err)
	}
	if loaded.Status != StatusAccepted || loaded.SolSession != "sol-session" || loaded.LunaSession != "luna-session" {
		t.Fatalf("persisted task = %#v", loaded)
	}
}

func TestReviewLoopStopsAtConfiguredCutoff(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{review: Review{
		Verdict:  "changes_requested",
		Summary:  "A blocking issue remains.",
		Findings: []Finding{{Severity: "blocking", File: "result.txt", Line: 1, Message: "Needs more work"}},
	}}
	task, err := app.Start(context.Background(), StartOptions{
		Repo: repo, Name: "cutoff", Request: "Create result.txt", Checks: []string{"test -f result.txt"},
		CheckTimeout: time.Second, MaxRejections: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if task.Status != StatusPaused || len(task.Rounds) != 3 || task.BatchRejections != 3 {
		t.Fatalf("cutoff state: status=%s rounds=%d rejections=%d", task.Status, len(task.Rounds), task.BatchRejections)
	}
	if !strings.Contains(task.PauseReason, "cutoff") {
		t.Fatalf("pause reason = %q", task.PauseReason)
	}
}

func TestFailedCheckOverridesApprovedReview(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Diff looks correct."}}
	task, err := app.Start(context.Background(), StartOptions{
		Repo: repo, Name: "failed-check", Request: "Create result.txt", Checks: []string{"test -f missing.txt"},
		CheckTimeout: time.Second, MaxRejections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusPaused || task.Rounds[0].GatePassed || task.Rounds[0].ChecksPass {
		t.Fatalf("failed check passed gate: %#v", task.Rounds[0])
	}
}

func TestMissingCheckToolPausesImmediately(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Diff looks correct."}}
	task, err := app.Start(context.Background(), StartOptions{
		Repo: repo, Name: "missing-tool", Request: "Create result.txt", Checks: []string{"definitely-not-a-real-duet-command"},
		CheckTimeout: time.Second, MaxRejections: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusPaused || len(task.Rounds) != 1 || !strings.Contains(task.PauseReason, "tool failure") {
		t.Fatalf("missing tool state: status=%s rounds=%d reason=%q", task.Status, len(task.Rounds), task.PauseReason)
	}
}

func TestResumeStartsANewBoundedBatchWithPersistentSessions(t *testing.T) {
	repo := newTestRepository(t)
	agent := &sequenceAgent{reviews: []Review{
		{Verdict: "changes_requested", Summary: "Fix it.", Findings: []Finding{{Severity: "blocking", Message: "Not ready"}}},
		{Verdict: "approved", Summary: "Ready now."},
	}}
	app := NewApp(&bytes.Buffer{})
	app.Agent = agent
	task, err := app.Start(context.Background(), StartOptions{
		Repo: repo, Name: "resume", Request: "Create result.txt", Checks: []string{"test -f result.txt"},
		CheckTimeout: time.Second, MaxRejections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusPaused || task.SolSession == "" || task.LunaSession == "" {
		t.Fatalf("first batch state: %#v", task)
	}
	if err := app.Resume(context.Background(), task, "address the finding"); err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusAwaitingAcceptance || len(task.Rounds) != 2 || task.BatchRejections != 0 || task.TotalRejections != 1 {
		t.Fatalf("resumed batch state: status=%s rounds=%d batch=%d total=%d", task.Status, len(task.Rounds), task.BatchRejections, task.TotalRejections)
	}
}

func TestTimedOutCheckIsRecordedAndRejected(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Diff looks correct."}}
	task, err := app.Start(context.Background(), StartOptions{
		Repo: repo, Name: "timeout", Request: "Create result.txt", Checks: []string{"sleep 1"},
		CheckTimeout: 20 * time.Millisecond, MaxRejections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
	check := task.Rounds[0].Checks[0]
	if task.Status != StatusPaused || check.ExitCode != 124 || !strings.Contains(check.OutputTail, "timed out") {
		t.Fatalf("timeout result: status=%s check=%#v", task.Status, check)
	}
}

func TestCheckOutputIsStreamedToLogAndTailIsBounded(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	task := &Task{Worktree: repo, GitCommonDir: filepath.Join(repo, ".git"), ID: "large-log", CheckTimeout: time.Second}
	if err := os.MkdirAll(task.taskDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	results := app.runChecks(context.Background(), task, 1)
	if len(results) != 0 {
		t.Fatalf("expected no undeclared checks, got %#v", results)
	}
	task.Checks = []string{"yes x | head -c 100000"}
	results = app.runChecks(context.Background(), task, 1)
	if results[0].ExitCode != 0 || len(results[0].OutputTail) > 4000 {
		t.Fatalf("large check result = %#v", results[0])
	}
	info, err := os.Stat(results[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 100000 {
		t.Fatalf("full log was not preserved: %d bytes", info.Size())
	}
}

func TestAcceptanceRejectsChangesAfterReview(t *testing.T) {
	repo := newTestRepository(t)
	app := NewApp(&bytes.Buffer{})
	app.Agent = fakeAgent{review: Review{Verdict: "approved", Summary: "Ready."}}
	task, err := app.Start(context.Background(), StartOptions{
		Repo: repo, Name: "stale-evidence", Request: "Create result.txt", Checks: []string{"test -f result.txt"},
		CheckTimeout: time.Second, MaxRejections: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Approve(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.Worktree, "result.txt"), []byte("changed after review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := app.Accept(task); err == nil {
		t.Fatal("Accept() unexpectedly accepted stale evidence")
	}
	if task.Status != StatusPaused || !strings.Contains(task.PauseReason, "changed after") {
		t.Fatalf("stale evidence state: %s %q", task.Status, task.PauseReason)
	}
}

func TestParseThreadID(t *testing.T) {
	data := []byte("{\"type\":\"message\"}\n{\"type\":\"thread.started\",\"payload\":{\"thread_id\":\"abc-123\"}}\n")
	if got := parseThreadID(data); got != "abc-123" {
		t.Fatalf("parseThreadID() = %q", got)
	}
}

func TestLoadTaskRejectsPathTraversal(t *testing.T) {
	repo := newTestRepository(t)
	if _, err := LoadTask(repo, "../../config"); err == nil || !strings.Contains(err.Error(), "invalid task ID") {
		t.Fatalf("LoadTask() path traversal error = %v", err)
	}
}

func newTestRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.name", "Duet Test")
	runGit(t, root, "config", "user.email", "duet-test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "README.md")
	runGit(t, root, "commit", "-q", "-m", "initial")
	return root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
