---
name: gateflow-issue
description: Use when drafting, splitting, checking or closing a GateFlow issue (task, bug or design), before it is published or marked Ready.
---

# Draft or check an issue

Sources: CONTRIBUTING.md [Issues](../../../CONTRIBUTING.md#issues) and
[Authorization](../../../CONTRIBUTING.md#authorization), and the matching form in
[.github/ISSUE_TEMPLATE](../../../.github/ISSUE_TEMPLATE). The form defines the
fields.

1. Pick the form: task for a deliverable, bug for a reproduced failure, design for a
   decision a human must make.
2. Write the title as `[<type>][<area>] <outcome>`, and write each required form field
   as a `### <field label>` section in a UTF-8 file under `.local/`. Create `.local/`
   if it does not exist. In Windows PowerShell 5.1, use an editor or
   `-Encoding utf8`, because `>` writes UTF-16.
3. Check the format (on macOS use `python3` if `python` is not Python 3):

   ```text
   python scripts/check_issue.py --title "<title>" --body-file "<draft file>"
   ```

   A passing format check does not make the issue Ready.
4. Check readiness against Issues. Split a broad outcome into sub-issues that can each
   be verified, and record their order.
5. Never invent an owner, approval, observation or issue number. List unknowns as
   open questions. A maintainer sets the assignee, and the owner adds `state:ready`.
6. Publishing is a remote write. Hand the draft to the coordinator unless the owner
   delegated publishing to you:

   ```text
   gh issue create --title "<title>" --body-file "<draft file>" --milestone "<milestone>"
   ```

   Then mark it as blocked by each issue it needs, using the blocking issue's
   numeric id (`gh api repos/bibi45c/gateflow/issues/<blocking number> --jq .id`):

   ```text
   gh api -X POST repos/bibi45c/gateflow/issues/<number>/dependencies/blocked_by -F issue_id=<id>
   ```

To close an issue as Not planned or Duplicate, record the reason and any replacement
issue; that is not Done.

Return: the draft file or issue link, readiness gaps and the next action.
