"""Exercise format feedback and bot-comment updates without network access."""
import copy
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import check_issue
from check_issue import (BOT, FORMS, MARKER, PHASE_FIELD, ROOT, SUPERSEDED, changes_areas, check_all,
                         check_remote, comment_body, issue_areas, requirements)

SCRIPT = ROOT / "tools" / "repo" / "check_issue.py"
AREAS = {"repo", "ci", "docs"}


def validate(title, body, forms=FORMS):
    return check_issue.validate(title, body, AREAS, forms)


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

    def test_unknown_area_is_rejected_and_the_labels_are_named(self):
        errors = validate("[task][banana] Verify reminder", valid_body())
        self.assertEqual(["Title: Use an area that has an area: label: ci, docs, repo."], errors)

    def test_areas_come_only_from_well_formed_area_labels(self):
        labels = ["area:repo", "area:ci", "bug", "state:ready", "area:Bad.Name", "Area:docs", "area:"]
        self.assertEqual({"repo", "ci"}, issue_areas(labels))

    def test_no_area_label_fails_visibly(self):
        for labels in ([], ["bug", "state:ready"], ["area:Bad"]):
            with self.subTest(labels=labels), self.assertRaises(ValueError):
                issue_areas(labels)

    def test_only_area_label_changes_start_a_recheck(self):
        self.assertTrue(changes_areas({"label": {"name": "area:infra"}}))
        self.assertTrue(changes_areas({"label": {"name": "infra"}, "changes": {"name": {"from": "area:infra"}}}))
        self.assertFalse(changes_areas({"label": {"name": "state:ready"}}))
        self.assertFalse(changes_areas({"label": {"name": "bug"}, "changes": {"color": {"from": "ffffff"}}}))

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
                                       "--body-file", str(path), "--areas", "repo, ci"],
                                      capture_output=True, text=True)
            self.assertEqual(0, run(good).returncode)
            self.assertEqual(1, run(partial).returncode)
            result = run(utf16)
            self.assertNotEqual(0, result.returncode)
            self.assertIn("not UTF-8", result.stderr)
            self.assertNotIn("Traceback", result.stderr)

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
        self.labels = [{"name": "area:repo"}, {"name": "bug"}]
        self.open_issues = [{"number": 1}]
        self.changed = False
        self.reads = self.label_reads = 0

    def call(self, path, payload=None, method=None):
        if "/labels?" in path:
            self.label_reads += 1
            return self.labels if path.endswith("page=1") else []
        if "/issues?state=open" in path:
            return self.open_issues if path.endswith("page=1") else []
        if payload is not None:
            self.writes.append((path, payload, method))
            if method == "POST":
                item = {"id": 1000, "body": payload["body"], "user": {"login": BOT, "type": "Bot"}}
                self.comments.append(item)
                return item
            item = next(item for item in self.comments if path.endswith(f"/comments/{item['id']}"))
            item["body"] = payload["body"]
            return item
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

    def test_deleted_area_label_fails_issues_that_use_it(self):
        fake = FakeGitHub()
        fake.issue["body"] = valid_body()
        self.assertEqual("passed", check_remote("owner/repo", 1, fake.call)["result"])
        fake.labels = [{"name": "area:ci"}]
        self.assertEqual("needs-info", check_remote("owner/repo", 1, fake.call)["result"])
        self.assertIn("area: label: ci.", fake.comments[0]["body"])

    def test_recheck_covers_open_issues_and_reads_labels_once(self):
        fake = FakeGitHub()
        fake.issue["body"] = valid_body()
        fake.open_issues = [{"number": 1}, {"number": 2, "pull_request": {}}, {"number": 3}]
        results, failed = check_all("owner/repo", fake.call)
        self.assertEqual(([1, 3], 0), ([item["issue"] for item in results], failed))
        self.assertEqual(1, fake.label_reads)

    def test_recheck_continues_after_an_issue_changes(self):
        fake = FakeGitHub()
        fake.changed = True
        fake.open_issues = [{"number": 1}, {"number": 2}]
        results, failed = check_all("owner/repo", fake.call)
        self.assertEqual((["failed", "created"], 1), ([item["action"] for item in results], failed))

    def test_closed_issues_and_pull_requests_are_skipped(self):
        for update in ({"state": "closed"}, {"pull_request": {}}):
            fake = FakeGitHub()
            fake.issue.update(update)
            with self.subTest(update=update):
                self.assertEqual("skipped", check_remote("owner/repo", 1, fake.call)["action"])
                self.assertEqual([], fake.writes)


