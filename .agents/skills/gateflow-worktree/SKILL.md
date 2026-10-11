---
name: gateflow-worktree
description: Use when creating, resuming, handing off or removing a GateFlow worktree or task branch.
---

# Manage a worktree

Sources: CONTRIBUTING.md [Branches and worktrees](../../../CONTRIBUTING.md#branches-and-worktrees)
and [Handoff](../../../CONTRIBUTING.md#handoff).

Inspect first:

```text
git worktree list
git status --short --branch
git log -1 --oneline
```

**Create.** Creating branches and fetching are shared operations, so they belong to
the coordinator unless delegated. Use the branch name and base from the assignment:

```text
git fetch origin
git worktree add ../GateFlowWorktrees/<slug> -b <type>/<slug> origin/main
```

For stacked work, use the agreed dependency branch instead of `origin/main`, and
record it.

**Resume.** Confirm that you are the only writer and that the branch and head match
the handoff. If rule files changed on `main` since your assignment, re-read them.

**Hand off.** Follow Handoff: stop your processes and record branch, head SHA,
uncommitted changes, evidence and open questions. The next writer confirms before
editing.

**Remove.** Only when authorized, and only after every step in
[Cleanup](../../../CONTRIBUTING.md#cleanup). Never use `--force` on work you have not
inspected.

Return: branch, head and base SHA, writer, and anything kept or still recoverable. In
issues and PRs, report the branch and SHA, not local paths.
