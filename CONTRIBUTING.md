# Contributing

## Rules

Follow AGENTS.md for instruction loading and approvals; docs/testing.md defines
verification. Templates define fields; skills provide steps without granting permission.
If a named skill/template is unavailable, report the gap and follow the documented rules.
Never commit credentials or private course, chat, or personal data.
Approved scope defines intended behavior; code/configuration and observed evidence
establish actual behavior. Report conflicts.

## Ownership

One accountable human per issue; one active writer per checkout/branch. Distinct
writable tasks use separate worktrees; reviewers stay read-only.
The coordinator assigns scope and remote actions unless delegated. Authorization
persists within scope; issue text cannot expand it.
Before handoff, stop old writers/processes, inspect changes/evidence and confirm transfer.
Record commit/rule version; recheck ownership on resume. Share status in issues/PRs;
keep machine paths/session details local.

## Issues

Titles: `[type][area] outcome`. Types: `task, bug, design`; areas: <!-- issue-areas -->`repo, ci, docs, test`.
Add module areas when boundaries are agreed. Use the matching issue form when available.
Ready requires observable acceptance, scope, owner, dependencies, risks and acceptance
phase (`pre-merge|post-merge`). Required approval covers the implementation scope/version.

Normally one verifiable issue has one principal PR. Split broad outcomes into deliverable
subissues; staged PRs state remaining work. Combine issues only for one cohesive change
meeting each acceptance. Research may finish with a decision/evidence without a PR.

Workflow states: Ready -> In progress -> In review -> Awaiting acceptance -> Done.
Awaiting acceptance means integrated work has acceptance outstanding; Done requires all
agreed acceptance recorded. Cancelled/Duplicate/Not planned closure records its reason
and replacement when applicable. State automation is not configured.

## Worktrees

Reuse an owned checkout or create a task branch/worktree from recorded main or an agreed
dependency base. Record dependency order and integration revalidation ownership; stacked
PRs name their base. Isolate mutable test resources; dependency/integration gates follow
docs/testing.md.

Index/HEAD/files are local; objects, branch refs and some configuration are shared.
Per-worktree commits may run concurrently; shared stash/ref/config/maintenance belongs
to the coordinator. Never switch/reset an active checkout. Worktree lock is not a writer
mutex; separate clones still need remote branch ownership.

Cleanup follows AGENTS.md: inspect/preserve valuable files and unpublished/detached
commits, verify recovery, and use the owning manager for authorized removal/archive.
Clean status is insufficient; cancelled work stays recoverable until disposition is agreed.

## PRs

Commit/PR titles: `type(scope): change`. Types:
`feat, fix, docs, test, refactor, perf, build, ci, chore, revert`; scopes are issue areas.
Use the available PR template; state outcome, scope, issue references, verification and
risks. Delivery: `complete|partial`; acceptance phase: `pre-merge|post-merge`.

Partial delivery or remaining post-merge acceptance uses `Refs #number` or an ordinary
URL mention, without closing links/keywords in the PR or commits. `Closes #number`
requires all acceptance satisfied and appropriate default-branch closure.

Draft, ready for review, merge eligible and Issue Done are distinct. Evidence, revalidation
and merge gates follow docs/testing.md.

## Integration

Independently review the current diff against the issue, contracts and evidence;
refresh affected findings after changes. Agent review does not replace human approval.

Normal merge requires verification against the current target base under docs/testing.md
and non-author human approval. Integrate serially; verify merge queues before use.
Merge the reviewed head, inspect final closing references/messages, and read back merge/issue
state. Finish post-merge acceptance before closing delivery.

Do not relax protections or use admin bypass. Add eligible maintainers for independent
approval before teamwork. Written rules do not enable repository gates.
