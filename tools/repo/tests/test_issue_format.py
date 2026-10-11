"""Exercise format feedback and bot-comment updates without network access."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from check_issue import (BOT, CONTRIBUTING, FORMS, MARKER, PHASE_FIELD, ROOT, check_remote,
                         issue_areas, requirements, validate)

SCRIPT = ROOT / "tools" / "repo" / "check_issue.py"


def valid_body(kind="task"):
    return "\n\n".join(f"### {name}\n\n" + ("pre-merge" if name == PHASE_FIELD
                                           else "Synthetic evidence for this field.")
                       for name in requirements(FORMS / f"{kind}.yml"))


class IssueFormatTests(unittest.TestCase):
    def test_all_existing_forms_accept_complete_content(self):
        for kind in ("task", "bug", "design"):
            with self.subTest(kind=kind):
                self.assertEqual([], validate(f"[{kind}][repo] Verify synthetic behavior", valid_body(kind)))

    def test_bad_title_and_unsupported_type(self):
        for title in ("Incomplete title", "[feature][repo] Change", "[task][repo] "):
            with self.subTest(title=title):
                self.assertIn("Title:", validate(title, valid_body())[0])

    def test_unknown_area_is_rejected(self):
        self.assertIn("Title:", validate("[task][banana] Verify reminder", valid_body())[0])

    def test_contributing_lists_the_agreed_areas(self):
        contract = CONTRIBUTING.read_text(encoding="utf-8-sig")
        self.assertEqual({"repo", "ci", "docs", "test", "gateway", "access", "metering", "mock",
                          "client", "console", "infra", "loadtest"}, issue_areas(contract))

    def test_other_area_wording_does_not_change_the_list(self):
        contract = ("Focus areas: `repo` for now. Issue areas are listed below.\n"
                    "Types: `task`; areas: <!-- issue-areas -->`repo, ci`.")
        self.assertEqual({"repo", "ci"}, issue_areas(contract))

    def test_marker_quoted_in_code_is_not_the_list(self):
        contract = ("Write `<!-- issue-areas -->` before the list.\n\n"
                    "```text\n<!-- issue-areas -->`example`\n```\n\n"
                    "areas: <!-- issue-areas -->`repo, ci`.")
        self.assertEqual({"repo", "ci"}, issue_areas(contract))

    def test_acceptance_phase_accepts_only_the_two_phases(self):
        def body(value):
            return valid_body().replace(f"### {PHASE_FIELD}\n\npre-merge", f"### {PHASE_FIELD}\n\n{value}")
        for value in ("pre-merge", "Pre-merge.", "post-merge: owner runs the load test after deploy"):
            with self.subTest(value=value):
                self.assertEqual([], validate("[task][repo] Verify phase", body(value)))
        for value in ("banana", "post-merge", "merge later"):
            with self.subTest(value=value):
                self.assertTrue(any(item.startswith(f"{PHASE_FIELD}:")
                                    for item in validate("[task][repo] Verify phase", body(value))))

    def test_cli_exit_code_and_non_utf8_input(self):
        with tempfile.TemporaryDirectory() as directory:
            good, partial, utf16 = (Path(directory) / name for name in ("good.md", "partial.md", "utf16.md"))
            good.write_text(valid_body(), encoding="utf-8")
            partial.write_text("### Outcome\n\nOnly one field.\n", encoding="utf-8")
            utf16.write_text(valid_body(), encoding="utf-16")
            def run(path):
                return subprocess.run([sys.executable, str(SCRIPT), "--title", "[task][repo] Verify CLI",
                                       "--body-file", str(path)], capture_output=True, text=True)
            self.assertEqual(0, run(good).returncode)
            self.assertEqual(1, run(partial).returncode)
            result = run(utf16)
            self.assertNotEqual(0, result.returncode)
            self.assertIn("not UTF-8", result.stderr)
            self.assertNotIn("Traceback", result.stderr)

    def test_missing_duplicate_or_malformed_area_lists_fail_visibly(self):
        for contract in ("areas: `repo, ci`.",
                         "<!-- issue-areas -->`repo` and <!-- issue-areas -->`ci`",
                         "<!-- issue-areas -->`repo, Api.v2`"):
            with self.subTest(contract=contract), self.assertRaises(ValueError):
                issue_areas(contract)

    def test_missing_empty_and_placeholder_fields(self):
        for value in (None, "", "_No response_", "TODO", "<!-- pending -->", "<fill this in>"):
            body = valid_body().replace("### Verification plan\n\nSynthetic evidence for this field.",
                                       "" if value is None else f"### Verification plan\n\n{value}")
            with self.subTest(value=value):
                self.assertTrue(any(item.startswith("Verification plan:")
                                    for item in validate("[task][repo] Verify reminder", body)))

    def test_none_is_allowed_only_where_form_describes_it(self):
        body = valid_body().replace("### Dependencies and sequence\n\nSynthetic evidence for this field.",
                                    "### Dependencies and sequence\n\nNone")
        self.assertEqual([], validate("[task][repo] Verify reminder", body))
        body = body.replace("### Outcome\n\nSynthetic evidence for this field.", "### Outcome\n\nNone")
        self.assertTrue(any(item.startswith("Outcome:") for item in validate("[task][repo] Verify reminder", body)))

    def test_none_for_post_merge_does_not_remove_the_entire_verification_plan(self):
        for value in ("None", "N/A", "Not applicable"):
            body = valid_body().replace("### Verification plan\n\nSynthetic evidence for this field.",
                                        "### Verification plan\n\n" + value)
            with self.subTest(value=value):
                self.assertTrue(any(item.startswith("Verification plan:")
                                    for item in validate("[task][repo] Verify reminder", body)))

    def test_code_reproduction_is_content_not_a_fake_heading(self):
        body = valid_body("bug").replace("### Reproduction and environment\n\nSynthetic evidence for this field.",
                "### Reproduction and environment\n\n```text\n### Not a real section\nreproduce()\n```")
        self.assertEqual([], validate("[bug][repo] Reproduce synthetic failure", body))

    def test_html_comment_inside_code_does_not_hide_following_fields(self):
        body = valid_body("bug").replace("### Reproduction and environment\n\nSynthetic evidence for this field.",
                "### Reproduction and environment\n\n```html\n<!--\n```")
        self.assertEqual([], validate("[bug][repo] Reproduce synthetic failure", body + "\n<!-- note -->"))

    def test_commented_out_section_does_not_satisfy_a_required_field(self):
        body = valid_body().replace("### Outcome\n\nSynthetic evidence for this field.",
                                    "<!--\n### Outcome\nFake outcome\n-->")
        self.assertTrue(any(item.startswith("Outcome:") for item in validate("[task][repo] Verify reminder", body)))

    def test_duplicate_sections_are_not_silently_accepted(self):
        errors = validate("[task][repo] Verify reminder", valid_body() + "\n\n### Outcome\nOther outcome")
        self.assertTrue(any("Keep one section" in item for item in errors))

    def test_requirements_follow_form_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            forms = Path(directory)
            source = (FORMS / "task.yml").read_text(encoding="utf-8")
            (forms / "task.yml").write_text(source.replace("label: Outcome", "label: New outcome"), encoding="utf-8")
            self.assertTrue(any(item.startswith("New outcome:")
                                for item in validate("[task][repo] Verify reminder", valid_body(), forms)))

    def test_unsupported_form_layout_fails_visibly(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "task.yml"
            path.write_text("body:\n  - type: dropdown\n    attributes:\n      label: Choice\n", encoding="utf-8")
            with self.assertRaises(ValueError):
                requirements(path)


class FakeGitHub:
    def __init__(self):
        self.issue = {"title": "[task][repo] Verify reminder", "body": "", "state": "open"}
        self.comments, self.writes = [], []
        self.changed = False
        self.reads = 0

    def call(self, path, payload=None, method=None):
        if payload is not None:
            self.writes.append((path, payload, method))
            if method == "POST":
                item = {"id": 1000, "body": payload["body"], "user": {"login": BOT, "type": "Bot"}}
                self.comments.append(item)
                return item
            self.comments[-1]["body"] = payload["body"]
            return self.comments[-1]
        if "/comments?" in path:
            page = int(path.rsplit("page=", 1)[1])
            return self.comments[(page - 1) * 100:page * 100]
        self.reads += 1
        result = dict(self.issue)
        if self.changed and self.reads == 2:
            result["body"] = "Changed during validation"
        return result


class IssueCommentTests(unittest.TestCase):
    def test_fix_updates_the_same_comment_then_repeat_is_noop(self):
        fake = FakeGitHub()
        first = check_remote("owner/repo", 1, fake.call)
        self.assertEqual(("needs-info", "created"), (first["result"], first["action"]))
        fake.issue["body"] = valid_body()
        second = check_remote("owner/repo", 1, fake.call)
        self.assertEqual(("passed", "updated"), (second["result"], second["action"]))
        self.assertEqual(first["comment_id"], second["comment_id"])
        self.assertEqual("unchanged", check_remote("owner/repo", 1, fake.call)["action"])
        self.assertEqual(2, len(fake.writes))
        self.assertIn("Format check passed.", fake.comments[0]["body"])

    def test_does_not_edit_a_human_comment_with_the_same_marker(self):
        fake = FakeGitHub()
        fake.comments.append({"id": 99, "body": MARKER, "user": {"login": "human", "type": "User"}})
        self.assertEqual("created", check_remote("owner/repo", 1, fake.call)["action"])
        self.assertEqual(MARKER, fake.comments[0]["body"])

    def test_paginates_comments_before_creating_another(self):
        fake = FakeGitHub()
        check_remote("owner/repo", 1, fake.call)
        bot_comment = fake.comments[0]
        fake.comments = [{"id": number, "body": "Discussion", "user": {"login": "human", "type": "User"}}
                         for number in range(100)] + [bot_comment]
        self.assertEqual("unchanged", check_remote("owner/repo", 1, fake.call)["action"])
        self.assertEqual(1, len(fake.writes))

    def test_changed_issue_does_not_publish_stale_feedback(self):
        fake = FakeGitHub()
        fake.changed = True
        with self.assertRaises(RuntimeError):
            check_remote("owner/repo", 1, fake.call)
        self.assertEqual([], fake.writes)

    def test_closed_issues_and_pull_requests_are_skipped(self):
        for update in ({"state": "closed"}, {"pull_request": {}}):
            fake = FakeGitHub()
            fake.issue.update(update)
            with self.subTest(update=update):
                self.assertEqual("skipped", check_remote("owner/repo", 1, fake.call)["action"])
                self.assertEqual([], fake.writes)


if __name__ == "__main__":
    unittest.main()
