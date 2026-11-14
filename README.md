# duet

`duet` coordinates two persistent Codex sessions around two explicit human decisions:

- **Sol** inspects the repository, prepares the plan, and reviews every implementation round.
- **Luna** implements the approved plan in an isolated Git worktree.
- **You** approve the plan and decide whether to accept the final result.

The original branch is never changed automatically. Accepted work stays in its task worktree until you integrate it using the repository's normal process.

```text
start → Sol plans → you approve → Luna implements → checks → Sol reviews
                                           ↑               │
                                           └── fix round ───┘
                                                          ↓
                                      you accept → integration handoff
```

## Requirements

- macOS or Linux
- Git with worktree support
- Go 1.23 or newer when building from source
- An installed and authenticated Codex CLI

Follow the [official Codex CLI guide](https://learn.chatgpt.com/docs/codex/cli) to install Codex. Run `codex` once and complete sign-in before using `duet`.

Confirm the prerequisites:

```sh
git --version
codex --version
go version
```

## Install duet

Install `duet` as a user-wide command in `~/.local/bin`:

```sh
make install
```

Make sure that directory is on `PATH`. Add this to `~/.zshrc` or `~/.bashrc` if needed:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Reload the shell, then check the installation:

```sh
command -v duet
duet version
duet help
```

The equivalent command without Make is:

```sh
GOBIN="$HOME/.local/bin" go install ./cmd/duet
```

To build a repository-local binary instead, use `make build` and run it as `./duet`.

## Quick start

The following example assumes `duet` was installed with `make install`. Substitute `./duet` if you are using the repository-local binary.

### 1. Start a task

Run the command inside the target Git repository. Give the task a short name and declare every command that must pass:

```sh
cd /path/to/repository

duet start \
  --name retry-client \
  --check 'go test ./...' \
  --check 'go vet ./...' \
  'Add bounded retries to the HTTP client'
```

`duet` will:

1. Verify that the repository supports Git worktrees.
2. Create the branch `duet/retry-client` in an isolated sibling worktree.
3. Start a read-only Sol session.
4. Print Sol's implementation plan.
5. Stop without implementing anything.

Review the printed plan before continuing.

### 2. Approve the plan

```sh
duet approve retry-client
```

You can attach notes for Luna without changing the approved plan:

```sh
duet approve retry-client --feedback 'Keep the existing public API compatible'
```

After approval, the automatic loop begins:

1. Luna implements in the task worktree.
2. `duet` runs all declared `--check` commands.
3. Sol reviews the worktree, complete diff, and check evidence.
4. Blocking findings and failed checks return to the same Luna session.

The loop pauses after three rejected reviews by default. It also pauses immediately when an agent or required tool fails unrecoverably.

### 3. Inspect the result

When the evidence gate passes, inspect the task:

```sh
duet status retry-client
```

The status output includes the worktree, task branch, latest diff, check logs, and Sol's findings.

You can also inspect the worktree directly using the path printed by `status`:

```sh
git -C /path/to/task-worktree status --short
git -C /path/to/task-worktree diff HEAD
```

### 4. Accept the result

```sh
duet accept retry-client
```

Before accepting, `duet` verifies that the worktree still matches the evidence Sol reviewed. Acceptance then writes and prints a handoff containing:

- the task worktree and branch;
- the base commit;
- the reviewed diff and Git status;
- every declared check and its log;
- suggested commit and integration commands.

Acceptance does **not** commit, merge, rebase, cherry-pick, push, or open a pull request.

## Running from outside the target repository

Pass `--repo` to every command when your current directory is elsewhere:

```sh
duet start \
  --repo /path/to/repository \
  --name retry-client \
  --check 'go test ./...' \
  'Add bounded retries to the HTTP client'

duet approve retry-client --repo /path/to/repository
duet status retry-client --repo /path/to/repository
duet accept retry-client --repo /path/to/repository
```

## Revising or resuming a task

`resume` handles every human-authorized return to work.

### Revise an unapproved plan

```sh
duet resume retry-client --feedback 'Do not change the exported Client interface'
```

Sol resumes its planning session, incorporates the feedback, and presents another plan for approval.

### Reject a result awaiting acceptance

```sh
duet resume retry-client --feedback 'Add coverage for cancellation during backoff'
```

This rejects final acceptance and starts another bounded Luna/Sol batch.

### Continue after a cutoff or tool failure

Inspect the evidence first:

```sh
duet status retry-client
```

Then authorize another batch:

```sh
duet resume retry-client --feedback 'The missing test tool is now installed; retry the checks'
```

Each new human-authorized batch gets the configured rejection allowance. Sol and Luna retain their separate session histories throughout the task.

### Recover after interruption

If the process receives `SIGINT` or `SIGTERM`, it records a paused state when control returns from the active subprocess. Use `status` and `resume` rather than starting another task with the same name.

## Commands

| Command | Purpose |
|---|---|
| `duet start <request>` | Create the isolated task and ask Sol for a plan. |
| `duet run <request>` | Alias for `start`. |
| `duet approve <task-id>` | Approve the plan and start the automatic implementation/review loop. |
| `duet resume <task-id>` | Revise a plan, reject final acceptance, or authorize another batch. |
| `duet accept <task-id>` | Record final acceptance and generate the integration handoff. |
| `duet status <task-id>` | Show one task and its latest evidence. |
| `duet list` | List tasks stored for the repository. |
| `duet version` | Print the installed version. |

Run `duet <command> --help` for the complete flags accepted by a command.

## Start options

| Option | Default | Meaning |
|---|---:|---|
| `--name ID` | Generated | Stable task ID and `duet/ID` branch name. |
| `--base REF` | `HEAD` | Commit or ref from which to create the task worktree. |
| `--check COMMAND` | Required | Acceptance command. Repeat the option for multiple checks. |
| `--check-timeout DURATION` | `10m` | Maximum runtime for each declared check. |
| `--max-rejections N` | `3` | Rejected Sol reviews allowed per automatic batch. |
| `--sol-model MODEL` | Codex default | Model override for Sol. |
| `--luna-model MODEL` | Codex default | Model override for Luna. |
| `--codex-bin PATH` | `codex` | Codex executable to invoke. |
| `--worktree-root PATH` | Sibling directory | Parent directory for isolated task worktrees. |
| `--repo PATH` | Current directory | Target Git repository. |

Every `--check` value is executed through `/bin/sh` inside the task worktree. Treat check strings as trusted shell commands.

## State, worktrees, and evidence

State and evidence are kept outside tracked files in the repository's common Git directory:

```text
.git/duet/tasks/<task-id>/
  state.json
  sol-plan-*.json
  sol-*.jsonl
  luna-*.jsonl
  round-*.diff
  round-*.status
  round-*-check-*.log
  acceptance-verification.*
  HANDOFF.md
```

The default task worktree is a sibling of the target repository:

```text
.<repository-name>-duet/<task-id>/
```

Failed, paused, and accepted tasks remain available for inspection. `duet` intentionally has no automatic cleanup command. Commit and integrate accepted work before removing its worktree according to your repository's retention policy.

## Safety guarantees

- Sol starts read-only and remains the independent planner/reviewer.
- Luna receives automatic workspace-write approval only inside the isolated task worktree.
- The original branch is not checked out, rewritten, or integrated automatically.
- All declared checks must pass before final acceptance is offered.
- Sol must approve with no blocking findings.
- The automatic correction loop has a configurable hard cutoff.
- Final acceptance fails if the worktree changed after Sol's reviewed evidence was captured.

## Troubleshooting

### `is not a Git worktree`

Run `duet` from a non-bare Git repository or pass the correct `--repo` path.

### `declare at least one acceptance check`

Add one or more `--check` flags to `start`. Use commands that return a nonzero exit code when acceptance should fail.

### `task branch already exists` or `task already exists`

Choose a new `--name`, or inspect the existing task with `duet status <task-id>`.

### Task paused after three reviews

Read the diff, check logs, and unresolved findings shown by `status`. Correct any environment problem, then explicitly authorize another batch with `resume`.

### Check command not found

Install the required tool so it is available on `PATH`, then resume the task. Exit code `127` is treated as an unrecoverable check-tool failure and pauses immediately.

### Worktree changed after review

`accept` refuses stale evidence. Run `resume <task-id> --feedback 'Review the latest worktree changes'` so checks and Sol review run again.

### Codex invocation failed

Verify that `codex` works directly and that sign-in is valid:

```sh
codex --version
codex
```

The task evidence directory contains the exact `*.jsonl` event stream and `*.stderr.log` for each invocation.

## Development checks

Run the project verification suite with:

```sh
go test ./...
go test -race ./...
go vet ./...
```

The tests cover the complete workflow, session resume, bounded correction loops, original-branch isolation, evidence persistence, failed checks, tool failures, stale acceptance evidence, unusual Git filenames, and the real Codex command protocol through a local protocol double.
