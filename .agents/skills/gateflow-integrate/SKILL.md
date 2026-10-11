---
name: gateflow-integrate
description: Use when merging an approved GateFlow pull request into main and closing out its issue; for the integrator only.
---

# Merge and close out

Sources: CONTRIBUTING.md [Review and merge](../../../CONTRIBUTING.md#review-and-merge),
[Agent review with owner confirmation](../../../CONTRIBUTING.md#agent-review-with-owner-confirmation),
[Authorization](../../../CONTRIBUTING.md#authorization),
[Commits and PRs](../../../CONTRIBUTING.md#commits-and-prs) and
[Stacked PRs](../../../CONTRIBUTING.md#stacked-prs), and docs/testing.md
[Gates](../../../docs/testing.md#gates).

1. Confirm that you are the integrator named under
   [Terms](../../../CONTRIBUTING.md#terms), or acting for them on their direct
   instruction in this session.
2. Read the PR, whether it is up to date with the live base branch, its reviews, its
   files including the old names of renamed files, and its comments:

   ```text
   gh pr view <number> --json isDraft,author,baseRefName,headRefOid,statusCheckRollup,closingIssuesReferences,body
   gh api repos/bibi45c/gateflow/compare/<baseRefName>...<headRefOid> --jq "{behind_by, merge_base: .merge_base_commit.sha}"
   gh api --paginate repos/bibi45c/gateflow/pulls/<number>/reviews --jq ".[] | {user: .user.login, state, commit_id, submitted_at}"
   gh api --paginate repos/bibi45c/gateflow/pulls/<number>/files --jq ".[] | .filename, .previous_filename // empty"
   gh api --paginate repos/bibi45c/gateflow/issues/<number>/comments --jq ".[] | {id, user: .user.login, created_at, updated_at, html_url, body}"
   gh api repos/bibi45c/gateflow/collaborators/<login>/permission --jq .permission
   ```

   Use the branch name, not `baseRefOid`: GitHub updates `baseRefOid` only when the
   head is pushed, so it can be stale. Read the owner (the linked issue's single
   assignee) with the command under
   [Delegating work](../../../CONTRIBUTING.md#delegating-work).
3. Stop unless every merge condition in Review and merge holds:
   - the body links an issue, the PR is not a draft, and `behind_by` is `0`;
   - in `statusCheckRollup`, the latest run of each required check succeeded, and no
     latest run failed without a recorded reason;
   - the issue's pre-merge acceptance evidence is in the PR;
   - for each reviewer, take their latest review that is not `COMMENTED`; none is
     `CHANGES_REQUESTED`, whichever approval path is used;
   - approval, either:
     - at least one `APPROVED` review with `commit_id` equal to `headRefOid`, from
       someone other than the author whose permission is `write`, `maintain` or
       `admin`; or
     - an agent review with owner confirmation, only when no listed file, current or
       previous name, is a rule file (see Terms), and you judge from the diff that it
       is not a controlled change. Every `Agent review:` comment that names
       `headRefOid`, in full or by a prefix of it, is unedited (`created_at` equals
       `updated_at`), and none has a
       confirmed high or medium finding without a resolving reply from a non-author
       maintainer. The owner's `Self-review:` comment names the full `headRefOid`,
       links one of those agent reviews, is unedited, is posted by the owner, and the
       owner's permission is `write`, `maintain` or `admin`.
4. Write the squash title (`<type>(<scope>): <change> (#<number>)`) and body to a
   UTF-8 file under `.local/`. The body summarizes the change and links the issue; it
   says `Closes #<n>` only when the base is `main` and all acceptance is met. Read
   both before merging.
5. Record the head SHA, then merge at it:

   ```text
   gh pr merge <number> --squash --match-head-commit <headRefOid> --subject "<title>" --body-file "<body file>"
   ```

6. Read back the result and update the issue label, or close the issue as completed
   when all acceptance is recorded:

   ```text
   gh pr view <number> --json state,mergeCommit
   gh issue view <issue> --json state,labels
   ```

7. Give the coordinators of stacked PRs the merged PR's old head SHA so they can
   rebase, and tell the owners of other affected open PRs to re-verify against the
   new `main`.
8. Clean up only with approval, using [gateflow-worktree](../gateflow-worktree/SKILL.md).

Return: the merge commit SHA, the merged PR's old head SHA, issue state, remaining
acceptance, and the branches and worktrees kept.
