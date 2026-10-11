---
name: gateflow-verify
description: Use when verifying a GateFlow change and reporting evidence tied to exact commits, before opening a PR or after its head or base changes.
---

# Verify a change

Sources: [docs/testing.md](../../../docs/testing.md), and the issue's acceptance
criteria.

1. Record the environment and versions:

   ```text
   python --version
   git rev-parse HEAD
   git merge-base HEAD origin/main
   git status --short
   ```

2. Run the checks closest to the change, then every required check listed in
   [Checks](../../../docs/testing.md#checks).
3. For a rule or template change, also try one realistic case.
4. Give tests that write files or use ports or databases their own temporary
   resources.
5. Name any uncommitted inputs that the checks used.

Return: one record per check in the format of
[Evidence](../../../docs/testing.md#evidence), and any unmet
[Gates](../../../docs/testing.md#gates). Report actual results: "not run" is not
"passed", and a required pre-merge check is never moved to post-merge.
