---
name: gateflow-task
description: Use when implementing an assigned GateFlow issue end to end, from checking that the issue is ready, through coding in an owned worktree, to a verified change ready for a PR.
---

# Implement an assigned issue

Sources: CONTRIBUTING.md [Authorization](../../../CONTRIBUTING.md#authorization),
[Roles and ownership](../../../CONTRIBUTING.md#roles-and-ownership),
[Issues](../../../CONTRIBUTING.md#issues) and
[Cross-platform](../../../CONTRIBUTING.md#cross-platform), and AGENTS.md
[Code layout](../../../AGENTS.md#code-layout).

1. Read the issue with `gh issue view <number> --json title,body,labels`, and its
   issue version and assignees with the command under
   [Delegating work](../../../CONTRIBUTING.md#delegating-work). Check that your
   assignment contains every item in Delegating work, that the issue has exactly one
   assignee and the `state:ready` label, and that the issue version matches the
   assignment. If anything is missing or different, stop and ask the owner. Draft missing issue
   content with [gateflow-issue](../gateflow-issue/SKILL.md).
2. For a controlled change, confirm that the owner's approval covers this scope and
   issue version, and that it came through a channel listed under Authorization.
   Otherwise stop.
3. Get or resume your worktree with [gateflow-worktree](../gateflow-worktree/SKILL.md).
4. Implement only the assigned scope and leave unrelated changes alone. If the scope
   must change, stop and ask; meanwhile continue only work that does not depend on the
   answer.
5. Verify with [gateflow-verify](../gateflow-verify/SKILL.md).
6. Commit and prepare the PR with [gateflow-pr](../gateflow-pr/SKILL.md).

To hand part of the work to another session, write a brief that covers every item in
Delegating work. Never pass on remote actions or coordinator duties.

Return: branch, head SHA, base branch and tip, what changed, evidence, remaining
acceptance and the next owner. Merging belongs to
[gateflow-integrate](../gateflow-integrate/SKILL.md).
