---
name: gateflow-pr
description: Use when writing GateFlow commits or preparing, opening or updating a pull request.
---

# Commit and prepare a PR

Sources: CONTRIBUTING.md [Commits and PRs](../../../CONTRIBUTING.md#commits-and-prs)
and [Stacked PRs](../../../CONTRIBUTING.md#stacked-prs), the
[PR template](../../../.github/pull_request_template.md) and docs/testing.md
[Evidence](../../../docs/testing.md#evidence).

Commit, in your own worktree:

1. Inspect your changes with `git status --short` and `git diff`.
2. Stage only your own files, by path: `git add <path>`. Leave other staged work
   alone.
3. Use the title format `<type>(<scope>): <change>`. Reference the issue with
   `Refs #<number>`; only a PR into `main` that meets all of the issue's acceptance
   before merge may say `Closes #<number>`.

PR:

4. Write the body in a UTF-8 file under `.local/`, filling every heading of the PR
   template, and choose Delivery and Acceptance phase deliberately. Every PR links an
   issue; without one, stop and ask the owner to create it.
5. Check the issue links in the body and in every commit message
   (`git log --format=%B <base tip>..HEAD`).
6. The evidence must match the current head and base tip. After new commits, update
   it and tell the reviewers whose findings are affected.
7. Pushing and opening a PR are remote writes. Do them only as the coordinator, or
   when the owner delegated them to you. Run from the writer's worktree:

   ```text
   git push -u origin <branch>
   gh pr create --draft --base <base branch> --head <branch> --title "<title>" --body-file "<body file>"
   gh pr checks <number>
   ```

This skill does not merge. Agent review is not human approval.

Return: the PR link or body file, head SHA, base branch and tip, check results and
remaining acceptance.
