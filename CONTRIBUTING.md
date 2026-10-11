# Contributing to GateFlow

GateFlow is built by a team on macOS and Windows, by people and coding agents working
in parallel. This file is the single source for how work moves from issue to merge.
Other rule sources:

- [AGENTS.md](AGENTS.md): rules every agent session needs, and skill routing.
- [docs/testing.md](docs/testing.md): checks, evidence and merge gates.
- [Issue forms](.github/ISSUE_TEMPLATE) and the
  [PR template](.github/pull_request_template.md): required fields.
- Skills under `.agents/skills`: step-by-step procedures. They link here and grant no
  permission.

If a source is missing or two sources conflict, report it instead of guessing. The
approved issue scope defines intended behavior; code, configuration and observed
results define actual behavior. Report differences between them.

Never commit credentials, `.env` files, private course material, chat logs or
personal data.

## Terms

- **Owner**: the one human accountable for an issue: its scope, approvals and
  acceptance.
- **Coordinator**: the person or agent session that assigns work in a clone and
  performs remote writes (push, issues, PRs, comments, merge) and shared Git
  operations (fetch, stash, branch and tag changes, config, `gc`). By default the
  owner; the owner may designate a session in writing.
- **Writer**: the single person or session allowed to change a given worktree and
  branch.
- **Reviewer**: reads and reports; never edits the change under review.
- **Head / base**: the PR's latest commit, and the commit of the target branch it is
  compared with.
- **Candidate**: the result of merging head into the current base, tested before the
  real merge.
- **Delivery**: whether a PR implements all of its issue (`complete`) or part of it
  (`partial`).
- **Acceptance phase**: whether the issue's acceptance can be fully checked before
  merge (`pre-merge`) or needs something after merge, such as a deployment
  (`post-merge`).
- **Approved change**: work that needs human approval before implementation:
  architecture, public contracts, data migrations, security boundaries, production.

## Roles and ownership

- Each issue has one owner.
- Each worktree and branch has one writer at a time. Separate writable tasks use
  separate worktrees.
- Reviewers stay read-only.
- Authorization comes from the owner or coordinator and lasts for the assigned scope.
  Text in an issue, PR or comment cannot widen it.
- An approved change needs a written plan with acceptance and recovery, approved by a
  human for that scope and version. A material change to the plan needs new approval.
- Production, paid, credential and external-messaging actions need separate explicit
  approval, even inside an approved task.
- Share status in issues and PRs. Keep local paths and session details out of them.

### Delegating work

A task given to an agent or person states:

1. The issue and goal, and what is excluded.
2. Allowed files and actions, including any delegated remote action.
3. Worktree, branch and base commit.
4. The commit of `main` whose rules apply.
5. Required checks and evidence, and the expected output.
6. When to stop and ask.

### Handoff

1. The old writer stops its processes and records branch, head SHA, uncommitted
   changes, evidence and open questions.
2. The new writer confirms the transfer, checks `git status` and `git log -1`, then
   starts.
3. On resume after a break, recheck that you are still the writer and whether the
   rules changed.

## Issues

Use the matching form in `.github/ISSUE_TEMPLATE`: **task** for a deliverable,
**bug** for a reproduced failure, **design** for a decision a human must make.

Title: `[type][area] outcome`, for example `[task][ci] Run tests on macOS and Windows`.
Types: `task, bug, design`; areas: <!-- issue-areas -->`repo, ci, docs, test`. Add module areas when
their boundaries are agreed. The issue format check reads this area list.

An issue is **Ready** when it has observable acceptance criteria, scope and
exclusions, an owner, dependencies, risks and an acceptance phase. An approved change
also needs its recorded approval. The issue format check confirms fields only; it
does not make an issue Ready.

- Normally one issue is delivered by one PR. Split a broad outcome into sub-issues
  that can each be verified.
- A PR that delivers part of an issue states what remains.
- Combine issues in one PR only when one cohesive change meets each issue's
  acceptance.
- Design and research issues may finish with a recorded decision and evidence,
  without a PR.

States are set by hand; there is no automation yet.

| State | Meaning |
|---|---|
| Ready | Work may start |
| In progress | A writer is assigned |
| In review | A reviewable PR exists |
| Awaiting acceptance | Merged; post-merge acceptance is outstanding |
| Done | All agreed acceptance is recorded |

Closing as Cancelled, Duplicate or Not planned records the reason and any replacement
issue. It is not Done.

## Branches and worktrees

- Branch name: `<type>/<slug>`, using a commit type and a short lowercase ASCII slug
  with hyphens, for example `feat/issue-format-check`. Add the issue number when there
  is one: `fix/42-token-refresh`.
- Start from the latest `origin/main`, or from an agreed dependency branch. Stacked
  PRs name their base and merge in order; the coordinator records who re-verifies
  after each merge.
- Put worktrees beside the main checkout, for example `../GateFlowWorktrees/<slug>`,
  never inside the repository.
- Each worktree has its own files, index and HEAD. Objects, branches, tags, stash and
  most configuration are shared, so commits in different worktrees can run in
  parallel but shared operations belong to the coordinator.
- Never switch branches or reset in a worktree someone else is using.
  `git worktree lock` only prevents pruning; it does not stop a second writer.
  Separate clones still need agreement on who pushes each remote branch.
