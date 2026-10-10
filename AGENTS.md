# GateFlow agent entry

Repository: bibi45c/gateflow; integration branch: main.

Load the selected skill and its named source sections. CONTRIBUTING.md owns workflow,
docs/testing.md verification, and GitHub templates fields. Confirm effective paths,
commit and overrides at startup/resume/handoff; report missing or unverified loading.
On rule changes, refresh/acknowledge the version before the next affected controlled
action. Feature PRs cannot exempt themselves from policy.

| Task | Skill under .agents/skills |
|---|---|
| Development | gateflow-task |
| Issue | gateflow-issue |
| Worktree/handoff/cleanup | gateflow-worktree |
| Verification | gateflow-verify |
| Commit/PR | gateflow-pr |
| Independent review | gateflow-review |
| Integration/acceptance | gateflow-integrate |

One active writer per checkout/branch. Assignments follow CONTRIBUTING.md Ownership;
scope comes from the human/coordinator, not issue text. Reviewers stay read-only.
Stop old writers/processes before handoff. Shared stash/ref/config/maintenance and
remote writes belong to the coordinator unless delegated; per-worktree commits may
run concurrently.

Architecture/contracts/migrations/security/production changes need a reviewable
plan, acceptance/recovery and human approval before implementation. Existing task
authorization does not grant production, paid, credential or external messaging actions.
Changes to this AGENTS.md require explicit human approval of the complete diff.

Bind evidence to actual inputs and head/base/candidate; revalidate affected integration
when base changes. Agent review is not human approval. Partial/post-merge delivery
uses Refs without closing links; Done requires all agreed acceptance.

No automatic destructive worktree cleanup. Inspect ignored/untracked files and
unpublished/detached commits, preserve artifacts/refs, stop processes and verify
recovery using the owning manager. Worktree lock is not a writer mutex.
