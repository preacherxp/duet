package duet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type App struct {
	Agent Agent
	Out   io.Writer
	Now   func() time.Time
}

func NewApp(out io.Writer) *App {
	return &App{Agent: CodexAgent{}, Out: out, Now: time.Now}
}

func (app *App) Start(ctx context.Context, options StartOptions) (*Task, error) {
	if strings.TrimSpace(options.Request) == "" {
		return nil, errors.New("task request cannot be empty")
	}
	if len(options.Checks) == 0 {
		return nil, errors.New("declare at least one acceptance check with --check")
	}
	if options.MaxRejections < 1 {
		return nil, errors.New("--max-rejections must be at least 1")
	}
	if options.CheckTimeout <= 0 {
		return nil, errors.New("--check-timeout must be greater than zero")
	}
	repo, err := discoverRepository(options.Repo)
	if err != nil {
		return nil, err
	}
	if options.AgentTimeout < 0 {
		return nil, errors.New("--agent-timeout cannot be negative")
	}
	now := app.Now().UTC()
	id := makeTaskID(options.Name, options.Request, now)
	if _, err := os.Stat(statePath(repo.CommonDir, id)); err == nil {
		return nil, fmt.Errorf("task %q already exists", id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect task state: %w", err)
	}
	release, err := acquireTaskLock(repo.CommonDir, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := os.Stat(statePath(repo.CommonDir, id)); err == nil {
		return nil, fmt.Errorf("task %q already exists", id)
	}
	if dirty, _ := gitOutput(repo.Root, "--no-optional-locks", "status", "--porcelain", "--untracked-files=no"); dirty != "" {
		fmt.Fprintln(app.Out, "Note: uncommitted changes in your working tree are not copied; the task starts from the base commit.")
	}
	fmt.Fprintf(app.Out, "Creating isolated worktree for %s...\n", id)
	branch, worktree, baseCommit, err := createWorktree(repo, id, options.Base, options.WorktreeRoot)
	if err != nil {
		release()
		os.Remove(taskDir(repo.CommonDir, id))
		return nil, err
	}
	base := options.Base
	if base == "" {
		base = "HEAD"
	}
	codexBin := options.CodexBin
	if codexBin == "" {
		codexBin = "codex"
	}
	task := &Task{
		Version:       StateVersion,
		ID:            id,
		Request:       strings.TrimSpace(options.Request),
		RepoRoot:      repo.Root,
		GitCommonDir:  repo.CommonDir,
		BaseRef:       base,
		BaseCommit:    baseCommit,
		Branch:        branch,
		Worktree:      worktree,
		Status:        StatusPlanning,
		SolModel:      options.SolModel,
		LunaModel:     options.LunaModel,
		CodexBin:      codexBin,
		Checks:        append([]string(nil), options.Checks...),
		CheckTimeout:  options.CheckTimeout,
		AgentTimeout:  options.AgentTimeout,
		MaxRejections: options.MaxRejections,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := saveTask(task); err != nil {
		return task, err
	}
	if err := app.plan(ctx, task, ""); err != nil {
		return task, err
	}
	return task, nil
}

func (app *App) plan(ctx context.Context, task *Task, feedback string) error {
	fmt.Fprintln(app.Out, "Sol is preparing the plan...")
	task.Status = StatusPlanning
	task.PauseReason = ""
	task.PlanRevisions++
	task.UpdatedAt = app.Now().UTC()
	if err := saveTask(task); err != nil {
		return err
	}
	plan, session, err := app.Agent.Plan(ctx, task, feedback)
	if session != "" {
		task.SolSession = session
	}
	if err != nil {
		return app.pause(task, "unrecoverable Sol planning failure: "+err.Error(), err)
	}
	task.Plan = plan
	task.Status = StatusAwaitingPlanApproval
	task.PauseReason = ""
	task.UpdatedAt = app.Now().UTC()
	if err := saveTask(task); err != nil {
		return err
	}
	app.printPlan(task)
	fmt.Fprintf(app.Out, "\nHuman gate: review the plan, then run:\n  duet approve %s\n", task.ID)
	fmt.Fprintf(app.Out, "To request a revision:\n  duet resume %s --feedback %s\n", task.ID, shellQuote("your feedback"))
	return nil
}

func (app *App) Approve(ctx context.Context, task *Task, feedback string) error {
	if task.Status != StatusAwaitingPlanApproval {
		return fmt.Errorf("task %q is %s; only a task awaiting plan approval can be approved", task.ID, task.Status)
	}
	now := app.Now().UTC()
	task.PlanApprovedAt = &now
	return app.runAuthorizedBatch(ctx, task, feedback, "Plan approved. Starting Luna in "+task.Worktree)
}

func (app *App) Resume(ctx context.Context, task *Task, feedback string) error {
	switch task.Status {
	case StatusAwaitingPlanApproval:
		if strings.TrimSpace(feedback) == "" {
			return errors.New("plan revision requires --feedback")
		}
		return app.plan(ctx, task, feedback)
	case StatusPaused, StatusPlanning, StatusRunning:
		// Planning or running here means the previous duet process died
		// mid-step (the CLI holds the task lock, so no other process owns it).
		if task.PlanApprovedAt == nil {
			return app.plan(ctx, task, feedback)
		}
		return app.runAuthorizedBatch(ctx, task, feedback, "Human authorized another bounded batch for "+task.ID+".")
	case StatusAwaitingAcceptance:
		if strings.TrimSpace(feedback) == "" {
			return errors.New("rejecting final acceptance requires --feedback")
		}
		return app.runAuthorizedBatch(ctx, task, feedback, "Final acceptance declined; starting another bounded batch for "+task.ID+".")
	case StatusAccepted:
		return fmt.Errorf("task %q is already accepted", task.ID)
	default:
		return fmt.Errorf("task %q is %s and cannot be resumed", task.ID, task.Status)
	}
}

func (app *App) runAuthorizedBatch(ctx context.Context, task *Task, feedback, message string) error {
	task.Status = StatusRunning
	task.PauseReason = ""
	task.BatchRejections = 0
	task.UpdatedAt = app.Now().UTC()
	if err := saveTask(task); err != nil {
		return err
	}
	fmt.Fprintln(app.Out, message)
	return app.runLoop(ctx, task, feedback)
}

func (app *App) runLoop(ctx context.Context, task *Task, feedback string) error {
	for {
		select {
		case <-ctx.Done():
			return app.pause(task, "orchestrator interrupted: "+ctx.Err().Error(), ctx.Err())
		default:
		}
		number := len(task.Rounds) + 1
		round := Round{Number: number, StartedAt: app.Now().UTC()}
		fmt.Fprintf(app.Out, "\nRound %d: Luna is implementing...\n", number)
		session, err := app.Agent.Implement(ctx, task, feedback)
		feedback = ""
		if session != "" {
			task.LunaSession = session
		}
		if err != nil {
			diffPath, statusPath, evidenceErr := captureGitEvidence(task, number)
			round.DiffPath, round.StatusPath = diffPath, statusPath
			round.FinishedAt = app.Now().UTC()
			task.Rounds = append(task.Rounds, round)
			cause := errors.Join(err, evidenceErr)
			reason := "unrecoverable Luna tool failure: " + err.Error()
			if evidenceErr != nil {
				reason += "; evidence collection also failed: " + evidenceErr.Error()
			}
			return app.pause(task, reason, cause)
		}

		fmt.Fprintf(app.Out, "Round %d: running %d declared check(s)...\n", number, len(task.Checks))
		round.Checks = app.runChecks(ctx, task, number)
		round.ChecksPass = checksPassed(round.Checks)
		diffPath, statusPath, err := captureGitEvidence(task, number)
		if err != nil {
			round.FinishedAt = app.Now().UTC()
			task.Rounds = append(task.Rounds, round)
			return app.pause(task, "unrecoverable evidence collection failure: "+err.Error(), err)
		}
		round.DiffPath, round.StatusPath = diffPath, statusPath
		if reason := unrecoverableCheckFailure(round.Checks); reason != "" {
			round.FinishedAt = app.Now().UTC()
			task.Rounds = append(task.Rounds, round)
			return app.pause(task, "unrecoverable check tool failure: "+reason, nil)
		}

		fmt.Fprintf(app.Out, "Round %d: Sol is reviewing the diff and evidence...\n", number)
		review, session, err := app.Agent.Review(ctx, task, round)
		if session != "" {
			task.SolSession = session
		}
		if err != nil {
			round.FinishedAt = app.Now().UTC()
			task.Rounds = append(task.Rounds, round)
			return app.pause(task, "unrecoverable Sol review failure: "+err.Error(), err)
		}
		round.Review = review
		round.GatePassed = round.ChecksPass && review.Verdict == "approved" && !review.hasBlockingFindings()
		round.FinishedAt = app.Now().UTC()
		task.Rounds = append(task.Rounds, round)
		task.UpdatedAt = app.Now().UTC()
		app.printRound(round)

		if round.GatePassed {
			task.Status = StatusAwaitingAcceptance
			task.PauseReason = ""
			if err := saveTask(task); err != nil {
				return err
			}
			fmt.Fprintf(app.Out, "\nEvidence gate passed. Human final acceptance is required:\n  duet accept %s\n", task.ID)
			fmt.Fprintf(app.Out, "To reject with feedback:\n  duet resume %s --feedback %s\n", task.ID, shellQuote("your feedback"))
			return nil
		}

		task.BatchRejections++
		task.TotalRejections++
		if task.BatchRejections >= task.MaxRejections {
			reason := fmt.Sprintf("bounded review cutoff reached after %d rejected reviews in this batch", task.BatchRejections)
			return app.pause(task, reason, nil)
		}
		if err := saveTask(task); err != nil {
			return err
		}
		fmt.Fprintf(app.Out, "Gate rejected round %d; returning evidence to Luna (%d/%d).\n", number, task.BatchRejections, task.MaxRejections)
	}
}

func (app *App) runChecks(ctx context.Context, task *Task, round int) []CheckResult {
	results := make([]CheckResult, 0, len(task.Checks))
	for index, command := range task.Checks {
		result := CheckResult{Command: command, ExitCode: -1}
		result.LogPath = filepath.Join(task.taskDir(), fmt.Sprintf("round-%02d-check-%02d.log", round, index+1))
		app.runCheck(ctx, task, &result)
		results = append(results, result)
		fmt.Fprintf(app.Out, "  [%s] %s (%s)\n", passLabel(result.ExitCode == 0), command, result.Duration.Round(time.Millisecond))
	}
	return results
}

func (app *App) runCheck(ctx context.Context, task *Task, result *CheckResult) {
	logFile, err := os.Create(result.LogPath)
	if err != nil {
		result.ToolError = "create check log: " + err.Error()
		return
	}
	outputTail := &tailWriter{limit: 4000}
	output := io.MultiWriter(logFile, outputTail)
	started := app.Now()
	checkContext, cancel := context.WithTimeout(ctx, task.CheckTimeout)
	cmd := exec.CommandContext(checkContext, "/bin/sh", "-c", result.Command)
	cmd.Dir = task.Worktree
	cmd.Stdout = output
	cmd.Stderr = output
	runErr := cmd.Run()
	cancel()
	result.Duration = app.Now().Sub(started)
	if runErr == nil {
		result.ExitCode = 0
	} else if exitErr, ok := runErr.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	}
	switch {
	case errors.Is(checkContext.Err(), context.DeadlineExceeded):
		fmt.Fprintf(output, "\n[duet] check timed out after %s\n", task.CheckTimeout)
		result.ExitCode = 124
	case ctx.Err() != nil:
		result.ToolError = "orchestrator context ended: " + ctx.Err().Error()
		fmt.Fprintf(output, "\n[duet] %s\n", result.ToolError)
	case runErr != nil:
		fmt.Fprintf(output, "\n[duet] check failed: %s\n", runErr)
		if _, ok := runErr.(*exec.ExitError); !ok {
			result.ToolError = runErr.Error()
		}
	}
	if result.ExitCode == 127 {
		result.ToolError = "command not found (exit 127)"
	}
	if err := logFile.Close(); err != nil && result.ToolError == "" {
		result.ToolError = "close check log: " + err.Error()
	}
	result.OutputTail = outputTail.String()
}

type tailWriter struct {
	data  []byte
	limit int
}

func (writer *tailWriter) Write(data []byte) (int, error) {
	written := len(data)
	if written >= writer.limit {
		writer.data = append(writer.data[:0], data[written-writer.limit:]...)
		return written, nil
	}
	if overflow := len(writer.data) + written - writer.limit; overflow > 0 {
		copy(writer.data, writer.data[overflow:])
		writer.data = writer.data[:len(writer.data)-overflow]
	}
	writer.data = append(writer.data, data...)
	return written, nil
}

func (writer *tailWriter) String() string {
	return strings.TrimSpace(string(writer.data))
}

func checksPassed(results []CheckResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if result.ExitCode != 0 {
			return false
		}
	}
	return true
}

func unrecoverableCheckFailure(results []CheckResult) string {
	for _, result := range results {
		if result.ToolError != "" {
			return fmt.Sprintf("%s: %s", result.Command, result.ToolError)
		}
	}
	return ""
}

func (app *App) pause(task *Task, reason string, cause error) error {
	task.Status = StatusPaused
	task.PauseReason = reason
	task.UpdatedAt = app.Now().UTC()
	if saveErr := saveTask(task); saveErr != nil {
		if cause != nil {
			return fmt.Errorf("%v; additionally failed to save paused state: %w", cause, saveErr)
		}
		return saveErr
	}
	fmt.Fprintf(app.Out, "\nPaused: %s\n", reason)
	app.printEvidence(task)
	fmt.Fprintf(app.Out, "Human authorization is required to continue:\n  duet resume %s --feedback %s\n", task.ID, shellQuote("optional guidance"))
	if cause != nil {
		return cause
	}
	return nil
}

func (app *App) Accept(task *Task) error {
	if task.Status != StatusAwaitingAcceptance {
		return fmt.Errorf("task %q is %s; only a task awaiting acceptance can be accepted", task.ID, task.Status)
	}
	if round := task.latestRound(); round != nil {
		oldDiff, diffErr := os.ReadFile(round.DiffPath)
		oldStatus, statusErr := os.ReadFile(round.StatusPath)
		if diffErr != nil || statusErr != nil {
			cause := errors.New("cannot verify the reviewed evidence before acceptance")
			return app.pause(task, cause.Error(), cause)
		}
		currentDiffPath, currentStatusPath, captureErr := captureGitEvidenceAs(task, "acceptance-verification")
		if captureErr != nil {
			return app.pause(task, "cannot verify the worktree before acceptance: "+captureErr.Error(), captureErr)
		}
		currentDiff, diffErr := os.ReadFile(currentDiffPath)
		currentStatus, statusErr := os.ReadFile(currentStatusPath)
		if evidenceErr := errors.Join(diffErr, statusErr); evidenceErr != nil {
			return app.pause(task, "cannot read acceptance verification evidence: "+evidenceErr.Error(), evidenceErr)
		}
		if !bytes.Equal(oldDiff, currentDiff) || !bytes.Equal(oldStatus, currentStatus) {
			round.DiffPath = currentDiffPath
			round.StatusPath = currentStatusPath
			round.GatePassed = false
			cause := errors.New("worktree changed after Sol's reviewed evidence; another review batch is required")
			return app.pause(task, cause.Error(), cause)
		}
	}
	now := app.Now().UTC()
	task.Status = StatusAccepted
	task.AcceptedAt = &now
	task.UpdatedAt = now
	handoffPath := filepath.Join(task.taskDir(), "HANDOFF.md")
	task.HandoffPath = handoffPath
	handoff := buildHandoff(task)
	if err := os.WriteFile(handoffPath, []byte(handoff), 0o644); err != nil {
		return fmt.Errorf("write handoff: %w", err)
	}
	if err := saveTask(task); err != nil {
		return err
	}
	fmt.Fprintln(app.Out, handoff)
	return nil
}

func (app *App) printPlan(task *Task) {
	fmt.Fprintf(app.Out, "\nPlan: %s\n%s\n", task.Plan.Title, task.Plan.Summary)
	for index, step := range task.Plan.Steps {
		fmt.Fprintf(app.Out, "  %d. %s\n", index+1, step)
	}
	if len(task.Plan.Risks) > 0 {
		fmt.Fprintln(app.Out, "Risks:")
		for _, risk := range task.Plan.Risks {
			fmt.Fprintf(app.Out, "  - %s\n", risk)
		}
	}
}

func (app *App) printRound(round Round) {
	fmt.Fprintf(app.Out, "Round %d review: %s — %s\n", round.Number, round.Review.Verdict, round.Review.Summary)
	for _, finding := range round.Review.Findings {
		fmt.Fprintf(app.Out, "  - %s\n", formatFinding(finding))
	}
	if !round.ChecksPass {
		fmt.Fprintln(app.Out, "  - [blocking] one or more declared checks failed")
	}
}

func (app *App) printEvidence(task *Task) {
	if round := task.latestRound(); round != nil {
		fmt.Fprintf(app.Out, "Evidence:\n  diff: %s\n  status: %s\n", round.DiffPath, round.StatusPath)
		for _, check := range round.Checks {
			fmt.Fprintf(app.Out, "  check (%d): %s\n", check.ExitCode, check.LogPath)
			if check.ToolError != "" {
				fmt.Fprintf(app.Out, "  tool error: %s\n", check.ToolError)
			}
		}
		for _, finding := range round.Review.Findings {
			fmt.Fprintf(app.Out, "  finding: %s\n", formatFinding(finding))
		}
	}
}

func buildHandoff(task *Task) string {
	var result strings.Builder
	fmt.Fprintf(&result, "# Accepted duet task: %s\n\n", task.ID)
	fmt.Fprintf(&result, "The evidence gate and final human acceptance passed. The original branch was not modified.\n\n")
	fmt.Fprintf(&result, "- Worktree: `%s`\n", task.Worktree)
	fmt.Fprintf(&result, "- Task branch: `%s`\n", task.Branch)
	fmt.Fprintf(&result, "- Base: `%s` (`%s`)\n", task.BaseRef, task.BaseCommit)
	if round := task.latestRound(); round != nil {
		fmt.Fprintf(&result, "- Diff evidence: `%s`\n", round.DiffPath)
		fmt.Fprintf(&result, "- Status evidence: `%s`\n", round.StatusPath)
		for _, check := range round.Checks {
			fmt.Fprintf(&result, "- Check `%s`: exit %d; log `%s`\n", check.Command, check.ExitCode, check.LogPath)
		}
	}
	fmt.Fprintf(&result, "- Integration policy: not machine-readable by duet; the human must apply the repository's documented policy.\n")
	fmt.Fprintf(&result, "\nSuggested integration commands\n\n")
	fmt.Fprintf(&result, "```sh\n")
	fmt.Fprintf(&result, "git -C %s status --short\n", shellQuote(task.Worktree))
	fmt.Fprintf(&result, "git -C %s diff --check HEAD\n", shellQuote(task.Worktree))
	fmt.Fprintf(&result, "git -C %s add -A\n", shellQuote(task.Worktree))
	fmt.Fprintf(&result, "git -C %s commit -m %s\n", shellQuote(task.Worktree), shellQuote(task.Plan.Title))
	fmt.Fprintf(&result, "# Then use your repository policy: open a PR, or merge/cherry-pick the resulting commit.\n")
	fmt.Fprintf(&result, "```\n")
	return result.String()
}

func (app *App) PrintStatus(task *Task) {
	fmt.Fprintf(app.Out, "%s  %s\n", task.ID, task.Status)
	fmt.Fprintf(app.Out, "request: %s\nbranch:  %s\nworktree: %s\n", task.Request, task.Branch, task.Worktree)
	fmt.Fprintf(app.Out, "rounds: %d; rejected: %d total\n", len(task.Rounds), task.TotalRejections)
	if task.PauseReason != "" {
		fmt.Fprintf(app.Out, "pause: %s\n", task.PauseReason)
	}
	if task.Status == StatusPlanning || task.Status == StatusRunning {
		if pid, alive := taskLockHolder(task.GitCommonDir, task.ID); alive {
			fmt.Fprintf(app.Out, "in progress: duet process %d is working on this task\n", pid)
		} else {
			fmt.Fprintf(app.Out, "interrupted: no duet process is working on this task; continue with\n  duet resume %s\n", task.ID)
		}
	}
	if task.Status == StatusAwaitingPlanApproval {
		app.printPlan(task)
	}
	app.printEvidence(task)
	if task.HandoffPath != "" {
		fmt.Fprintf(app.Out, "handoff: %s\n", task.HandoffPath)
	}
}

func (app *App) PrintList(tasks []*Task) {
	if len(tasks) == 0 {
		fmt.Fprintln(app.Out, "No duet tasks in this repository.")
		return
	}
	fmt.Fprintln(app.Out, "TASK\tSTATUS\tROUNDS\tREQUEST")
	for _, task := range tasks {
		fmt.Fprintf(app.Out, "%s\t%s\t%d\t%s\n", task.ID, task.Status, len(task.Rounds), oneLine(task.Request, 60))
	}
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func makeTaskID(name, request string, now time.Time) string {
	source := strings.TrimSpace(name)
	if source == "" {
		source = request
	}
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(source), "-"), "-")
	if len(slug) > 36 {
		slug = strings.Trim(slug[:36], "-")
	}
	if slug == "" {
		slug = "task"
	}
	if strings.TrimSpace(name) != "" {
		return slug
	}
	return slug + "-" + now.Format("20060102-150405")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func passLabel(pass bool) string {
	if pass {
		return "pass"
	}
	return "FAIL"
}

func oneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit-1] + "…"
}

func LoadTask(repoPath, id string) (*Task, error) {
	repo, err := discoverRepository(repoPath)
	if err != nil {
		return nil, err
	}
	return loadTask(repo.CommonDir, id)
}

// LockTask takes the task lock and then loads the task, so the loaded state
// cannot be changed by another duet process until release is called.
func LockTask(repoPath, id string) (*Task, func(), error) {
	repo, err := discoverRepository(repoPath)
	if err != nil {
		return nil, nil, err
	}
	if !validTaskID.MatchString(id) {
		return nil, nil, fmt.Errorf("invalid task ID %q", id)
	}
	if _, err := os.Stat(statePath(repo.CommonDir, id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("task %q does not exist", id)
		}
		return nil, nil, fmt.Errorf("read task state: %w", err)
	}
	release, err := acquireTaskLock(repo.CommonDir, id)
	if err != nil {
		return nil, nil, err
	}
	task, err := loadTask(repo.CommonDir, id)
	if err != nil {
		release()
		return nil, nil, err
	}
	return task, release, nil
}

func ListTasks(repoPath string) ([]*Task, error) {
	repo, err := discoverRepository(repoPath)
	if err != nil {
		return nil, err
	}
	return listTasks(repo.CommonDir)
}
