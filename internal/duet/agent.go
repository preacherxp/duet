package duet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Agent interface {
	Plan(context.Context, *Task, string) (Plan, string, error)
	Implement(context.Context, *Task, string) (string, error)
	Review(context.Context, *Task, Round) (Review, string, error)
}

type CodexAgent struct{}

const planSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "summary", "steps", "risks"],
  "properties": {
    "title": {"type": "string", "minLength": 1},
    "summary": {"type": "string", "minLength": 1},
    "steps": {"type": "array", "minItems": 1, "items": {"type": "string"}},
    "risks": {"type": "array", "items": {"type": "string"}}
  }
}`

const reviewSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "summary", "findings"],
  "properties": {
    "verdict": {"type": "string", "enum": ["approved", "changes_requested"]},
    "summary": {"type": "string", "minLength": 1},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["severity", "file", "line", "message"],
        "properties": {
          "severity": {"type": "string", "enum": ["blocking", "non_blocking"]},
          "file": {"type": "string"},
          "line": {"type": "integer", "minimum": 0},
          "message": {"type": "string", "minLength": 1}
        }
      }
    }
  }
}`

func (CodexAgent) Plan(ctx context.Context, task *Task, feedback string) (Plan, string, error) {
	prompt := planPrompt(task, feedback)
	phase := fmt.Sprintf("plan-%02d", task.PlanRevisions)
	output, session, err := invokeCodex(ctx, task, "sol", phase, task.SolSession, task.SolModel, planSchema, prompt)
	if err != nil {
		return Plan{}, session, err
	}
	var plan Plan
	if err := decodeStrictJSON(output, &plan); err != nil {
		return Plan{}, session, fmt.Errorf("decode Sol plan: %w", err)
	}
	if strings.TrimSpace(plan.Title) == "" || strings.TrimSpace(plan.Summary) == "" || len(plan.Steps) == 0 {
		return Plan{}, session, errors.New("Sol returned an incomplete plan")
	}
	return plan, session, nil
}

func (CodexAgent) Implement(ctx context.Context, task *Task, feedback string) (string, error) {
	prompt := implementationPrompt(task, feedback)
	_, session, err := invokeCodex(ctx, task, "luna", fmt.Sprintf("implement-%02d", len(task.Rounds)+1), task.LunaSession, task.LunaModel, "", prompt)
	return session, err
}

func (CodexAgent) Review(ctx context.Context, task *Task, round Round) (Review, string, error) {
	prompt := reviewPrompt(task, round)
	output, session, err := invokeCodex(ctx, task, "sol", fmt.Sprintf("review-%02d", round.Number), task.SolSession, task.SolModel, reviewSchema, prompt)
	if err != nil {
		return Review{}, session, err
	}
	var review Review
	if err := decodeStrictJSON(output, &review); err != nil {
		return Review{}, session, fmt.Errorf("decode Sol review: %w", err)
	}
	if review.Verdict != "approved" && review.Verdict != "changes_requested" {
		return Review{}, session, fmt.Errorf("Sol returned invalid verdict %q", review.Verdict)
	}
	for _, finding := range review.Findings {
		if finding.Severity != "blocking" && finding.Severity != "non_blocking" {
			return Review{}, session, fmt.Errorf("Sol returned invalid finding severity %q", finding.Severity)
		}
	}
	return review, session, nil
}

