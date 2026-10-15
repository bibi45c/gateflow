# Service images

[The image workflow](../../.github/workflows/images.yml) builds gateway, access,
metering and mock-upstream with the existing Dockerfile. PR jobs have read-only
repository permissions, do not log in and never publish. Main pushes and `v*`
tag pushes publish to `ghcr.io/bibi45c/gateflow-<service>` with the full commit SHA.
Version-tag pushes also publish the exact version tag. Only the publishing job
has `packages: write`; it uses the built-in GITHUB_TOKEN, not a personal token.

All images target Linux amd64, matching the planned m6i/c6i EKS nodes. Each image
records OCI source and revision labels. The publishing job summary records the
digest; use `image@sha256:...` in deployments for an immutable reference. A version
tag can be moved, so record the digest even when deploying a named release.

## First publication acceptance

After the first approved merge, bibi45c makes all four packages public in GitHub
package settings. New GHCR packages start private, and making them public is a
one-way change. This workflow does not change package visibility.

KaenBin records the four SHA tags and digests from the workflow summary, then
checks anonymous pulls using an empty Docker config directory, one image at a
time. Create a temporary directory outside the repository for that config:

```text
docker --config <empty-directory> pull ghcr.io/bibi45c/gateflow-gateway:<full-sha>
docker --config <empty-directory> pull ghcr.io/bibi45c/gateflow-access:<full-sha>
docker --config <empty-directory> pull ghcr.io/bibi45c/gateflow-metering:<full-sha>
docker --config <empty-directory> pull ghcr.io/bibi45c/gateflow-mock-upstream:<full-sha>
```

Compare pulled digests with the job summaries. Confirm OCI labels with `docker
image inspect`. Pre-merge acceptance also requires a PR workflow run showing all
four build jobs passed and publishing skipped. Local static checks do not replace
that run. No publishing, deployment or package-visibility changes are performed
by merely editing or testing the local branch.

## Recovery

Restore the previous deployment digest to roll back. If publishing is faulty,
revert the workflow through the repository process; removal of bad registry
versions is a separate authorised remote action. A four-image matrix can partly
succeed, so verify all four summaries before selecting a deployment version.
