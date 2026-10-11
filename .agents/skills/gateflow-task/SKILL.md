---
name: gateflow-task
description: Use when implementing an assigned GateFlow issue end to end, from checking that the issue is ready, through coding in an owned worktree, to a verified change ready for a PR.
---

# Implement an assigned issue

Sources: CONTRIBUTING.md [Roles and ownership](../../../CONTRIBUTING.md#roles-and-ownership),
[Issues](../../../CONTRIBUTING.md#issues) and
[Cross-platform](../../../CONTRIBUTING.md#cross-platform).

1. Check that your assignment contains every item in
   [Delegating work](../../../CONTRIBUTING.md#delegating-work). If an item is missing
   or the issue is not Ready, stop and ask. Draft missing issue content with
   [gateflow-issue](../gateflow-issue/SKILL.md).
2. If the issue is an approved change, confirm that the recorded approval covers this
   scope and version. Otherwise stop.
3. Get or resume your worktree with [gateflow-worktree](../gateflow-worktree/SKILL.md).
4. Implement only the assigned scope and leave unrelated changes alone. If the scope
   must change, stop and ask; meanwhile continue only work that does not depend on the
   answer.
5. Verify with [gateflow-verify](../gateflow-verify/SKILL.md).
6. Commit and prepare the PR with [gateflow-pr](../gateflow-pr/SKILL.md).

To hand part of the work to someone else, write a brief that covers every item in
Delegating work.

Return: branch, head and base SHA, what changed, evidence, remaining acceptance and
the next owner. Merging belongs to [gateflow-integrate](../gateflow-integrate/SKILL.md).
