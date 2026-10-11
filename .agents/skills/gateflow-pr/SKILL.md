---
name: gateflow-pr
description: Use when writing GateFlow commits or preparing, opening or updating a pull request.
---

# Commit and prepare a PR

Sources: CONTRIBUTING.md [Commits and PRs](../../../CONTRIBUTING.md#commits-and-prs),
the [PR template](../../../.github/pull_request_template.md) and
docs/testing.md [Evidence](../../../docs/testing.md#evidence).

Commit:

1. Inspect your changes with `git status --short` and `git diff`.
2. Stage only your own files, by path: `git add <path>`. Leave other staged work
   alone.
3. Use the title format `type(scope): change`. Write `Refs #n` in the message unless
   the change meets all of the issue's acceptance.

PR:

4. Fill every heading of the PR template, and choose Delivery and Acceptance phase
   deliberately.
5. Check the issue links in the PR body and in every commit message
   (`git log --format=%B origin/main..HEAD`). Use `Closes #n` only when the PR meets
   all of the issue's acceptance before merge.
6. The evidence must match the current head. After new commits, update it and tell
   the reviewers whose findings are affected.
7. Pushing and opening a PR are remote writes. Do them only as the coordinator or
   when delegated:

   ```text
   git push -u origin <branch>
   gh pr create --draft --base main --title "type(scope): change" --body-file .local/pr.md
   ```

   For stacked work, set `--base` to the dependency branch.

This skill does not merge. Agent review is not human approval.

Return: the PR link or draft body, head and base SHA, check status and remaining
acceptance.