func invokeCodex(ctx context.Context, task *Task, role, phase, session, model, schema, prompt string) ([]byte, string, error) {
	if task.CodexBin == "" {
		task.CodexBin = "codex"
	}
	dir := task.taskDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, session, fmt.Errorf("create agent evidence directory: %w", err)
	}
	lastMessagePath := filepath.Join(dir, role+"-"+phase+".json")
	eventPath := filepath.Join(dir, role+"-"+phase+".jsonl")
	errorPath := filepath.Join(dir, role+"-"+phase+".stderr.log")

	var schemaPath string
	if schema != "" {
		schemaPath = filepath.Join(dir, role+"-"+phase+".schema.json")
		if err := os.WriteFile(schemaPath, []byte(schema+"\n"), 0o644); err != nil {
			return nil, session, fmt.Errorf("write output schema: %w", err)
		}
	}

	args := codexArgs(task.Worktree, role, session, model, schemaPath, lastMessagePath)

	eventFile, err := os.Create(eventPath)
	if err != nil {
		return nil, session, fmt.Errorf("create %s %s event log: %w", role, phase, err)
	}
	errorFile, err := os.Create(errorPath)
	if err != nil {
		eventFile.Close()
		return nil, session, fmt.Errorf("create %s %s error log: %w", role, phase, err)
	}
	timeout := task.AgentTimeout
	if timeout <= 0 {
		timeout = DefaultAgentTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, task.CodexBin, args...)
	cmd.Dir = task.Worktree
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = eventFile
	cmd.Stderr = errorFile
	// Ask Codex to stop cleanly first; kill it if it has not exited shortly after.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	runErr := cmd.Run()
	switch {
	case ctx.Err() != nil:
		runErr = fmt.Errorf("interrupted: %w", ctx.Err())
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		runErr = fmt.Errorf("timed out after %s", timeout)
	}
	eventSyncErr := eventFile.Sync()
	errorSyncErr := errorFile.Sync()
	eventCloseErr := eventFile.Close()
	errorCloseErr := errorFile.Close()
	parsedSession := parseThreadIDFile(eventPath)
	if parsedSession != "" {
		session = parsedSession
	}
	if runErr != nil {
		detail := runErr.Error()
		if tail := readFileTail(errorPath, 4000); tail != "" {
			detail += ": " + tail
		}
		return nil, session, fmt.Errorf("%s %s failed: %s (events: %s; stderr: %s)", role, phase, detail, eventPath, errorPath)
	}
	if logErr := errors.Join(eventSyncErr, errorSyncErr, eventCloseErr, errorCloseErr); logErr != nil {
		return nil, session, fmt.Errorf("persist %s %s logs: %w", role, phase, logErr)
	}
	if session == "" {
		return nil, "", fmt.Errorf("%s %s did not emit a resumable thread ID (events: %s)", role, phase, eventPath)
	}
	if schema == "" {
		return nil, session, nil
	}
	output, err := os.ReadFile(lastMessagePath)
	if err != nil {
		return nil, session, fmt.Errorf("read %s %s result: %w", role, phase, err)
	}
	return output, session, nil
}

// codexArgs builds the argument list for one Codex invocation. Sandbox and
// approval policy are passed on every call, including resumed sessions:
// `codex exec resume` does not inherit the original session's sandbox and
// would otherwise fall back to the user's config and project trust level.
// Sol is read-only; Luna may write only inside the task worktree. Approval
// requests are never granted, so neither role can escape its sandbox.
func codexArgs(worktree, role, session, model, schemaPath, lastMessagePath string) []string {
	sandbox := "read-only"
	if role == "luna" {
		sandbox = "workspace-write"
	}
	args := []string{"exec"}
	if session != "" {
		args = append(args, "resume", "--json")
	} else {
		args = append(args, "--json", "--color", "never", "--cd", worktree)
	}
	args = append(args,
		"--config", `sandbox_mode="`+sandbox+`"`,
		"--config", `approval_policy="never"`,
	)
	if model != "" {
		args = append(args, "--model", model)
	}
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	args = append(args, "--output-last-message", lastMessagePath)
	if session != "" {
		args = append(args, session)
	}
	return append(args, "-")
}

func parseThreadID(data []byte) string {
	return parseThreadIDReader(bytes.NewReader(data))
}

func parseThreadIDFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	return parseThreadIDReader(file)
}

func parseThreadIDReader(reader io.Reader) string {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if id := findStringKey(event, "thread_id"); id != "" {
			return id
		}
	}
	return ""
}

