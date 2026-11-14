# Dual-Codex Orchestration Decisions

Date: 2026-08-20  
Status: Accepted through Visual pressure test

## Decision 1 — Human approval gates

Sol prepares the plan. A human approves that plan before implementation begins and makes the final acceptance decision. Sol/Luna review-and-fix rounds between those gates run unattended.

Consequence: Human authority is concentrated at consequential boundaries without requiring approval for every correction round.

## Decision 2 — Disposable worktree per task

Every task runs in an isolated Git worktree and task branch. Rejected, incomplete, or failed changes do not touch the user's branch.

Consequence: The orchestrator can discard failed attempts safely and can hand off an accepted branch with its Git history intact.

## Decision 3 — Persistent role-specific sessions

Sol and Luna each keep a separate resumable session for the life of a task. Their sessions persist across review-and-fix rounds.

Consequence: Each role retains its own context while the orchestrator preserves the planner/reviewer and implementer boundary.

## Decision 4 — Bounded evidence gate

A task is ready for final human acceptance only when all declared checks pass and Sol reports no blocking findings. The automatic loop pauses after three rejected reviews or an unrecoverable tool failure.

When pausing, the orchestrator must provide the current diff, check results, tool errors, and unresolved findings.

Consequence: Unattended work has a hard cost and oscillation boundary. A potentially fixable fourth round is deliberately escalated rather than run automatically.

## Decision 5 — Explicit integration handoff

Final acceptance preserves the accepted task worktree and reports its branch or commit, diff, check evidence, and suggested repository-specific integration commands. It does not automatically mutate the user's branch.

Consequence: Acceptance and integration remain distinct. The human integrates by the repository's merge, rebase, cherry-pick, or pull-request policy.

## Assumptions to validate

- Confirm that a target repository supports Git worktrees before starting either role; fail early when it does not.
- Treat three rejected reviews as an initial safety policy. Record real round outcomes and revisit the cutoff using observed tasks.
- Detect or document the target repository's integration policy before suggesting integration commands.

## Deferred (low impact)

Role naming, exact command spelling, progress presentation, and evidence-bundle formatting can be decided during implementation without changing the architecture.
