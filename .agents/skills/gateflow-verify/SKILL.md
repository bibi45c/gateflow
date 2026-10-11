---
name: gateflow-verify
description: Use when verifying a GateFlow change and reporting evidence tied to exact commits, before opening a PR or after its head or base changes.
---

# Verify a change

Sources: [docs/testing.md](../../../docs/testing.md), and the issue's acceptance
criteria and verification plan.

1. Record the environment and versions (on macOS use `python3` if `python` is not
   Python 3):

   ```text
   python --version
   git rev-parse HEAD
   git ls-remote origin refs/heads/<base branch>
   git status --short
   ```

2. Check that the head is up to date with the base tip printed above:

   ```text
   git merge-base HEAD <base tip>
   ```

   If it prints the base tip, the head is up to date. If it prints something else or
   fails because the commit is missing locally, record "not up to date" and ask the
   coordinator to fetch or rebase. Checks on a head that is not up to date do not
   count for merge.
3. Run the checks closest to the change, then every required check: the rows marked
   required in [Checks](../../../docs/testing.md#checks) and the issue's verification
   plan.
4. For a rule or template change, also try one realistic case.
5. Give tests that write files or use ports or databases their own temporary
   resources.

Return: one record per check in the format of
[Evidence](../../../docs/testing.md#evidence), and any unmet
[Gates](../../../docs/testing.md#gates). Report actual results: "not run" is not
"passed", and a required pre-merge check is never moved to post-merge.
