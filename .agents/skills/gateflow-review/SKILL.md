---
name: gateflow-review
description: Use when independently reviewing a GateFlow pull request or commit range without editing it.
---

# Review a change

Sources: CONTRIBUTING.md [Review and merge](../../../CONTRIBUTING.md#review-and-merge)
and [Cross-platform](../../../CONTRIBUTING.md#cross-platform), and
[docs/testing.md](../../../docs/testing.md).

1. Pin the version you review and record it:

   ```text
   gh pr view <number> --json headRefOid,baseRefOid
   ```

2. Read the issue and its contracts, then the diff (`git diff <base>...<head>`),
   before reading other reviewers' conclusions.
3. Stay read-only: no edits, commits or pushes. Run tests only if assigned, in a
   separate worktree with its own temporary resources.
4. For each finding, give the location (`file:line`), trigger, observable impact and
   evidence, and label it confirmed, likely or missing proof.
5. Also check that issue links match the acceptance phase, that the change follows
   Cross-platform, and that the evidence matches the current head and base.
6. If asked to cross-check another review, re-verify the disputed evidence and keep
   genuine disagreements visible.

Return: the reviewed SHAs, findings ordered by severity, checks with their actual
results, and open questions. Do not post, approve as a human or widen the scope unless
authorized.
