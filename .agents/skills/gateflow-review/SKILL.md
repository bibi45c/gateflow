---
name: gateflow-review
description: Use when independently reviewing a GateFlow pull request or commit range without editing it.
---

# Review a change

Sources: CONTRIBUTING.md [Review and merge](../../../CONTRIBUTING.md#review-and-merge),
[Authorization](../../../CONTRIBUTING.md#authorization) and
[Cross-platform](../../../CONTRIBUTING.md#cross-platform), and
[docs/testing.md](../../../docs/testing.md).

1. Pin the version you review and record it:

   ```text
   gh pr view <number> --json headRefOid,baseRefName,baseRefOid,author
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

Never submit an approving review; only a human approves, from their own account. Post
comments only when the owner authorized it.

Return: the reviewed head and base SHAs, findings ordered by severity, checks with
their actual results, and open questions.
