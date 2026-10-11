---
name: gateflow-worktree
description: Use when creating, resuming, handing off or removing a GateFlow worktree or task branch.
---

# Manage a worktree

Sources: CONTRIBUTING.md [Branches and worktrees](../../../CONTRIBUTING.md#branches-and-worktrees),
[Rule version](../../../CONTRIBUTING.md#rule-version) and
[Handoff](../../../CONTRIBUTING.md#handoff).

Inspect first:

```text
git worktree list
git status --short --branch
git log -1 --oneline
```

**Create.** Fetching and creating branches are shared operations, so only the
coordinator does this. Run from the main checkout's root, which is the first path
`git worktree list` prints, and use the branch name and base commit from the
assignment:

```text
git fetch origin
git worktree add --no-track -b <type>/<slug> "../GateFlowWorktrees/<slug>" <base commit>
```

**Resume.** Confirm that you are the only writer, that the branch and head match the
handoff, and that the issue's last-edited time is unchanged
(`gh issue view <number> --json updatedAt`). If the owner or coordinator reported
newer rules, re-read them first.

**Hand off.** Follow Handoff: stop your processes and record branch, head SHA,
uncommitted changes, evidence and open questions. The next writer confirms before
editing.

**Remove.** Only with approval from the owner or coordinator, and only after every
step in [Cleanup](../../../CONTRIBUTING.md#cleanup), run from a different checkout.
`git worktree prune` is a shared operation.

Return: branch, head SHA, base commit, writer, and anything kept or still
recoverable. In issues and PRs, report the branch and SHA, not local paths.
