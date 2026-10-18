#!/usr/bin/env bash
# Credential bridge acceptance, see ../credential-bridge.md. Run from the repository root in Git Bash
# or another POSIX shell, with deploy/aws/credserve.py running and the container's AWS config written:
#   deploy/terraform/bridge-test/run.sh [work folder] [AWS config folder] [credserve log]
# Defaults: .local/cloud-day/bridge-test, .local/cloud-day/tfaws and .local/cloud-day/credserve.log.
# The work folder gets main.tf, the provider lock file, the local state and the logs, so it must be
# ignored by git; the logs name the account. TERRAFORM_IMAGE overrides the image.
# Exit 0 when the apply succeeded, the account is the same after the wait, and credserve served at
# least twice during the test; 1 otherwise. acceptance.txt in the work folder records the result.
set -u
work=${1:-.local/cloud-day/bridge-test}
aws_dir=${2:-.local/cloud-day/tfaws}
log=${3:-.local/cloud-day/credserve.log}
image=${TERRAFORM_IMAGE:-hashicorp/terraform:1.14.4}

absolute() { case $1 in /* | ?:*) printf '%s' "$1" ;; *) printf '%s/%s' "$PWD" "$1" ;; esac; }
host_path() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }
terraform() {
  MSYS_NO_PATHCONV=1 docker run --rm -v "$(host_path "$(absolute "$aws_dir")"):/root/.aws:ro" \
    -v "$(host_path "$(absolute "$work")"):/t" -w /t "$image" "$@"
}

mkdir -p "$work"
cp "$(dirname "$0")/main.tf" "$work/main.tf"
lines=0
[ -f "$log" ] && lines=$(wc -l < "$log")
start=$(date -u +%FT%TZ)
terraform init -input=false -no-color > "$work/init.log" 2>&1 || { echo "terraform init failed, see $work/init.log"; exit 1; }
terraform apply -auto-approve -input=false -no-color > "$work/apply.log" 2>&1
rc=$?
end=$(date -u +%FT%TZ)
same=$(grep -o 'same_account_after_expiry = .*' "$work/apply.log" | tail -1)
fetches=0
[ -f "$log" ] && fetches=$(tail -n +$((lines + 1)) "$log" | grep -c ' served, ')
verdict=FAIL
[ "$rc" = 0 ] && [ "$same" = 'same_account_after_expiry = true' ] && [ "$fetches" -ge 2 ] && verdict=PASS
{
  echo "start $start"
  echo "end $end"
  echo "apply exit $rc"
  echo "${same:-no same_account_after_expiry output}"
  echo "credserve fetches during the test: $fetches"
  [ -f "$log" ] && tail -n +$((lines + 1)) "$log"
  echo "result $verdict"
} > "$work/acceptance.txt"
cat "$work/acceptance.txt"
[ "$verdict" = PASS ]
