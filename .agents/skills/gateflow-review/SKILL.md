---
name: gateflow-review
description: Use when independently reviewing a GateFlow pull request or commit range without editing it.
---

# Review a change

Sources: CONTRIBUTING.md [Review and merge](../../../CONTRIBUTING.md#review-and-merge),
[Authorization](../../../CONTRIBUTING.md#authorization) and
[Cross-platform](../../../CONTRIBUTING.md#cross-platform), and
[docs/testing.md](../../../docs/testing.md).

1. Pin the version you review and record the head, the base branch and its live tip:

   ```text
   gh pr view <number> --json headRefOid,baseRefName,author
   git ls-remote origin refs/heads/<baseRefName>
   ```

2. Read the issue and its contracts, then the diff, before reading other reviewers'
   conclusions:

   ```text
   gh pr diff <number>
   ```

   If `headRefOid` changes before you finish, restart on the new head or state which
   head your findings cover.
3. Stay read-only: no edits, commits or pushes. Run tests only when your assignment
   allows it, in a separate worktree with its own temporary resources.
4. For each finding, give the location (`file:line`), trigger, observable impact and
   evidence, and label it confirmed, likely or missing proof.
5. Also check that issue links match the acceptance phase and the PR's base, that the
   change follows Cross-platform, and that the evidence matches the current head and
   base tip.
6. If asked to cross-check another review, re-verify the disputed evidence and keep
   genuine disagreements visible.

Never submit an approving review or write a self-review; only a human approves, from
their own account. Post comments only when the owner authorized it, ending each with
`_Posted by an agent._`.

For an [agent review with owner confirmation](../../../CONTRIBUTING.md#agent-review-with-owner-confirmation),
first confirm that this session wrote, committed and submitted no part of the PR;
otherwise stop. The posted comment starts with `Agent review:`, names the reviewed
head SHA, lists findings with severity, and states no approval.

Return: the reviewed head and base SHAs, findings ordered by severity, checks with
their actual results, and open questions.
