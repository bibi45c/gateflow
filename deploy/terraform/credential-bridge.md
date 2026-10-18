# Terraform credential bridge (bibi45c only)

On the cloud days Terraform runs in the `hashicorp/terraform:1.14.4` container, the
tested version (see [README.md](README.md#local-checks-no-aws-credentials)). The
operator signs in with the AWS CLI on the host, and those sign-in credentials last
about 15 minutes, while an apply or destroy of the environment takes longer. No
static access key is used and no `AWS_*` variable is passed to the container. The
bridge lets the AWS SDK inside the container fetch fresh credentials itself, through
`credential_process`, from [deploy/aws/credserve.py](../aws/credserve.py) on the host.

## How it works

- `credserve.py` writes a new random token to a file that git ignores, listens on
  `127.0.0.1` only, and answers `GET /<token>` with the output of
  `aws configure export-credentials --format process`. Any other path gets 404, a
  failed export 500.
- The container's AWS config, mounted read-only at `/root/.aws`, runs
  `wget -qO- http://host.docker.internal:<port>/<token>` as its `credential_process`.
  The SDK runs it again whenever the credentials it holds expire.
- `credserve.log` gets one line per request: time, outcome and the credentials'
  expiry. It never holds a credential value, the token or the request path.

Docker Desktop forwards `host.docker.internal` to the host's loopback interface, so
the loopback-only listener is reachable from the container. A Linux Docker Engine
does not do that; the bridge is not set up for it.

Anyone on the host who can read the token can fetch credentials while
`credserve.py` runs. Keep the token and the config under `.local/`, and stop
`credserve.py` with Ctrl-C when the session ends.

## Use

From the repository root in Git Bash, after signing in with the AWS CLI and
unsetting any static `AWS_*` variables:

1. Start the bridge in a terminal of its own and keep it running:

   ```text
   python deploy/aws/credserve.py --token-file .local/cloud-day/credserve.token
   ```

   `--port` (default 17651), `--log` (default `credserve.log` next to the token file)
   and `--profile` (default: the CLI default) are optional. It exits with 2 when the
   token file is inside the repository and not ignored by git, or when it cannot
   write the file or listen, and with 0 after Ctrl-C.

2. Write the container's AWS config. Every start writes a new token, so write the
   config again after each start:

   ```bash
   mkdir -p .local/cloud-day/tfaws
   printf '[default]\nregion = %s\ncredential_process = wget -qO- http://host.docker.internal:%s/%s\n' \
     ap-southeast-1 17651 "$(cat .local/cloud-day/credserve.token)" > .local/cloud-day/tfaws/config
   ```

3. Run Terraform through a function; the repository path has spaces, so a function
   rather than a string. Outside Git Bash, leave out `cygpath -w` and
   `MSYS_NO_PATHCONV=1`.

   ```bash
   tf() { MSYS_NO_PATHCONV=1 docker run --rm -v "$(cygpath -w "$PWD/.local/cloud-day/tfaws"):/root/.aws:ro" \
     -v "$(cygpath -w "$PWD/deploy/terraform"):/tf" -w /tf hashicorp/terraform:1.14.4 "$@"; }
   tf init -reconfigure -backend-config=backend.hcl
   ```

4. Check, after `init`, that no static key is set and that a plan works through the
   bridge; the last log line must be a new `served, expires ...`:

   ```bash
   env | grep -qE '^AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)=' && echo 'unset the AWS_* variables first'
   tf plan -lock=false -input=false -no-color > /dev/null && tail -1 .local/cloud-day/credserve.log
   ```

## Acceptance

[bridge-test/main.tf](bridge-test/main.tf) reads the caller identity, waits 17
minutes in a `time_sleep`, past the lifetime of the sign-in credentials, and reads it
again, all in one `terraform apply`. It creates no AWS resource and keeps its state
local. [bridge-test/run.sh](bridge-test/run.sh) copies it to a work folder, runs
`init` and `apply` in the same container as above, and writes `acceptance.txt`:

```text
deploy/terraform/bridge-test/run.sh [work folder] [AWS config folder] [credserve log]
```

The defaults are `.local/cloud-day/bridge-test`, `.local/cloud-day/tfaws` and
`.local/cloud-day/credserve.log`, and `TERRAFORM_IMAGE` overrides the image. The
bridge passes when all three hold, and the script then exits with 0:

- the apply exits with 0;
- the output is `same_account_after_expiry = true`;
- `credserve.log` gained at least two `served` lines during the test, so the SDK
  fetched credentials again after the wait.

`acceptance.txt` lists the start and end times, the apply exit code, the output, the
log lines of the test and `result PASS` or `result FAIL`. The work folder's
`init.log` and `apply.log` name the account, so the folder stays under `.local/`.
Run the acceptance again after a change to `credserve.py`, the config format, the
Terraform image or the AWS CLI.
