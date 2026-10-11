---
name: gateflow-integrate
description: Use when merging an approved GateFlow pull request into main and closing out its issue; for the integrator only.
---

# Merge and close out

Sources: CONTRIBUTING.md [Review and merge](../../../CONTRIBUTING.md#review-and-merge),
[Bootstrap exception](../../../CONTRIBUTING.md#bootstrap-exception),
[Commits and PRs](../../../CONTRIBUTING.md#commits-and-prs) and
[Stacked PRs](../../../CONTRIBUTING.md#stacked-prs), and docs/testing.md
[Gates](../../../docs/testing.md#gates).

1. Confirm that you are the integrator named under
   [Terms](../../../CONTRIBUTING.md#terms), or acting for them with their direct
   instruction.
2. Read the PR, its reviews and whether it is up to date:

   ```text
   gh pr view <number> --json isDraft,author,baseRefName,baseRefOid,headRefOid,statusCheckRollup,closingIssuesReferences,body
   gh api repos/bibi45c/gateflow/pulls/<number>/reviews --jq ".[] | {user: .user.login, state, commit_id}"
   gh api repos/bibi45c/gateflow/compare/<baseRefOid>...<headRefOid> --jq .behind_by
   ```

3. Stop unless every merge condition in Review and merge holds:
   - not a draft, and `behind_by` is `0`;
   - every required check passed, and no check failed without the owner's recorded
     reason;
   - the issue's pre-merge acceptance evidence is in the PR;
   - an `APPROVED` review from a maintainer other than the author whose `commit_id`
     equals `headRefOid`, or, under the bootstrap exception, your self-review naming
     that head.
4. Write the squash title (`<type>(<scope>): <change> (#<number>)`) and body to a
   UTF-8 file under `.local/`. The body summarizes the change and links the issue; it
   says `Closes #<n>` only when the base is `main` and all acceptance is met. Read
   both before merging.
5. Merge at the checked head:

   ```text
   gh pr merge <number> --squash --match-head-commit <headRefOid> --subject "<title>" --body-file "<body file>"
   ```

6. Read back the result and update the issue label, or close the issue as completed
   when all acceptance is recorded:

   ```text
   gh pr view <number> --json state,mergeCommit
   gh issue view <issue> --json state,labels
   ```

7. Tell the coordinators of stacked PRs to rebase and retarget them, and of other
   affected open PRs to re-verify against the new `main`.
8. Clean up only with approval, using [gateflow-worktree](../gateflow-worktree/SKILL.md).

Return: the merge commit SHA, issue state, remaining acceptance, and the branches and
worktrees kept.
