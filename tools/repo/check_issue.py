"""Advisory issue format check; canonical fields come from the issue forms.

Uses the repository forms' simple YAML layout, without a third-party YAML dependency.
Unsupported field layouts fail visibly rather than silently skipping requirements.
Issue areas are the repository's area:<name> labels.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
REPO = "bibi45c/gateflow"
FORMS = ROOT / ".github" / "ISSUE_TEMPLATE"
MARKER = "<!-- gateflow-issue-format:v1 -->"
BOT = "github-actions[bot]"
TITLE = re.compile(r"^\[([a-z]+)\]\[([a-z][a-z0-9-]*)\] (\S.*)$")
EMPTY = {"", "no response", "todo", "tbd", "fill this in", "fill here"}
AREA_LABEL = "area:"
AREA = re.compile(r"[a-z][a-z0-9-]*")
PHASE_FIELD = "Acceptance phase"
PHASE = re.compile(r"pre-merge\.?|post-merge\b.*\S.*", re.S)


def scalar(value):
    value = value.strip()
    if not value or value[0] in "|>" or " #" in value:
        raise ValueError("Unsupported scalar in issue form")
    if value.startswith('"'):
        return json.loads(value)
    if value.startswith("'") and value.endswith("'"):
        return value[1:-1].replace("''", "'")
    return value


def requirements(path):
    """Read required input/textarea labels from the existing two-space form layout."""
    text = path.read_text(encoding="utf-8-sig")
    if not re.search(r"^body:\s*$", text, re.M):
        raise ValueError(f"Unsupported form body: {path.name}")
    body = re.split(r"^body:\s*$", text, maxsplit=1, flags=re.M)[1]
    parts = re.split(r"^  - type: ([a-z]+)\s*$", body, flags=re.M)
    fields = {}
    if parts[0].strip() or len(parts) == 1:
        raise ValueError(f"Unsupported form layout: {path.name}")
    for index in range(1, len(parts), 2):
        kind, block = parts[index:index + 2]
        if kind == "markdown":
            continue
        if kind not in {"input", "textarea"}:
            raise ValueError(f"Unsupported form field: {kind}")
        label = re.search(r"^      label: (.+)$", block, re.M)
        if not label:
            raise ValueError(f"Missing form label: {path.name}")
        name = scalar(label[1])
        if name in fields:
            raise ValueError(f"Duplicate form label: {name}")
        required = re.search(r"^    validations:\s*\n      required: (true|false)\s*$", block, re.M)
        if "validations:" in block and not required:
            raise ValueError(f"Unsupported form validation: {name}")
        if required and required[1] == "true":
            description = re.search(r"^      description: (.+)$", block, re.M)
            fields[name] = bool(description and re.search(r"\bor None\b", scalar(description[1])))
    if not fields:
        raise ValueError(f"No required fields: {path.name}")
    return fields


def sections(body):
    result, duplicates = {}, set()
    current, fence, in_comment = None, None, False
    for line in (body or "").splitlines():
        if fence:
            if current:
                result[current].append(line)
            if re.fullmatch(r" {0,3}" + re.escape(fence[0]) + "{" + str(len(fence)) + r",}\s*", line):
                fence = None
            continue
        # HTML comments are hidden outside code fences; inside fences they are data.
        visible = []
        while line:
            if in_comment:
                end = line.find("-->")
                if end < 0:
                    break
                line, in_comment = line[end + 3:], False
            else:
                start = line.find("<!--")
                if start < 0:
                    visible.append(line)
                    break
                visible.append(line[:start])
                line, in_comment = line[start + 4:], True
        line = "".join(visible)
        opening = re.match(r"^ {0,3}(`{3,}|~{3,})", line)
        if opening:
            fence = opening[1]
        heading = None if fence is not None or opening else re.match(r"^#{2,3} (.+?)\s*$", line)
        if heading:
            current = heading[1]
            if current in result:
                duplicates.add(current)
            result.setdefault(current, [])
        elif current:
            result[current].append(line)
    return {key: "\n".join(value).strip() for key, value in result.items()}, duplicates


def issue_areas(labels):
    """Return the areas named by area:<name> labels; other labels are ignored."""
    areas = {name[len(AREA_LABEL):] for name in labels if name.startswith(AREA_LABEL)}
    areas = {area for area in areas if AREA.fullmatch(area)}
    if not areas:
        raise ValueError(f"The repository has no {AREA_LABEL}<name> label")
    return areas


def validate(title, body, areas, forms=FORMS):
    match = TITLE.fullmatch(title)
    if not match or not (forms / f"{match[1]}.yml").is_file() or match[1] == "config":
        return ["Title: Use [task|bug|design][area] followed by a concrete outcome."]
    fields, duplicates = sections(body)
    errors = []
    if match[2] not in areas:
        errors.append(f"Title: Use an area that has an {AREA_LABEL} label: " + ", ".join(sorted(areas)) + ".")
    for name, allows_none in requirements(forms / f"{match[1]}.yml").items():
        if name in duplicates:
            errors.append(f"{name}: Keep one section with this name.")
        elif name not in fields:
            errors.append(f"{name}: Required section is missing.")
        else:
            value = fields[name].strip().strip("*_<>[]").strip().casefold()
            if value in EMPTY:
                errors.append(f"{name}: Replace empty content or placeholder text.")
            elif value in {"none", "n/a", "not applicable"} and not allows_none:
                errors.append(f"{name}: This required field does not allow an empty alternative.")
            elif name == PHASE_FIELD and not PHASE.fullmatch(value):
                errors.append(f"{name}: Write pre-merge, or post-merge plus what is checked after merge and by whom.")
    return errors


def comment_body(issue, errors):
    version = hashlib.sha256(json.dumps([issue["title"], issue.get("body")], ensure_ascii=False).encode()).hexdigest()[:12]
    lines = [MARKER, "## Issue format check", ""]
    if errors:
        lines += ["Please complete:", ""] + [f"- {item}" for item in errors]
    else:
        lines += ["Format check passed."]
    lines += ["", "This checks format only; it does not approve scope or mark the issue Ready.",
              f"<!-- content:{version} -->"]
    return "\n".join(lines)


def api(path, payload=None, method=None):
    headers = {"Accept": "application/vnd.github+json", "Content-Type": "application/json",
               "User-Agent": "gateflow-issue-format"}
    if os.environ.get("GH_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GH_TOKEN"]
    request = urllib.request.Request("https://api.github.com/" + path,
        data=json.dumps(payload).encode() if payload is not None else None,
        method=method, headers=headers)
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def every_page(call, path):
    page = 1
    while True:
        items = call(f"{path}{'&' if '?' in path else '?'}per_page=100&page={page}")
        yield from items
        if len(items) < 100:
            return
        page += 1


def repo_areas(repo, call=api):
    return issue_areas(label["name"] for label in every_page(call, f"repos/{repo}/labels"))


def check_remote(repo, number, call=api, forms=FORMS, areas=None):
    path = f"repos/{repo}/issues/{number}"
    issue = call(path)
    if "pull_request" in issue or issue["state"] != "open":
        return {"issue": number, "action": "skipped"}
    errors = validate(issue["title"], issue.get("body"), areas or repo_areas(repo, call), forms)
    body = comment_body(issue, errors)
    existing = next((item for item in every_page(call, f"{path}/comments")
                     if item["user"]["login"] == BOT and item["user"]["type"] == "Bot"
                     and item.get("body", "").startswith(MARKER)), None)
    current = call(path)
    if (current["title"], current.get("body"), current["state"]) != (issue["title"], issue.get("body"), issue["state"]):
        raise RuntimeError("Issue changed during checking; retry the current version")
    if existing and existing["body"] == body:
        action, comment = "unchanged", existing
    elif existing:
        action = "updated"
        comment = call(f"repos/{repo}/issues/comments/{existing['id']}", {"body": body}, "PATCH")
    else:
        action = "created"
        comment = call(f"{path}/comments", {"body": body}, "POST")
    return {"issue": number, "result": "needs-info" if errors else "passed",
            "action": action, "comment_id": comment["id"], "missing": len(errors)}


def check_all(repo, call=api, forms=FORMS):
    """Recheck every open issue, for example after the area labels change."""
    areas = repo_areas(repo, call)
    results, failed = [], 0
    for issue in list(every_page(call, f"repos/{repo}/issues?state=open")):
        if "pull_request" in issue:
            continue
        try:
            results.append(check_remote(repo, issue["number"], call, forms, areas))
        except RuntimeError as error:
            failed += 1
            results.append({"issue": issue["number"], "action": "failed", "error": str(error)})
    return results, failed


def changes_areas(event):
    names = [event["label"]["name"], event.get("changes", {}).get("name", {}).get("from", "")]
    return any(name.startswith(AREA_LABEL) for name in names)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--title")
    parser.add_argument("--body-file", type=Path)
    parser.add_argument("--repo", default=REPO, help="repository whose area labels apply")
    parser.add_argument("--areas", help="comma-separated areas to use instead of the labels")
    args = parser.parse_args()
    if args.title is not None:
        if not args.body_file:
            parser.error("--title requires --body-file")
        try:
            body = args.body_file.read_text(encoding="utf-8-sig")
        except UnicodeDecodeError:
            sys.exit(f"{args.body_file} is not UTF-8; save it as UTF-8 and retry.")
        if args.areas is not None:
            areas = issue_areas(AREA_LABEL + area.strip() for area in args.areas.split(","))
        else:
            try:
                areas = repo_areas(args.repo)
            except OSError as error:
                sys.exit(f"Cannot read the {AREA_LABEL} labels of {args.repo} ({error}); "
                         "retry, set GH_TOKEN, or pass --areas.")
        errors = validate(args.title, body, areas)
        print(json.dumps({"result": "needs-info" if errors else "passed", "errors": errors}))
        sys.exit(1 if errors else 0)
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
    name, repo = os.environ.get("GITHUB_EVENT_NAME"), event["repository"]["full_name"]
    if name == "issues" and event.get("action") in {"opened", "edited"}:
        print(json.dumps(check_remote(repo, int(event["issue"]["number"]))))
    elif name == "label" and not changes_areas(event):
        print(json.dumps({"action": "skipped", "reason": "not an area label"}))
    elif name in {"label", "workflow_dispatch"}:
        results, failed = check_all(repo)
        for result in results:
            print(json.dumps(result))
        sys.exit(1 if failed else 0)
    else:
        raise ValueError(f"Unexpected event: {name} {event.get('action')}")


if __name__ == "__main__":
    main()
