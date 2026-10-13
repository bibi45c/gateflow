"""Keep the error codes of api/errors.md and the OpenAPI contracts in step."""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parents[3]
API = ROOT / "api"
SERVICES = ("access", "metering")
# The internal codes of issue #18 revision 3.2, per service that returns them.
AGREED = {
    "access": {"invalid_request", "unauthorized", "invalid_key", "model_not_allowed",
               "price_version_not_found", "internal_error", "dependency_unavailable"},
    "metering": {"invalid_request", "unauthorized", "wallet_not_found", "reservation_not_found",
                 "insufficient_balance", "idempotency_conflict", "internal_error",
                 "dependency_unavailable"},
}
INTERNAL_ROW = re.compile(r"^\|[ \t]*(access|metering)[ \t]*\|[ \t]*(\d{3})[ \t]*\|[ \t]*`([a-z_]+)`[ \t]*\|",
                          re.M)
EXAMPLE_CODE = re.compile(r"^[ \t]*code:[ \t]*([a-z_]+)[ \t]*$", re.M)
ENUM_ITEM = re.compile(r"[ \t]*-[ \t]*([a-z_]+)[ \t]*")


def read(path):
    return path.read_text(encoding="utf-8")


def internal_rows(text):
    """(service, status, code) for each row of the internal error table."""
    return INTERNAL_ROW.findall(text)


def internal_codes(text):
    codes = {service: set() for service in SERVICES}
    for service, _, code in internal_rows(text):
        codes[service].add(code)
    return codes


def error_enum(text):
    """The codes under enum: in the ErrorCode schema, or None when there is none."""
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if line.strip() != "ErrorCode:":
            continue
        indent = len(line) - len(line.lstrip())
        codes, listing = [], False
        for inner in lines[index + 1:]:
            if not inner.strip():
                continue
            if len(inner) - len(inner.lstrip()) <= indent:
                break
            item = ENUM_ITEM.fullmatch(inner)
            if inner.strip() == "enum:":
                listing = True
            elif listing and item:
                codes.append(item[1])
            else:
                listing = False
        return codes
    return None


class ApiContractTests(unittest.TestCase):
    def test_contracts_are_openapi_3_1(self):
        for service in SERVICES:
            with self.subTest(service=service):
                self.assertTrue(read(API / f"{service}.yaml").startswith("openapi: 3.1"))

    def test_errors_md_lists_the_agreed_codes(self):
        self.assertEqual(AGREED, internal_codes(read(API / "errors.md")))

    def test_each_enum_matches_errors_md(self):
        errors = read(API / "errors.md")
        documented, rows = internal_codes(errors), internal_rows(errors)
        for service in SERVICES:
            with self.subTest(service=service):
                text = read(API / f"{service}.yaml")
                enum = error_enum(text)
                self.assertIsNotNone(enum, "no ErrorCode enum")
                self.assertEqual(len(enum), len(set(enum)), "a code is repeated in the enum")
                self.assertEqual(documented[service], set(enum))
                for _, status, code in (row for row in rows if row[0] == service):
                    self.assertRegex(text, rf"(?m)^[ \t]*'{status}':", f"no {status} response for {code}")

    def test_examples_use_only_enum_codes_and_show_each_one(self):
        for service in SERVICES:
            with self.subTest(service=service):
                text = read(API / f"{service}.yaml")
                self.assertEqual(set(error_enum(text) or ()), set(EXAMPLE_CODE.findall(text)))

    def test_parsers_on_synthetic_text(self):
        markdown = ("| Service | Status | Code | Returned when |\n|---|---|---|---|\n"
                    "| access | 401 | `invalid_key` | Unknown key. |\n"
                    "| metering | 409 | `insufficient_balance` | Too little. |\n\n"
                    "| Internal | Public status | Public type |\n|---|---|---|\n"
                    "| access `model_not_allowed` | 403 | `permission_error` |\n")
        self.assertEqual([("access", "401", "invalid_key"), ("metering", "409", "insufficient_balance")],
                         internal_rows(markdown))
        yaml = ("    Error:\n      properties:\n        code:\n"
                "          $ref: '#/components/schemas/ErrorCode'\n"
                "    ErrorCode:\n      type: string\n      enum:\n        - invalid_key\n\n"
                "        - unauthorized\n      description: Codes.\n"
                "    Other:\n      enum:\n        - not_an_error_code\n")
        self.assertEqual(["invalid_key", "unauthorized"], error_enum(yaml))
        self.assertIsNone(error_enum("openapi: 3.1.0\n"))
        self.assertEqual(["invalid_key"], EXAMPLE_CODE.findall("  code: invalid_key\n  code:\n"))


if __name__ == "__main__":
    unittest.main()
