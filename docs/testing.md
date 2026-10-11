# Verification

## Checks

| Check | Command, from the repository root | Required for merge |
|---|---|---|
| Repository tests | `python -m unittest discover -s tests -v` | Not yet; CI runs it on Linux, macOS and Windows |

The tests need Python 3.13 or newer and only the standard library. On macOS use
`python3` if `python` is not Python 3; on Windows `py -3.13` also works.

The suite covers repository tooling (the issue format check and documentation links),
not application code. There is no application stack yet: agree its checks before
implementation, and add their exact commands here together with the configuration
that runs them. No configured check means no automated evidence; report it as
"not run", never as "passed".

For each change:

1. Run the checks closest to what you changed.
2. Run every applicable required check.
3. For a rule or template change, also try one realistic case, such as filling in the
   changed issue form. Valid syntax alone proves nothing about behavior.
4. Give tests that write files or use ports or databases their own temporary
   resources, because other worktrees may run tests at the same time.

## Evidence

Report each check with:

- Command or scenario.
- Environment: OS and Python version.
- Commit: head and base SHA, or the candidate.
- Uncommitted inputs, if any.
- Result: passed, failed, not run or blocked. Explain the last three.
- Short evidence, such as the summary line of the output.

Example: `python -m unittest discover -s tests -v`; Windows 11, Python 3.14; head
`d1606e4`, base `c299882`; passed, 17 tests.

Re-run affected checks when the head or base changes, or when you are unsure that the
tested content equals the delivered content. Two branches that pass separately are not
proven to work together.

## Gates

- Required pre-merge checks must pass on the current head against the current base.
  A failed or missing required check blocks the merge.
- Post-merge acceptance, such as a test that needs a deployment, must be agreed in the
  issue. It never replaces a required security, contract or correctness check.
- An issue is Done only when all agreed acceptance is recorded on it.
- Local runs, CI, agent review, human approval, merge and deployment are separate
  evidence; one does not imply another.
- Repository protections are whatever GitHub settings enforce, and this document does
  not enable any. Read them back with
  `gh api repos/bibi45c/gateflow/branches/main/protection`.