func readFileTail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, _ := io.ReadAll(io.LimitReader(file, limit))
	return strings.TrimSpace(string(data))
}

func findStringKey(value any, key string) string {
	switch typed := value.(type) {
	case map[string]any:
		if raw, ok := typed[key]; ok {
			if result, ok := raw.(string); ok {
				return result
			}
		}
		for _, child := range typed {
			if result := findStringKey(child, key); result != "" {
				return result
			}
		}
	case []any:
		for _, child := range typed {
			if result := findStringKey(child, key); result != "" {
				return result
			}
		}
	}
	return ""
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func planPrompt(task *Task, feedback string) string {
	if task.SolSession == "" {
		return fmt.Sprintf(`You are Sol, the planner and independent reviewer in a two-agent workflow.

Analyze the repository and produce an implementation plan for this request:

%s

Declared acceptance checks:
%s

You are read-only. Do not edit files. Make the plan concrete enough for a separate implementer, call out material risks, and respect the repository's existing conventions.`, task.Request, bulletList(task.Checks))
	}
	return fmt.Sprintf(`Revise your implementation plan in light of this human feedback. Remain read-only.

Human feedback:
%s

Original request:
%s

Declared acceptance checks:
%s`, feedback, task.Request, bulletList(task.Checks))
}

func implementationPrompt(task *Task, feedback string) string {
	plan, _ := json.MarshalIndent(task.Plan, "", "  ")
	if task.LunaSession == "" {
		return fmt.Sprintf(`You are Luna, the implementer in a two-agent workflow. Work only in this isolated task worktree.

Implement the approved plan for the user's request. Inspect the repository, make complete production-quality changes, and run useful checks while developing. Do not commit, merge, rebase, create pull requests, or modify any branch outside this worktree. The orchestrator will run the declared acceptance checks after you finish.

User request:
%s

Approved plan:
%s

Declared acceptance checks:
%s

Human approval notes:
%s`, task.Request, plan, bulletList(task.Checks), emptyAsNone(feedback))
	}
	return fmt.Sprintf(`Continue implementing in the same worktree and resolve the evidence below. Do not commit or modify any other branch.

User or gate feedback:
%s

Latest check results and Sol review:
%s`, emptyAsNone(feedback), latestEvidenceSummary(task))
}

func reviewPrompt(task *Task, round Round) string {
	plan, _ := json.MarshalIndent(task.Plan, "", "  ")
	checks, _ := json.MarshalIndent(round.Checks, "", "  ")
	return fmt.Sprintf(`Act as Sol, the independent reviewer. Remain read-only. Inspect the current worktree and review the complete diff against the approved plan and user request.

Return "changes_requested" for any correctness, security, data-loss, compatibility, test, or material requirement issue. Use severity "blocking" for findings that must be fixed before human acceptance. Non-blocking observations may accompany an approved verdict. Do not approve merely because checks pass.

User request:
%s

Approved plan:
%s

Declared check results:
%s

Full diff evidence: %s
Git status evidence: %s`, task.Request, plan, checks, round.DiffPath, round.StatusPath)
}

func latestEvidenceSummary(task *Task) string {
	round := task.latestRound()
	if round == nil {
		return "No prior round evidence."
	}
	value := struct {
		Checks []CheckResult `json:"checks"`
		Review Review        `json:"review"`
	}{round.Checks, round.Review}
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func bulletList(values []string) string {
	var result strings.Builder
	for _, value := range values {
		result.WriteString("- ")
		result.WriteString(value)
		result.WriteByte('\n')
	}
	return strings.TrimSpace(result.String())
}

func emptyAsNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "None."
	}
	return value
}

func formatFinding(finding Finding) string {
	location := finding.File
	if finding.Line > 0 {
		location += ":" + strconv.Itoa(finding.Line)
	}
	if location == "" {
		location = "repository"
	}
	return fmt.Sprintf("[%s] %s: %s", finding.Severity, location, finding.Message)
}
