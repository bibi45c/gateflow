"""Keep rule documents linked: relative links, heading anchors and skill routing."""
import html
import os
from pathlib import Path
import posixpath
import re
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock
from urllib.parse import unquote

ROOT = Path(__file__).resolve().parents[3]
SKIP = {".git", ".venv", ".local", "worktrees", "node_modules"}
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")
FENCE_CLOSE = re.compile(r"^ {0,3}(`{3,}|~{3,})[ \t]*$")
TARGET = r"(<[^>]*>|(?:[^\s()]|\([^\s()]*\))+)"
TITLE = r"(?:\s+(?:\"[^\"]*\"|'[^']*'|\([^)]*\)))?"
LINK = re.compile(r"\[(?:[^\[\]]|\[[^\]]*\])*\]\(\s*" + TARGET + TITLE + r"\s*\)")
REFERENCE = re.compile(r"^ {0,3}\[(?!\^)[^\]]+\]:\s*(<[^>]*>|\S+)", re.M)
HEADING = re.compile(r"^ {0,3}(?:> ?)*#{1,6}[ \t]+(.+?)(?:[ \t]+#+)?[ \t]*$", re.M)
SETEXT = re.compile(r"^ {0,3}(?:> ?)*(\S.*?)[ \t]*\n {0,3}(?:> ?)*(?:=+|-+)[ \t]*$", re.M)
HTML_ANCHOR = re.compile(r"<a\s[^>]*\b(?:name|id)=\"([^\"]+)\"")
SCHEME = re.compile(r"^[a-z][a-z0-9+.-]+:", re.I)


def repository_files(root):
    """Paths Git would commit (tracked or untracked, not ignored).

    Returns None only outside a Git repository or without Git installed. Inside a
    repository a Git failure is an error, because silently falling back would lose the
    case-exact check on Windows and macOS.
    """
    if not any((folder / ".git").exists() for folder in (root, *root.parents)):
        return None
    try:
        result = subprocess.run(["git", "-C", str(root), "ls-files", "-z", "--cached",
                                 "--others", "--exclude-standard"], capture_output=True)
    except FileNotFoundError:
        return None
    if result.returncode != 0:
        raise RuntimeError("git ls-files failed: " + result.stderr.decode("utf-8", "replace").strip())
    return {name for name in result.stdout.decode("utf-8").split("\0")
            if name and (root / name).is_file()}


def documents(root, files):
    if files is None:
        return sorted(path.relative_to(root).as_posix() for path in root.rglob("*.md")
                      if not SKIP.intersection(path.relative_to(root).parts))
    return sorted(name for name in files if name.endswith(".md"))


def exists(relative, root, files):
    """Case-exact when Git is available, so Windows and macOS agree with Linux CI."""
    if files is None:
        return (root / relative).exists()
    return (relative == "." or relative in files
            or any(name.startswith(relative + "/") for name in files))


def prose(path):
    """Document text without fenced code blocks or HTML comments."""
    lines, fence = [], None
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        if fence:
            closing = FENCE_CLOSE.match(line)
            if closing and closing[1][0] == fence[0] and len(closing[1]) >= len(fence):
                fence = None
            lines.append("")
        elif opening := FENCE.match(line):
            fence = opening[1]
            lines.append("")
        else:
            lines.append(line)
    text = re.sub(r"`[^`\n]*<!--[^`\n]*`", "", "\n".join(lines))
    return re.sub(r"<!--.*?-->", "", text, flags=re.S)


def slug(heading):
    text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", heading)
    text = html.unescape(text.replace("`", "")).strip().lower()
    return re.sub(r"[^\w\- ]", "", text).replace(" ", "-")


def anchors(path):
    """Heading anchors as GitHub generates them, including -1 style duplicates."""
    text = prose(path)
    result, seen = set(HTML_ANCHOR.findall(text)), {}
    for heading in HEADING.findall(text) + SETEXT.findall(text):
        base = slug(heading)
        count = seen.get(base, 0)
        seen[base] = count + 1
        result.add(base if count == 0 else f"{base}-{count}")
    return result


def targets(text):
    """Link targets per paragraph, so link text may wrap across lines."""
    for paragraph in re.split(r"\n[ \t]*\n", text):
        yield from LINK.findall(re.sub(r"`[^`]*`", "", " ".join(paragraph.splitlines())))
    yield from REFERENCE.findall(text)


def link_problems(root):
    root = root.resolve()
    files = repository_files(root)
    problems, cache = [], {}
    for document in documents(root, files):
        for raw in targets(prose(root / document)):
            target = raw[1:-1] if raw.startswith("<") else raw
            if SCHEME.match(target):
                continue
            where = f"{document}: {raw}"
            if "\\" in target:
                problems.append(f"{where} (use forward slashes)")
                continue
            file_part, _, anchor = unquote(target).partition("#")
            file_part = file_part.split("?", 1)[0]
            if file_part:
                base = "" if file_part.startswith("/") else posixpath.dirname(document)
                relative = posixpath.normpath(posixpath.join(base, file_part.lstrip("/")))
            else:
                relative = document
            if relative == ".." or relative.startswith("../"):
                problems.append(f"{where} (outside the repository)")
            elif not exists(relative, root, files):
                problems.append(f"{where} (missing file)")
            elif anchor and relative.endswith(".md"):
                if relative not in cache:
                    cache[relative] = anchors(root / relative)
                if anchor not in cache[relative]:
                    problems.append(f"{where} (missing heading)")
    return problems