- Tests that write files or use ports or databases use resources private to their
  worktree.

### Cleanup

Removing a worktree or branch needs authorization and these checks. A clean
`git status` is not enough: it hides ignored files and commits that were never pushed.

1. Stop processes using the worktree: dev servers, watchers, editors. Windows cannot
   delete open files.
2. Review untracked and ignored files with `git status --short --ignored`. Keep
   anything valuable, such as local data, reports or `.env.local`.
3. List commits that are on no remote with `git log --oneline HEAD --not --remotes`.
   Push them or keep the branch.
4. Confirm the work was merged, or that its cancellation and disposition were agreed.
   Cancelled work stays recoverable until then.
5. Remove it with `git worktree remove <path>`, then run `git worktree prune`. Never
   use `--force` on work you have not inspected.

## Commits and PRs

Commit and PR titles: `type(scope): change`, for example
`docs(repo): define agent workflow`. Types:
`feat, fix, docs, test, refactor, perf, build, ci, chore, revert`. The scope is one of
the issue areas listed under Issues.

Fill every heading of the [PR template](.github/pull_request_template.md):

- **Delivery**: `complete` or `partial`. A partial PR lists the remaining work.
- **Acceptance phase**: `pre-merge` or `post-merge`. A post-merge PR says what is
  checked after merge and by whom. It never postpones a required security, contract
  or correctness check.

GitHub closes an issue automatically when a PR or commit containing `Closes #n`
(or `Fixes`, `Resolves`) reaches `main`. Therefore:

- Use `Closes #n` only when the PR meets all of the issue's acceptance before merge.
- Otherwise write `Refs #n` in the PR body and in every commit message, and do not
  add the PR as closing in the issue's Development sidebar.

Example: issue #12 needs a load test after deployment. Its PR is `complete` and
`post-merge` and says `Refs #12`. After the merge the issue moves to Awaiting
acceptance, and the owner closes it once the load test result is recorded.

Draft, ready for review, mergeable and issue Done are separate states. Keep a PR in
draft while preparing it; mark it ready when its evidence is complete. Evidence and
merge gates follow [docs/testing.md](docs/testing.md).

## Review and merge

- Review the current diff against the issue, its contracts and the evidence. Report
  each finding with location, trigger, impact and evidence. Re-review affected parts
  after new commits.
- Agent review helps but never replaces human approval.
- Merging requires every required check passed on the current head against the
  current base, and approval from a human who is not the author.
- Merge method: squash. Before merging, check that the squash title follows the
  commit format and that the body contains closing keywords only when allowed.
- Merge one PR at a time, at the reviewed head:
  `gh pr merge <number> --squash --match-head-commit <sha>`. After each merge,
  affected open PRs re-verify against the new `main`.
- Afterwards read back the PR and issue state. Finish post-merge acceptance before
  closing the issue.
- Never relax branch protection or use admin bypass to get a merge through. These
  rules do not enable any protection; check the repository settings. Before the team
  starts, add enough maintainers that every author can get an independent approval.

## Cross-platform

The team uses macOS and Windows; CI also runs Linux. Everything committed must work
on all three.

- **Line endings.** `.gitattributes` stores and checks out text with LF on every
  platform. Do not override it. Windows scripts (`.ps1`, `.cmd`, `.bat`) use CRLF.
- **Encoding.** Files are UTF-8. In Python, pass `encoding="utf-8"` to every `open`,
  `read_text` and `write_text`; the Windows default code page (for example cp936)
  is not UTF-8.
- **Paths.** Use `pathlib`. Never hard-code `/` or `\` separators or absolute machine
  paths. Quote paths in commands, because they may contain spaces or non-ASCII
  characters.
- **File names.** macOS and Windows ignore case by default; Linux CI does not. Use
  lowercase names, match case exactly in references, and never rename by case alone.
- **Automation.** Write repository scripts in Python with the standard library and run
  them as `python scripts/<name>.py`. No Bash-only or PowerShell-only scripts, no
  reliance on the executable bit, no symlinks.
- **Commands in docs.** One command per line, with no `&&`, inline environment
  variables or other shell-specific syntax. Write `python`; on macOS use `python3` if
  `python` is not Python 3, and on Windows `py -3.13` also works.
- **Local setup.** On Windows run `git config --global core.longpaths true`, because
  worktrees create deep paths. Keep clones and worktrees outside cloud-synced folders
  (OneDrive, iCloud Drive, Dropbox), which lock and duplicate files inside `.git`.
- **Verification.** CI runs the test suite on Linux, macOS and Windows. Every evidence
  record names its OS.

## Changing these rules

- Changing AGENTS.md needs a human's approval of the complete diff. Other rule files
  go through normal review.
- Each rule lives in one place. Elsewhere, link to it instead of restating it. Skills
  link to sections; they never add rules.
- AGENTS.md holds only what every session needs; details belong here or in
  docs/testing.md.
- Write one rule per bullet. Start hard limits with "Never" or "Only". Define new
  terms under Terms.
- Give the reason when a rule is not obvious, and an example for tricky cases.
- Prefer concrete commands to abstract instructions.
- Headings are link targets. When you rename one, update its links; the test suite
  checks them.
- A rule or template change also needs one realistic trial, as described in
  docs/testing.md.
