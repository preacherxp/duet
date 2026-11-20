package duet

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

const Version = "0.1.0"

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ", ") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printRootHelp(stdout)
		return 2
	}
	app := NewApp(stdout)
	var err error
	switch args[0] {
	case "start", "run":
		err = runStart(ctx, app, args[1:], stderr)
	case "approve":
		err = runApprove(ctx, app, args[1:], stderr)
	case "resume":
		err = runResume(ctx, app, args[1:], stderr)
	case "accept":
		err = runAccept(app, args[1:], stderr)
	case "status":
		err = runStatus(app, args[1:], stderr)
	case "list":
		err = runList(app, args[1:], stderr)
	case "help", "-h", "--help":
		printRootHelp(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "duet %s\n", Version)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printRootHelp(stderr)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "duet: %v\n", err)
		return 1
	}
	return 0
}

func runStart(ctx context.Context, app *App, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options StartOptions
	var checks stringList
	set.StringVar(&options.Repo, "repo", ".", "target Git repository")
	set.StringVar(&options.Name, "name", "", "stable task ID (defaults to a request slug plus timestamp)")
	set.StringVar(&options.Base, "base", "HEAD", "commit or ref from which to create the task branch")
	set.StringVar(&options.WorktreeRoot, "worktree-root", "", "parent directory for isolated task worktrees")
	set.Var(&checks, "check", "acceptance check command; repeat for multiple checks")
	set.DurationVar(&options.CheckTimeout, "check-timeout", 10*time.Minute, "timeout for each declared check")
	set.DurationVar(&options.AgentTimeout, "agent-timeout", DefaultAgentTimeout, "timeout for each Codex invocation (Sol plan or review, Luna round)")
	set.IntVar(&options.MaxRejections, "max-rejections", 3, "rejected reviews allowed per automatic batch")
	set.StringVar(&options.SolModel, "sol-model", "", "Codex model override for Sol")
	set.StringVar(&options.LunaModel, "luna-model", "", "Codex model override for Luna")
	set.StringVar(&options.CodexBin, "codex-bin", "codex", "path to the Codex executable")
	set.Usage = func() {
		fmt.Fprintln(stderr, "Usage: duet start [options] <task request>")
		set.PrintDefaults()
	}
	if err := set.Parse(args); err != nil {
		return err
	}
	if len(set.Args()) == 0 {
		set.Usage()
		return errors.New("missing task request")
	}
	options.Request = strings.Join(set.Args(), " ")
	options.Checks = checks
	_, err := app.Start(ctx, options)
	return err
}

func runApprove(ctx context.Context, app *App, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("approve", flag.ContinueOnError)
	set.SetOutput(stderr)
	var repo, feedback string
	set.StringVar(&repo, "repo", ".", "target Git repository")
	set.StringVar(&feedback, "feedback", "", "human notes passed to Luna")
	id, err := parseTaskCommand(set, args, "duet approve [options] <task-id>")
	if err != nil {
		return err
	}
	task, release, err := LockTask(repo, id)
	if err != nil {
		return err
	}
	defer release()
	return app.Approve(ctx, task, feedback)
}

func runResume(ctx context.Context, app *App, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("resume", flag.ContinueOnError)
	set.SetOutput(stderr)
	var repo, feedback string
	set.StringVar(&repo, "repo", ".", "target Git repository")
	set.StringVar(&feedback, "feedback", "", "human guidance for the next plan or implementation batch")
	id, err := parseTaskCommand(set, args, "duet resume [options] <task-id>")
	if err != nil {
		return err
	}
	task, release, err := LockTask(repo, id)
	if err != nil {
		return err
	}
	defer release()
	return app.Resume(ctx, task, feedback)
}

func runAccept(app *App, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("accept", flag.ContinueOnError)
	set.SetOutput(stderr)
	var repo string
	set.StringVar(&repo, "repo", ".", "target Git repository")
	id, err := parseTaskCommand(set, args, "duet accept [options] <task-id>")
	if err != nil {
		return err
	}
	task, release, err := LockTask(repo, id)
	if err != nil {
		return err
	}
	defer release()
	return app.Accept(task)
}

func runStatus(app *App, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("status", flag.ContinueOnError)
	set.SetOutput(stderr)
	var repo string
	set.StringVar(&repo, "repo", ".", "target Git repository")
	id, err := parseTaskCommand(set, args, "duet status [options] <task-id>")
	if err != nil {
		return err
	}
	task, err := LoadTask(repo, id)
	if err != nil {
		return err
	}
	app.PrintStatus(task)
	return nil
}

func runList(app *App, args []string, stderr io.Writer) error {
	set := flag.NewFlagSet("list", flag.ContinueOnError)
	set.SetOutput(stderr)
	var repo string
	set.StringVar(&repo, "repo", ".", "target Git repository")
	set.Usage = func() {
		fmt.Fprintln(stderr, "Usage: duet list [options]")
		set.PrintDefaults()
	}
	if err := set.Parse(args); err != nil {
		return err
	}
	if len(set.Args()) != 0 {
		return errors.New("list does not accept positional arguments")
	}
	tasks, err := ListTasks(repo)
	if err != nil {
		return err
	}
	app.PrintList(tasks)
	return nil
}

func parseTaskCommand(set *flag.FlagSet, args []string, usage string) (string, error) {
	var leadingID string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		leadingID = args[0]
		args = args[1:]
	}
	set.Usage = func() {
		fmt.Fprintln(set.Output(), "Usage:", usage)
		set.PrintDefaults()
	}
	if err := set.Parse(args); err != nil {
		return "", err
	}
	positionals := set.Args()
	if leadingID != "" {
		if len(positionals) != 0 {
			set.Usage()
			return "", errors.New("too many task IDs")
		}
		return leadingID, nil
	}
	if len(positionals) != 1 {
		set.Usage()
		return "", errors.New("exactly one task ID is required")
	}
	return positionals[0], nil
}

func printRootHelp(out io.Writer) {
	fmt.Fprintln(out, `duet coordinates a persistent Sol planner/reviewer and Luna implementer.

Usage:
  duet <command> [options]

Commands:
  start, run   Create an isolated task and ask Sol for a plan
  approve      Approve the plan and run the bounded Luna/Sol loop
  resume       Revise a plan, reject final acceptance, or authorize another batch
  accept       Record final human acceptance and print the integration handoff
  status       Show one task and its latest evidence
  list         List tasks stored in the repository
  version      Print the version

Run "duet <command> -h" for command options.`)
}
