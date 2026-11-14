package duet

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type repository struct {
	Root      string
	CommonDir string
}

func discoverRepository(path string) (repository, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return repository{}, fmt.Errorf("resolve repository path: %w", err)
	}
	root, err := gitOutput(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return repository{}, fmt.Errorf("%s is not a Git worktree: %w", abs, err)
	}
	inside, err := gitOutput(root, "rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		return repository{}, fmt.Errorf("%s is not a non-bare Git worktree", root)
	}
	common, err := gitOutput(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return repository{}, fmt.Errorf("find Git common directory: %w", err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return repository{}, fmt.Errorf("resolve Git common directory: %w", err)
	}
	if _, err := gitOutput(root, "worktree", "list", "--porcelain"); err != nil {
		return repository{}, fmt.Errorf("repository does not support Git worktrees: %w", err)
	}
	return repository{Root: root, CommonDir: common}, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	output, err := gitRawOutput(dir, args...)
	return strings.TrimSpace(string(output)), err
}

func gitRawOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return stdout.Bytes(), nil
}

func createWorktree(repo repository, taskID, base, requestedRoot string) (branch, worktree, baseCommit string, err error) {
	if base == "" {
		base = "HEAD"
	}
	baseCommit, err = gitOutput(repo.Root, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return "", "", "", fmt.Errorf("resolve base %q: %w", base, err)
	}
	branch = "duet/" + taskID
	if requestedRoot == "" {
		requestedRoot = filepath.Join(filepath.Dir(repo.Root), "."+filepath.Base(repo.Root)+"-duet")
	}
	requestedRoot, err = filepath.Abs(requestedRoot)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve worktree root: %w", err)
	}
	worktree = filepath.Join(requestedRoot, taskID)
	if _, statErr := os.Stat(worktree); statErr == nil {
		return "", "", "", fmt.Errorf("worktree path already exists: %s", worktree)
	} else if !os.IsNotExist(statErr) {
		return "", "", "", fmt.Errorf("inspect worktree path: %w", statErr)
	}
	if err := os.MkdirAll(requestedRoot, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create worktree root: %w", err)
	}
	if _, err := gitOutput(repo.Root, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return "", "", "", fmt.Errorf("task branch already exists: %s", branch)
	}
	if _, err := gitOutput(repo.Root, "worktree", "add", "-b", branch, worktree, baseCommit); err != nil {
		return "", "", "", fmt.Errorf("create task worktree: %w", err)
	}
	return branch, worktree, baseCommit, nil
}

func captureGitEvidence(task *Task, round int) (diffPath, statusPath string, err error) {
	return captureGitEvidenceAs(task, fmt.Sprintf("round-%02d", round))
}

func captureGitEvidenceAs(task *Task, name string) (diffPath, statusPath string, err error) {
	diff, err := gitOutput(task.Worktree, "diff", "--binary", "HEAD")
	if err != nil {
		return "", "", err
	}
	status, err := gitOutput(task.Worktree, "status", "--short", "--untracked-files=all")
	if err != nil {
		return "", "", err
	}
	untracked, err := gitRawOutput(task.Worktree, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return "", "", err
	}
	var patch strings.Builder
	patch.WriteString(diff)
	if diff != "" {
		patch.WriteByte('\n')
	}
	for _, rawPath := range bytes.Split(untracked, []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		path := string(rawPath)
		cmd := exec.Command("git", "-C", task.Worktree, "diff", "--binary", "--no-index", "--", "/dev/null", path)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		runErr := cmd.Run()
		if exitErr, ok := runErr.(*exec.ExitError); runErr != nil && (!ok || exitErr.ExitCode() != 1) {
			return "", "", fmt.Errorf("capture untracked file %s: %s", path, strings.TrimSpace(out.String()))
		}
		patch.Write(out.Bytes())
		if out.Len() > 0 && out.Bytes()[out.Len()-1] != '\n' {
			patch.WriteByte('\n')
		}
	}
	diffPath = filepath.Join(task.taskDir(), name+".diff")
	statusPath = filepath.Join(task.taskDir(), name+".status")
	if err := os.WriteFile(diffPath, []byte(patch.String()), 0o644); err != nil {
		return "", "", fmt.Errorf("write diff evidence: %w", err)
	}
	if err := os.WriteFile(statusPath, []byte(status+"\n"), 0o644); err != nil {
		return "", "", fmt.Errorf("write status evidence: %w", err)
	}
	return diffPath, statusPath, nil
}
