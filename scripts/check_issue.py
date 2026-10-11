"""Advisory issue format check; canonical fields come from the issue forms.

Uses the repository forms' simple YAML layout, without a third-party YAML dependency.
Unsupported field layouts fail visibly rather than silently skipping requirements.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
FORMS = ROOT / ".github" / "ISSUE_TEMPLATE"
MARKER = "<!-- gateflow-issue-format:v1 -->"
BOT = "github-actions[bot]"
TITLE = re.compile(r"^\[([a-z]+)\]\[([a-z][a-z0-9-]*)\] (\S.*)$")
EMPTY = {"", "no response", "todo", "tbd", "fill this in", "fill here"}
AREA_MARKER = "<!-- issue-areas -->"
AREAS = re.compile(re.escape(AREA_MARKER) + r"\s*`([^`]+)`")


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


def issue_areas(contract):
    """Read the one area list that follows the issue-areas marker in CONTRIBUTING.md."""
    found = AREAS.findall(contract)
    if len(found) != 1:
        raise ValueError(f"CONTRIBUTING.md must contain exactly one {AREA_MARKER} list")
    areas = {area.strip() for area in found[0].split(",") if area.strip()}
    if not all(re.fullmatch(r"[a-z][a-z0-9-]*", area) for area in areas):
        raise ValueError("Issue areas must be lowercase words separated by commas")
    return areas


def validate(title, body, forms=FORMS):
    match = TITLE.fullmatch(title)
    if not match or not (forms / f"{match[1]}.yml").is_file() or match[1] == "config":
        return ["Title: Use [task|bug|design][area] followed by a concrete outcome."]
    areas = issue_areas((ROOT / "CONTRIBUTING.md").read_text(encoding="utf-8-sig"))
    fields, duplicates = sections(body)
    errors = []
    if match[2] not in areas:
        errors.append("Title: Use an area listed in CONTRIBUTING.md: " + ", ".join(sorted(areas)) + ".")
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
    request = urllib.request.Request("https://api.github.com/" + path,
        data=json.dumps(payload).encode() if payload is not None else None,
        method=method, headers={"Authorization": "Bearer " + os.environ["GH_TOKEN"],
        "Accept": "application/vnd.github+json", "Content-Type": "application/json",
        "User-Agent": "gateflow-issue-format"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def check_remote(repo, number, call=api, forms=FORMS):
    path = f"repos/{repo}/issues/{number}"
    issue = call(path)
    if "pull_request" in issue or issue["state"] != "open":
        return {"issue": number, "action": "skipped"}
    errors = validate(issue["title"], issue.get("body"), forms)
    body = comment_body(issue, errors)
    existing = None
    page = 1
    while True:
        comments = call(f"{path}/comments?per_page=100&page={page}")
        for item in comments:
            if (item["user"]["login"] == BOT and item["user"]["type"] == "Bot"
                    and item.get("body", "").startswith(MARKER)):
                existing = item
                break
        if existing or len(comments) < 100:
            break
        page += 1
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--title")
    parser.add_argument("--body-file", type=Path)
    args = parser.parse_args()
    if args.title is not None:
        if not args.body_file:
            parser.error("--title requires --body-file")
        errors = validate(args.title, args.body_file.read_text(encoding="utf-8-sig"))
        print(json.dumps({"result": "needs-info" if errors else "passed", "errors": errors}))
        return
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
    if event.get("action") not in {"opened", "edited"} or "issue" not in event:
        raise ValueError("Expected an opened or edited issue event")
    print(json.dumps(check_remote(event["repository"]["full_name"], int(event["issue"]["number"]))))


if __name__ == "__main__":
    main()
