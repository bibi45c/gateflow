"""Keep rule documents linked: relative links, heading anchors and skill routing."""
from pathlib import Path
import re
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
SKIP = {".git", ".venv", ".local", "worktrees", "node_modules"}
LINK = re.compile(r"\[[^\]]*\]\(([^)\s]+)\)")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")
SCHEME = re.compile(r"^[a-z][a-z0-9+.-]*:")


def documents(root):
    for path in sorted(root.rglob("*.md")):
        if not SKIP.intersection(path.relative_to(root).parts):
            yield path


def prose_lines(path):
    """Yield lines outside fenced code blocks."""
    fence = None
    for line in path.read_text(encoding="utf-8-sig").splitlines():
        opening = FENCE.match(line)
        if fence:
            if opening and opening[1][0] == fence[0] and len(opening[1]) >= len(fence):
                fence = None
        elif opening:
            fence = opening[1]
        else:
            yield line


def anchors(path):
    """Heading anchors as GitHub generates them, ignoring duplicate suffixes."""
    result = set()
    for line in prose_lines(path):
        heading = re.match(r"^#{1,6} (.+?)\s*$", line)
        if heading:
            text = heading[1].replace("`", "").strip().lower()
            result.add(re.sub(r"[^\w\- ]", "", text).replace(" ", "-"))
    return result


def link_problems(root):
    root = root.resolve()
    problems = []
    for path in documents(root):
        for line in prose_lines(path):
            for target in LINK.findall(re.sub(r"`[^`]*`", "", line)):
                if SCHEME.match(target):
                    continue
                file_part, _, anchor = target.partition("#")
                linked = (path.parent / file_part).resolve() if file_part else path
                where = f"{path.relative_to(root).as_posix()}: {target}"
                if linked != root and root not in linked.parents:
                    problems.append(f"{where} (outside the repository)")
                elif not linked.exists():
                    problems.append(f"{where} (missing file)")
                elif anchor and linked.suffix == ".md" and anchor not in anchors(linked):
                    problems.append(f"{where} (missing heading)")
    return problems


class DocumentLinkTests(unittest.TestCase):
    def test_repository_links_and_anchors_resolve(self):
        self.assertEqual([], link_problems(ROOT))

    def test_broken_links_are_reported_and_code_is_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "b.md").write_text("# Real heading\n", encoding="utf-8")
            (root / "a.md").write_text(
                "[ok](b.md#real-heading) [web](https://example.com)\n"
                "[gone](missing.md) [anchor](b.md#no-such-heading) [up](../x.md)\n"
                "`[code](ignored.md)`\n```text\n[fenced](ignored.md)\n```\n",
                encoding="utf-8")
            self.assertEqual(["a.md: missing.md (missing file)",
                              "a.md: b.md#no-such-heading (missing heading)",
                              "a.md: ../x.md (outside the repository)"], link_problems(root))


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