REPO = "owner/repo"


class StatefulGitHub:
    """Thread-safe offline API: several issues, per-issue comments and increasing comment ids.

    `before` runs ahead of serving each call, so a test can start another run, edit the
    issue or wait on a barrier at an exact point of a run.
    """

    def __init__(self, issues, labels=("area:repo", "area:ci", "bug", "docs", "area:Bad")):
        self.issues = {number: dict(issue) for number, issue in issues.items()}
        self.comments = {number: [] for number in issues}
        self.labels = [{"name": name} for name in labels]
        self.next_id, self.writes, self.before = 1000, [], None
        self.lock = threading.Lock()

    def add_comment(self, number, body, login=BOT, kind="Bot"):
        with self.lock:
            item = {"id": self.next_id, "body": body, "user": {"login": login, "type": kind},
                    "html_url": f"https://github.com/{REPO}/issues/{number}#issuecomment-{self.next_id}"}
            self.next_id += 1
            self.comments[number].append(item)
            return copy.deepcopy(item)

    def call(self, path, payload=None, method=None):
        if self.before:
            self.before(path, payload, method)
        route, _, query = path[len(f"repos/{REPO}/"):].partition("?")
        first_page = query.endswith("page=1")
        if route == "labels":
            return copy.deepcopy(self.labels) if first_page else []
        if route == "issues":
            return [{"number": number, **({"pull_request": {}} if "pull_request" in issue else {})}
                    for number, issue in self.issues.items() if issue["state"] == "open"] if first_page else []
        if payload is not None:
            self.writes.append((path, payload, method))
        match = re.fullmatch(r"issues/comments/(\d+)", route)
        if match and method == "PATCH":
            with self.lock:
                item = next(item for items in self.comments.values() for item in items
                            if item["id"] == int(match[1]))
                item["body"] = payload["body"]
                return copy.deepcopy(item)
        match = re.fullmatch(r"issues/(\d+)/comments", route)
        if match and method == "POST":
            return self.add_comment(int(match[1]), payload["body"])
        if match:
            page = int(query.rsplit("page=", 1)[1])
            with self.lock:
                return copy.deepcopy(self.comments[int(match[1])][(page - 1) * 100:page * 100])
        match = re.fullmatch(r"issues/(\d+)", route)
        if match and payload is None:
            with self.lock:
                return copy.deepcopy(self.issues[int(match[1])])
        raise AssertionError(f"Unexpected call: {method} {path}")

    def canonical(self, number):
        return [item for item in self.comments[number] if item["user"] == {"login": BOT, "type": "Bot"}
                and item["body"].startswith(MARKER)]


def issue_read(number, nth):
    """Match the nth read of one issue: read 1 is the snapshot, read 2 the version recheck."""
    seen = [0]
    def matches(path, method):
        if path == f"repos/{REPO}/issues/{number}" and method is None:
            seen[0] += 1
            return seen[0] == nth
        return False
    return matches


def run_inside(fake, trigger, *runs):
    """Serve the first call matching trigger only after the given runs have completed."""
    def before(path, payload, method):
        if fake.before is before and trigger(path, method):
            fake.before = None
            for run in runs:
                run()
    fake.before = before


