---
name: gateflow-integrate
description: Use when merging an approved GateFlow pull request and closing out its issue; for the coordinator or a maintainer only.
---

# Merge and close out

Sources: CONTRIBUTING.md [Review and merge](../../../CONTRIBUTING.md#review-and-merge)
and [Commits and PRs](../../../CONTRIBUTING.md#commits-and-prs), and docs/testing.md
[Gates](../../../docs/testing.md#gates).

1. Confirm that you are authorized to merge this PR.
2. Read its current state:

   ```text
   gh pr view <number> --json headRefOid,baseRefOid,reviewDecision,statusCheckRollup,closingIssuesReferences
   ```

3. Stop if a required check or a non-author human approval is missing, or if the base
   moved and affected checks were not re-run.
4. Check the squash title against the commit format, and make sure the body has
   closing keywords only when allowed.
5. Merge one PR at a time, at the reviewed head:

   ```text
   gh pr merge <number> --squash --match-head-commit <sha>
   ```

6. Read back the PR and issue state. An issue with post-merge acceptance moves to
   Awaiting acceptance; it becomes Done only when that acceptance is recorded.
7. Tell the owners of affected open PRs to re-verify against the new `main`.
8. Clean up only when authorized, with [gateflow-worktree](../gateflow-worktree/SKILL.md).

Return: the merge commit SHA, issue state, remaining acceptance, and the branches and
worktrees kept.
