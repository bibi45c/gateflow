# GateFlow

GateFlow team project. The application scope and stack are still being agreed.

## Start here

- People: [CONTRIBUTING.md](CONTRIBUTING.md).
- Coding agents: [AGENTS.md](AGENTS.md). Claude Code loads it through
  [CLAUDE.md](CLAUDE.md).
- Checks and evidence: [docs/testing.md](docs/testing.md).

## Setup on macOS and Windows

1. Install Git, Python 3.13 or newer, and the GitHub CLI (`gh`).
2. On Windows, enable long paths:

   ```text
   git config --global core.longpaths true
   ```

3. Clone into a folder that OneDrive, iCloud Drive or Dropbox does not sync:

   ```text
   git clone https://github.com/bibi45c/gateflow.git
   ```

4. Run the tests from the repository root (on macOS, `python3` if `python` is not
   Python 3):

   ```text
   python -m unittest discover -s tests -v
   ```
