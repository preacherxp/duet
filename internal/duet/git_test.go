package duet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceIncludesUntrackedPathsVerbatim(t *testing.T) {
	repoPath := newTestRepository(t)
	repo, err := discoverRepository(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	branch, worktree, baseCommit, err := createWorktree(repo, "unusual-path", "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{
		ID: "unusual-path", RepoRoot: repoPath, GitCommonDir: repo.CommonDir,
		Branch: branch, Worktree: worktree, BaseCommit: baseCommit,
	}
	if err := os.MkdirAll(task.taskDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	name := " leading space\nand newline.txt"
	if err := os.WriteFile(filepath.Join(worktree, name), []byte("unusual content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diffPath, statusPath, err := captureGitEvidence(task, 1)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := os.ReadFile(diffPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diff), "unusual content") {
		t.Fatalf("untracked content missing from diff:\n%s", diff)
	}
	status, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(status) == 0 {
		t.Fatal("status evidence is empty")
	}
}
