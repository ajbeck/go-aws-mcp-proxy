---
title: Release process
description: Prepare, review, publish, and recover aws-mcp-proxy releases.
weight: 50
kicker: Maintainers
---
Releases are proposed and published by
[release-please](https://github.com/googleapis/release-please). It owns the
release pull request, `CHANGELOG.md`, `version.txt`, the version manifest, the
`vMAJOR.MINOR.PATCH` tag, and the GitHub Release. The repository workflow keeps
ownership of compiled archives, checksums, SPDX SBOMs, and build attestations.

## Change pull requests

Use a Conventional Commit title for every pull request:

```text
fix: reject unsigned redirects
feat(doctor): report the effective identity
deps: update the AWS SDK
feat!: remove a deprecated option
```

`fix` and `deps` request a patch release, `feat` requests a minor release, and
`!` or a `BREAKING CHANGE` footer requests a major release. Other accepted
types do not request a release on their own. CI validates the title, and
Dependabot is configured to emit compatible `deps:` titles.

Prefer squash merging with the pull request title as the squash commit title.
That gives release-please one reviewed release unit per pull request and keeps
`main` linear. Enable squash merges in the repository and disable rebase merges
after the current stack lands. Until then, every commit that is rebase-merged
must itself use Conventional Commit syntax.

## Publish a release

1. Merge release-worthy changes into `main`.
2. Wait for the `Release` workflow to create or update the single pull request
   labelled `autorelease: pending`.
3. Review its proposed version and generated changelog, and require its normal
   CI checks to pass.
4. Merge the release pull request when that exact collection of changes should
   ship.
5. The next `Release` run verifies the release commit, creates the tag and
   GitHub Release, builds every platform archive, generates checksums and SPDX
   SBOMs, records provenance and SBOM attestations, and uploads the assets.
6. Verify the published assets using the commands in [Installation](/docs/installation/#verify-a-release).

The first automated release is explicitly targeted at `v1.0.0` by the
bootstrap commit's `Release-As: 1.0.0` footer. That commit also carries the
Conventional entries for changes accumulated since `v0.4.0`, replacing the
legacy `Unreleased` section. Later versions are derived from Conventional
Commit titles.

## Release automation token

The workflow can use the repository `GITHUB_TOKEN`, but GitHub suppresses new
workflow runs caused by that token. Configure the same GitHub App pattern used
by Quorra so release-please-created pull requests receive ordinary CI runs:

- install a GitHub App on this repository with Contents, Issues, and Pull
  requests read/write permissions;
- set `RELEASE_PLEASE_CLIENT_ID` as a repository variable;
- set `RELEASE_PLEASE_APP_PRIVATE_KEY` as a repository secret.

When the variable is absent, the workflow falls back to `GITHUB_TOKEN`. The
release workflow still verifies `main` before creating a tag, but the pending
release pull request will not automatically trigger the pull-request workflow.

## Recover release assets

If packaging or upload fails after release-please creates a tag, rerun the
`Release` workflow with `release_tag` set to the existing tag, for example
`v1.0.0`. The workflow verifies that tagged commit, rebuilds all assets, and
uploads them with replacement enabled. Do not create or move tags manually.