class ReminderOverlapTests(unittest.TestCase):
    """Full and single-issue checks overlap because they use different concurrency groups."""

    def assert_one_reminder(self, fake, number, body):
        canonical = fake.canonical(number)
        self.assertEqual(1, len(canonical), fake.comments[number])
        self.assertEqual(body, canonical[0]["body"])
        ours = [item for item in fake.comments[number] if item["user"] == {"login": BOT, "type": "Bot"}]
        self.assertEqual(min(item["id"] for item in ours), canonical[0]["id"])
        for item in ours[1:]:
            self.assertTrue(item["body"].startswith(SUPERSEDED), item)
            self.assertIn(canonical[0]["html_url"], item["body"])
            self.assertNotIn("Please complete", item["body"])
            self.assertNotIn("Format check passed", item["body"])
        return canonical[0]

    def test_two_first_readers_leave_one_canonical_reminder(self):
        fake = StatefulGitHub({1: {"title": "[task][repo] Verify reminder", "body": "", "state": "open"}})
        human = fake.add_comment(1, "Discussion", "human", "User")
        other = []
        # The second run reads, sees no reminder and creates one while the first run is
        # between its own read of the comments and its create.
        run_inside(fake, issue_read(1, 2), lambda: other.append(check_remote(REPO, 1, fake.call)))
        first = check_remote(REPO, 1, fake.call)
        expected = comment_body(fake.issues[1], check_issue.validate(fake.issues[1]["title"], "", {"repo", "ci"}))
        canonical = self.assert_one_reminder(fake, 1, expected)
        self.assertEqual(("created", canonical["id"]), (other[0]["action"], other[0]["comment_id"]))
        self.assertEqual((canonical["id"], [canonical["id"] + 1]), (first["comment_id"], first["superseded"]))
        self.assertEqual(human, fake.comments[1][0])
        self.assertEqual("unchanged", check_remote(REPO, 1, fake.call)["action"])

    def test_threads_reading_together_leave_one_canonical_reminder(self):
        fake = StatefulGitHub({1: {"title": "[task][repo] Verify reminder", "body": valid_body(), "state": "open"}})
        barrier, local = threading.Barrier(2, timeout=10), threading.local()
        def before(path, payload, method):
            kind = ("issue" if path == f"repos/{REPO}/issues/1" else
                    "comments" if "/issues/1/comments?" in path else None)
            if kind:
                count = getattr(local, kind, 0) + 1
                setattr(local, kind, count)
                # Both runs read the comments before either creates, then both create before
                # either recounts.
                if count == 2:
                    barrier.wait()
        fake.before = before
        results, errors = [], []
        def run():
            try:
                results.append(check_remote(REPO, 1, fake.call))
            except Exception as error:  # surfaced by the assertion below
                errors.append(error)
        threads = [threading.Thread(target=run) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(30)
        self.assertEqual([], errors)
        self.assertEqual(2, len([item for item in fake.writes if item[2] == "POST"]))
        canonical = self.assert_one_reminder(fake, 1, comment_body(fake.issues[1], []))
        self.assertEqual({canonical["id"]}, {item["comment_id"] for item in results})

    def test_existing_reminder_overlap_updates_it_without_creating_another(self):
        fake = StatefulGitHub({1: {"title": "[task][repo] Verify reminder", "body": valid_body(), "state": "open"}})
        stale = fake.add_comment(1, MARKER + "\nPlease complete:\n- Outcome: Required section is missing.")
        other = []
        run_inside(fake, issue_read(1, 2), lambda: other.append(check_remote(REPO, 1, fake.call)))
        first = check_remote(REPO, 1, fake.call)
        self.assert_one_reminder(fake, 1, comment_body(fake.issues[1], []))
        self.assertEqual([stale["id"]], [item["id"] for item in fake.comments[1]])
        self.assertEqual(["updated", "updated"], [other[0]["action"], first["action"]])
        self.assertFalse([item for item in fake.writes if item[2] == "POST"])

    def test_recheck_after_a_content_change_supersedes_existing_duplicates(self):
        fake = StatefulGitHub({1: {"title": "[task][repo] Verify reminder", "body": "", "state": "open"}})
        old = comment_body(fake.issues[1], ["Outcome: Required section is missing."])
        oldest = fake.add_comment(1, old)
        spoof = fake.add_comment(1, MARKER + "\nFormat check passed.", "human", "User")
        duplicate = fake.add_comment(1, old)
        fake.issues[1]["body"] = valid_body()
        result = check_remote(REPO, 1, fake.call)
        self.assertEqual(("passed", "updated", oldest["id"], [duplicate["id"]]),
                         (result["result"], result["action"], result["comment_id"], result["superseded"]))
        self.assert_one_reminder(fake, 1, comment_body(fake.issues[1], []))
        self.assertEqual(spoof, fake.comments[1][1])
        writes = len(fake.writes)
        again = check_remote(REPO, 1, fake.call)
        self.assertEqual(("unchanged", oldest["id"], []), (again["action"], again["comment_id"], again["superseded"]))
        self.assertEqual(writes, len(fake.writes))

    def test_spoofed_reminders_are_neither_canonical_nor_edited(self):
        fake = StatefulGitHub({1: {"title": "[task][repo] Verify reminder", "body": valid_body(), "state": "open"}})
        spoofs = [fake.add_comment(1, MARKER + "\nFormat check passed.", "human", "User"),
                  fake.add_comment(1, MARKER + "\nFormat check passed.", BOT, "User")]
        other = []
        run_inside(fake, issue_read(1, 2), lambda: other.append(check_remote(REPO, 1, fake.call)))
        result = check_remote(REPO, 1, fake.call)
        canonical = self.assert_one_reminder(fake, 1, comment_body(fake.issues[1], []))
        self.assertNotIn(canonical["id"], [item["id"] for item in spoofs])
        self.assertEqual(spoofs, fake.comments[1][:2])
        spoof_paths = tuple(f"/comments/{item['id']}" for item in spoofs)
        self.assertEqual([], [path for path, _, _ in fake.writes if path.endswith(spoof_paths)])
        self.assertEqual([canonical["id"]] * 2, [other[0]["comment_id"], result["comment_id"]])

    def test_full_and_single_checks_overlap_across_issues(self):
        fake = StatefulGitHub({
            1: {"title": "[task][ci] Verify reminder", "body": valid_body(), "state": "open"},
            2: {"title": "[task][docs] Verify reminder", "body": valid_body(), "state": "open"},
            3: {"title": "Pull request", "body": "", "state": "open", "pull_request": {}},
            4: {"title": "[task][repo] Verify reminder", "body": "", "state": "open"},
        })
        singles = []
        # Single-issue runs for issues 1 and 2, queued close together, finish while the full
        # run is between reading issue 1's comments and creating its reminder.
        run_inside(fake, issue_read(1, 2), *(lambda number=number: singles.append(
            check_remote(REPO, number, fake.call)) for number in (1, 2)))
        results, failed = check_all(REPO, fake.call)
        self.assertEqual(([1, 2, 4], 0), ([item["issue"] for item in results], failed))
        self.assertEqual([("passed", "created"), ("needs-info", "created")],
                         [(item["result"], item["action"]) for item in singles])
        self.assertEqual(["unchanged", "unchanged", "created"], [item["action"] for item in results])
        first = self.assert_one_reminder(fake, 1, comment_body(fake.issues[1], []))
        second = self.assert_one_reminder(fake, 2, comment_body(fake.issues[2], [
            "Title: Use an area that has an area: label: ci, repo."]))
        self.assertEqual(1, len(fake.comments[2]))
        self.assertEqual([], fake.comments[3])
        self.assertEqual(1, len(fake.comments[4]))
        self.assertEqual([first["id"], second["id"]], [item["comment_id"] for item in results[:2]])
        supersede = [path for path, payload, _ in fake.writes if payload["body"].startswith(SUPERSEDED)]
        self.assertEqual([f"repos/{REPO}/issues/comments/{first['id'] + 2}"], supersede)

    def test_issue_edit_during_overlap_ends_on_the_current_version(self):
        fake = StatefulGitHub({1: {"title": "[task][repo] Verify reminder", "body": "", "state": "open"}})
        single = []
        def edit_then_check():
            fake.issues[1]["body"] = valid_body()
            single.append(check_remote(REPO, 1, fake.call))
        # The full run validated the old version and is about to create; meanwhile the issue
        # is edited and the edit's own run creates the reminder for the new version.
        run_inside(fake, lambda path, method: method == "POST", edit_then_check)
        results, failed = check_all(REPO, fake.call)
        self.assertEqual(0, failed)
        self.assertEqual(("passed", "created"), (single[0]["result"], single[0]["action"]))
        self.assertEqual("passed", results[0]["result"])
        self.assert_one_reminder(fake, 1, comment_body(fake.issues[1], []))
        self.assertNotIn("Please complete", fake.canonical(1)[0]["body"])


if __name__ == "__main__":
    unittest.main()
