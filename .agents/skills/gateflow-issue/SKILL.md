---
name: gateflow-issue
description: Use when drafting, splitting, checking or closing a GateFlow issue (task, bug or design), before it is published or marked Ready.
---

# Draft or check an issue

Sources: CONTRIBUTING.md [Issues](../../../CONTRIBUTING.md#issues) and the matching
form in [.github/ISSUE_TEMPLATE](../../../.github/ISSUE_TEMPLATE). The form defines
the fields.

1. Pick the form: task for a deliverable, bug for a reproduced failure, design for a
   decision a human must make.
2. Write the title as `[type][area] outcome`, and write each required form field as a
   `### <field label>` section. Keep drafts in `.local/`, which Git ignores.
3. Check the format:

   ```text
   python scripts/check_issue.py --title "[task][repo] Example outcome" --body-file .local/issue.md
   ```

   A passing format check does not make the issue Ready.
4. Check readiness against Issues. Split a broad outcome into sub-issues that can each
   be verified, and record their order.
5. Never invent an owner, approval, observation or issue number. List unknowns as
   open questions.
6. Publishing is a remote write. Hand the draft to the coordinator unless publishing
   was delegated to you:

   ```text
   gh issue create --title "[task][repo] Example outcome" --body-file .local/issue.md
   ```

To close an issue as Cancelled, Duplicate or Not planned, record the reason and any
replacement issue; that is not Done.

Return: the draft body or issue link, readiness gaps and the next action.