class DocumentLinkTests(unittest.TestCase):
    def test_repository_links_and_anchors_resolve(self):
        self.assertEqual([], link_problems(ROOT))

    def test_link_forms_and_anchor_rules(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "b.md").write_text(
                "# Real heading ##\n\n## FAQ\n\n## FAQ\n\n## The [spec](b.md) rules\n"
                "\n## Q&amp;A\n\n<a name=\"custom\"></a>\n", encoding="utf-8")
            (root / "a.md").write_text(
                "[ok](b.md#real-heading) [dup](b.md#faq-1) [linked](b.md#the-spec-rules)\n"
                "[entity](b.md#qa) [html](b.md#custom) [web](https://example.com)\n"
                "[titled](b.md \"Title\") [root](/b.md) [self](#local)\n\n## Local\n\n"
                "See the [wrapped\nlink](wrapped-gone.md) here.\n\n"
                "[gone](missing.md) [angle](<gone file.md>) [anchor](b.md#no-such)\n"
                "[up](../x.md) [slash](docs\\b.md) [drive](c:/nope.md) [ref][r]\n\n"
                "[r]: ref-gone.md\n\n"
                "`[code](ignored.md)` <!-- [comment](ignored.md) -->\n"
                "```text\n[fenced](ignored.md)\n```\n", encoding="utf-8")
            self.assertCountEqual([
                "a.md: wrapped-gone.md (missing file)",
                "a.md: missing.md (missing file)",
                "a.md: <gone file.md> (missing file)",
                "a.md: b.md#no-such (missing heading)",
                "a.md: ../x.md (outside the repository)",
                "a.md: docs\\b.md (use forward slashes)",
                "a.md: c:/nope.md (missing file)",
                "a.md: ref-gone.md (missing file)",
            ], link_problems(root))

    def test_titles_setext_quotes_footnotes_and_fences(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "f_(1).md").write_text("Intro\n=====\n\n> ## Note\n", encoding="utf-8")
            (root / "a.md").write_text(
                "[setext](f_(1).md#intro) [quoted](f_(1).md#note) [query](f_(1).md?plain=1)\n"
                "[sq](sq-gone.md 'T') [paren](paren-gone.md (T))\n\n"
                "Type `<!--` to start a comment. [after](after-gone.md)\n\n"
                "Text with a footnote.[^1]\n\n[^1]: Some note\n\n"
                "````markdown\n```text\n[inner](ignored.md)\n````\n", encoding="utf-8")
            self.assertCountEqual([
                "a.md: sq-gone.md (missing file)",
                "a.md: paren-gone.md (missing file)",
                "a.md: after-gone.md (missing file)",
            ], link_problems(root))

    @unittest.skipUnless(shutil.which("git"), "Git is not available")
    def test_case_mismatch_and_ignored_files_follow_git(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            (root / ".gitignore").write_text("private.md\n", encoding="utf-8")
            (root / "b.md").write_text("# B\n", encoding="utf-8")
            (root / "a.md").write_text("[case](B.md)\n", encoding="utf-8")
            (root / "private.md").write_text("[mine](../outside.md)\n", encoding="utf-8")
            self.assertEqual(["a.md: B.md (missing file)"], link_problems(root))

    @unittest.skipUnless(shutil.which("git"), "Git is not available")
    def test_git_failure_inside_a_repository_is_an_error(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q", str(root)], check=True)
            (root / "a.md").write_text("# A\n", encoding="utf-8")
            with mock.patch.dict(os.environ, {"GIT_DIR": str(root / "no-such-dir")}):
                with self.assertRaises(RuntimeError):
                    link_problems(root)


class SkillRoutingTests(unittest.TestCase):
    def test_every_skill_is_routed_and_described(self):
        routes = set(re.findall(r"\(\.agents/skills/([a-z0-9-]+)/SKILL\.md\)",
                                (ROOT / "AGENTS.md").read_text(encoding="utf-8-sig")))
        skills = {path.parent.name: path for path in (ROOT / ".agents" / "skills").glob("*/SKILL.md")}
        self.assertEqual(set(skills), routes)
        for name, path in skills.items():
            with self.subTest(skill=name):
                header = re.match(r"^---\nname: (.+)\ndescription: (.+)\n---\n",
                                  path.read_text(encoding="utf-8-sig"))
                self.assertIsNotNone(header)
                self.assertEqual(name, header[1])
                self.assertTrue(header[2].startswith("Use when "))


if __name__ == "__main__":
    unittest.main()
