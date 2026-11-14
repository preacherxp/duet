package duet

import "time"

const StateVersion = 1

type Status string

const (
	StatusPlanning             Status = "planning"
	StatusAwaitingPlanApproval Status = "awaiting_plan_approval"
	StatusRunning              Status = "running"
	StatusAwaitingAcceptance   Status = "awaiting_acceptance"
	StatusPaused               Status = "paused"
	StatusAccepted             Status = "accepted"
)

type Plan struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Steps   []string `json:"steps"`
	Risks   []string `json:"risks"`
}

type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
}

type Review struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

type CheckResult struct {
	Command    string        `json:"command"`
	ExitCode   int           `json:"exit_code"`
	Duration   time.Duration `json:"duration"`
	LogPath    string        `json:"log_path"`
	OutputTail string        `json:"output_tail,omitempty"`
	ToolError  string        `json:"tool_error,omitempty"`
}

type Round struct {
	Number     int           `json:"number"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Checks     []CheckResult `json:"checks"`
	Review     Review        `json:"review"`
	DiffPath   string        `json:"diff_path"`
	StatusPath string        `json:"status_path"`
	ChecksPass bool          `json:"checks_pass"`
	GatePassed bool          `json:"gate_passed"`
}

type Task struct {
	Version         int           `json:"version"`
	ID              string        `json:"id"`
	Request         string        `json:"request"`
	RepoRoot        string        `json:"repo_root"`
	GitCommonDir    string        `json:"git_common_dir"`
	BaseRef         string        `json:"base_ref"`
	BaseCommit      string        `json:"base_commit"`
	Branch          string        `json:"branch"`
	Worktree        string        `json:"worktree"`
	Status          Status        `json:"status"`
	PauseReason     string        `json:"pause_reason,omitempty"`
	Plan            Plan          `json:"plan"`
	PlanRevisions   int           `json:"plan_revisions"`
	PlanApprovedAt  *time.Time    `json:"plan_approved_at,omitempty"`
	SolSession      string        `json:"sol_session,omitempty"`
	LunaSession     string        `json:"luna_session,omitempty"`
	SolModel        string        `json:"sol_model,omitempty"`
	LunaModel       string        `json:"luna_model,omitempty"`
	CodexBin        string        `json:"codex_bin"`
	Checks          []string      `json:"checks"`
	CheckTimeout    time.Duration `json:"check_timeout"`
	MaxRejections   int           `json:"max_rejections"`
	BatchRejections int           `json:"batch_rejections"`
	TotalRejections int           `json:"total_rejections"`
	Rounds          []Round       `json:"rounds"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	AcceptedAt      *time.Time    `json:"accepted_at,omitempty"`
	HandoffPath     string        `json:"handoff_path,omitempty"`
}

type StartOptions struct {
	Repo          string
	Name          string
	Request       string
	Base          string
	WorktreeRoot  string
	Checks        []string
	CheckTimeout  time.Duration
	MaxRejections int
	SolModel      string
	LunaModel     string
	CodexBin      string
}

func (t *Task) taskDir() string {
	return taskDir(t.GitCommonDir, t.ID)
}

func (t *Task) latestRound() *Round {
	if len(t.Rounds) == 0 {
		return nil
	}
	return &t.Rounds[len(t.Rounds)-1]
}

func (r Review) hasBlockingFindings() bool {
	for _, finding := range r.Findings {
		if finding.Severity == "blocking" {
			return true
		}
	}
	return false
}
